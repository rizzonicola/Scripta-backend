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
		`SELECT id, user_id, name, parent_id, updated_at, deleted_at
		 FROM folders WHERE id = ? AND user_id = ?`,
		id, userID,
	).Scan(&f.ID, &f.UserID, &f.Name, &f.ParentID, &f.UpdatedAt, &f.DeletedAt)
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
		INSERT INTO folders (id, user_id, name, parent_id, updated_at, deleted_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name       = excluded.name,
			parent_id  = excluded.parent_id,
			updated_at = excluded.updated_at,
			deleted_at = excluded.deleted_at
		WHERE excluded.updated_at >= folders.updated_at
		  AND folders.user_id = excluded.user_id
	`, f.ID, f.UserID, f.Name, f.ParentID, f.UpdatedAt, f.DeletedAt)
	return err
}

// ForceSet sovrascrive incondizionatamente lo stato di cancellazione di una
// cartella, usata dalla cascade quando un antenato viene cancellato (vedi
// CascadeSoftDelete nel gestore di sync): la cancellazione del padre deve
// propagarsi ai figli indipendentemente dal loro updated_at precedente.
func (r *FoldersRepo) ForceSet(ctx context.Context, userID, id string, updatedAt int64, deletedAt *int64) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE folders SET updated_at = ?, deleted_at = ? WHERE id = ? AND user_id = ?`,
		updatedAt, deletedAt, id, userID,
	)
	return err
}

// ListUpdatedSince restituisce tutte le cartelle (incluse le soft-deleted)
// di un utente con updated_at strettamente maggiore di "since". Query di
// "pull" della sync, analoga a NotesRepo.ListUpdatedSince.
func (r *FoldersRepo) ListUpdatedSince(ctx context.Context, userID string, since int64) ([]models.Folder, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, user_id, name, parent_id, updated_at, deleted_at
		 FROM folders WHERE user_id = ? AND updated_at > ? ORDER BY updated_at ASC`,
		userID, since,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var folders []models.Folder
	for rows.Next() {
		var f models.Folder
		if err := rows.Scan(&f.ID, &f.UserID, &f.Name, &f.ParentID, &f.UpdatedAt, &f.DeletedAt); err != nil {
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

// PurgeExpiredTombstones rimuove definitivamente le cartelle soft-deleted da
// più tempo della finestra di retention. Vedi NotesRepo.PurgeExpiredTombstones
// per la spiegazione completa della strategia di retention.
func (r *FoldersRepo) PurgeExpiredTombstones(ctx context.Context, olderThan int64) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM folders WHERE deleted_at IS NOT NULL AND deleted_at < ?`,
		olderThan,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
