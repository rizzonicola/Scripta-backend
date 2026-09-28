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
// resta quella (più recente) già presente sul server. Vedi il commento su
// NotesRepo.UpsertLWW per il dettaglio della clausola di isolamento per
// utente.
func (r *FoldersRepo) UpsertLWW(ctx context.Context, f *models.Folder) error {
	if f.ID == "" {
		f.ID = uuid.NewString()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO folders (id, user_id, name, parent_id, updated_at, synced_at, deleted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name       = excluded.name,
			parent_id  = excluded.parent_id,
			updated_at = excluded.updated_at,
			synced_at  = excluded.synced_at,
			deleted_at = excluded.deleted_at
		WHERE excluded.updated_at >= folders.updated_at
		  AND folders.user_id = excluded.user_id
	`, f.ID, f.UserID, f.Name, f.ParentID, f.UpdatedAt, f.SyncedAt, f.DeletedAt)
	return err
}

// ForceSet sovrascrive incondizionatamente lo stato di cancellazione di una
// cartella, usata dalla cascade quando un antenato viene cancellato (vedi
// CascadeSoftDelete nel gestore di sync): la cancellazione del padre deve
// propagarsi ai figli indipendentemente dal loro updated_at precedente.
func (r *FoldersRepo) ForceSet(ctx context.Context, userID, id string, updatedAt, syncedAt int64, deletedAt *int64) error {
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

// ListActiveChildIDs restituisce gli ID delle sottocartelle dirette e ancora
// attive (non soft-deleted) di una data cartella. Usata dalla cascade
// soft-delete per attraversare ricorsivamente il sottoalbero.
func (r *FoldersRepo) ListActiveChildIDs(ctx context.Context, userID, parentID string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id FROM folders WHERE user_id = ? AND parent_id = ? AND deleted_at IS NULL`,
		userID, parentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// PurgeExpiredTombstones rimuove definitivamente le cartelle soft-deleted il
// cui synced_at (istante server) è più vecchio della finestra di retention.
// Vedi NotesRepo.PurgeExpiredTombstones per la strategia completa.
func (r *FoldersRepo) PurgeExpiredTombstones(ctx context.Context, olderThan int64) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM folders WHERE deleted_at IS NOT NULL AND synced_at < ?`,
		olderThan,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// TouchSynced aggiorna SOLO synced_at di una riga esistente dell'utente. Serve
// quando un push perde il confronto LWW (il server ha già una versione più
// recente): la riga vincente non cambia, ma va ri-consegnata al dispositivo
// "perdente" nella pull della stessa richiesta, altrimenti quel dispositivo
// resterebbe con una copia divergente (il suo cursore è già oltre il vecchio
// synced_at della riga vincente).
func (r *FoldersRepo) TouchSynced(ctx context.Context, userID, id string, syncedAt int64) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE folders SET synced_at = ? WHERE id = ? AND user_id = ?`,
		syncedAt, id, userID,
	)
	return err
}
