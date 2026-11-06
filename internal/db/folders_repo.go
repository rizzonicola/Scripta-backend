package db

import (
	"context"
	"database/sql"

	"github.com/google/uuid"

	"notes-server/internal/models"
)

// FoldersRepo gestisce la persistenza delle cartelle. La gerarchia è
// interamente ID-based (parent_id punta a folders.id, mai un percorso
// testuale): spostare una cartella è un singolo UPDATE su parent_id.
type FoldersRepo struct {
	db execer
}

func NewFoldersRepo(d execer) *FoldersRepo {
	return &FoldersRepo{db: d}
}

// Get recupera una cartella per (userID, id), inclusi i tombstone.
func (r *FoldersRepo) Get(ctx context.Context, userID, id string) (*models.Folder, error) {
	var f models.Folder
	err := r.db.QueryRowContext(ctx,
		`SELECT id, user_id, name, parent_id, updated_at, synced_at, deleted_at
		 FROM folders WHERE id = ? AND user_id = ?`,
		id, userID,
	).Scan(&f.ID, &f.UserID, &f.Name, &f.ParentID, &f.UpdatedAt, &f.SyncedAt, &f.DeletedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// UpsertLWW inserisce o aggiorna una cartella con la stessa semantica
// Last-Write-Wins di NotesRepo.UpsertLWW: la UPDATE ha effetto solo se
// l'updated_at in arrivo è >= di quello già memorizzato, altrimenti la riga
// resta quella (più recente) già presente sul server. La chiave di conflitto
// è (user_id, id): l'ID è unico solo all'interno dell'account. Il bool
// restituito è true quando l'upsert ha scritto la riga.
func (r *FoldersRepo) UpsertLWW(ctx context.Context, f *models.Folder) (bool, error) {
	if f.ID == "" {
		f.ID = uuid.NewString()
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO folders (id, user_id, name, parent_id, updated_at, synced_at, deleted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, id) DO UPDATE SET
			name       = excluded.name,
			parent_id  = excluded.parent_id,
			updated_at = excluded.updated_at,
			synced_at  = excluded.synced_at,
			deleted_at = excluded.deleted_at
		WHERE excluded.updated_at >= folders.updated_at
	`, f.ID, f.UserID, f.Name, f.ParentID, f.UpdatedAt, f.SyncedAt, f.DeletedAt)
	if err != nil {
		return false, err
	}
	nRows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return nRows > 0, nil
}

// ForceSet sovrascrive incondizionatamente lo stato di cancellazione di una
// cartella, usata dalla cascade quando un antenato viene cancellato (vedi
// CascadeSoftDelete nel gestore di sync): la cancellazione del padre deve
// propagarsi ai figli indipendentemente dal loro updated_at precedente.
func (r *FoldersRepo) ForceSet(ctx context.Context, userID, id string, updatedAt, syncedAt, deletedAt int64) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE folders SET updated_at = ?, synced_at = ?, deleted_at = ? WHERE id = ? AND user_id = ?`,
		updatedAt, syncedAt, deletedAt, id, userID,
	)
	return err
}

// ListSyncedBetween restituisce le cartelle di un utente con synced_at
// nell'intervallo (since, upTo]. Vedi NotesRepo.ListSyncedBetween per la
// motivazione (cursore basato solo sull'orologio del server).
func (r *FoldersRepo) ListSyncedBetween(ctx context.Context, userID string, since, upTo int64, includeTombstones bool) ([]models.Folder, error) {
	query := `SELECT id, user_id, name, parent_id, updated_at, synced_at, deleted_at
		 FROM folders WHERE user_id = ? AND synced_at > ? AND synced_at <= ?`
	if !includeTombstones {
		query += ` AND deleted_at IS NULL`
	}
	query += ` ORDER BY synced_at ASC, id ASC`
	rows, err := r.db.QueryContext(ctx, query, userID, since, upTo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var folders []models.Folder
	for rows.Next() {
		var f models.Folder
		if err := rows.Scan(&f.ID, &f.UserID, &f.Name, &f.ParentID, &f.UpdatedAt, &f.SyncedAt, &f.DeletedAt); err != nil {
			return nil, err
		}
		folders = append(folders, f)
	}
	return folders, rows.Err()
}

// FolderChild è una sottocartella diretta con il suo stato di cancellazione.
// La cascade usa Active per decidere se propagare il tombstone, ma attraversa
// anche i rami già cancellati per raggiungere eventuali discendenti attivi.
type FolderChild struct {
	ID     string
	Active bool
}

// ListChildren restituisce tutte le sottocartelle dirette, tombstone inclusi,
// in una sola query. Prima la cascade ne eseguiva due (tutti i figli + soli
// attivi) per ogni cartella visitata.
func (r *FoldersRepo) ListChildren(ctx context.Context, userID, parentID string) ([]FolderChild, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, deleted_at IS NULL FROM folders WHERE user_id = ? AND parent_id = ?`,
		userID, parentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var children []FolderChild
	for rows.Next() {
		var child FolderChild
		var active int
		if err := rows.Scan(&child.ID, &active); err != nil {
			return nil, err
		}
		child.Active = active == 1
		children = append(children, child)
	}
	return children, rows.Err()
}

// PurgeExpiredTombstones rimuove definitivamente le cartelle soft-deleted il
// cui synced_at (istante server) è più vecchio della finestra di retention.
// Vedi NotesRepo.PurgeExpiredTombstones per la strategia completa.
func (r *FoldersRepo) PurgeExpiredTombstones(ctx context.Context, olderThan int64) (int64, error) {
	return purgeExpiredTombstones(ctx, r.db, "folders", olderThan)
}

// TouchSynced aggiorna SOLO synced_at di una riga esistente dell'utente. Serve
// quando un push perde il confronto LWW (il server ha già una versione più
// recente): la riga vincente non cambia, ma va ri-consegnata al dispositivo
// "perdente" nella pull della stessa richiesta, altrimenti quel dispositivo
// resterebbe con una copia divergente (il suo cursore è già oltre il vecchio
// synced_at della riga vincente).
func (r *FoldersRepo) TouchSynced(ctx context.Context, userID, id string, syncedAt int64) error {
	return touchSynced(ctx, r.db, "folders", userID, id, syncedAt)
}
