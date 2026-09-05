package db

import (
	"context"
	"database/sql"

	"github.com/google/uuid"

	"notes-server/internal/models"
)

// execer è l'interfaccia minima comune tra *sql.DB e *sql.Tx: permette a
// NotesRepo di operare sia in modalità standalone sia dentro una transazione
// esplicita, senza duplicare le query.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type NotesRepo struct {
	db execer
	// sqlDB è non-nil solo sull'istanza "radice" (quella creata con
	// NewNotesRepo) e abilita BeginTx. Le istanze restituite da WithTx non
	// possono aprire a loro volta una transazione annidata.
	sqlDB *sql.DB
}

func NewNotesRepo(db *sql.DB) *NotesRepo {
	return &NotesRepo{db: db, sqlDB: db}
}

// BeginTx apre una transazione esplicita e restituisce sia la *sql.Tx (che il
// chiamante deve Commit o Rollback) sia un NotesRepo che opera al suo
// interno. Usata dalla sync batch per raggruppare più upsert in un solo
// commit: meno fsync (soprattutto con synchronous=NORMAL) e atomicità sui
// metadati dell'intero batch.
func (r *NotesRepo) BeginTx(ctx context.Context) (*sql.Tx, *NotesRepo, error) {
	if r.sqlDB == nil {
		return nil, nil, sql.ErrTxDone
	}
	tx, err := r.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	return tx, &NotesRepo{db: tx}, nil
}

// Get recupera i metadati di una nota (o cartella) per (userID, relativePath).
// Restituisce (nil, nil) se non esiste.
func (r *NotesRepo) Get(ctx context.Context, userID, relativePath string) (*models.Note, error) {
	var n models.Note
	var deleted, isFolder int
	err := r.db.QueryRowContext(ctx,
		`SELECT id, user_id, relative_path, updated_at, deleted, checksum, is_folder
		 FROM notes WHERE user_id = ? AND relative_path = ?`,
		userID, relativePath,
	).Scan(&n.ID, &n.UserID, &n.RelativePath, &n.UpdatedAt, &deleted, &n.Checksum, &isFolder)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	n.Deleted = deleted == 1
	n.IsFolder = isFolder == 1
	return &n, nil
}

// Upsert inserisce o aggiorna i metadati di una nota o cartella (usato dopo
// la scrittura/spostamento fisico sul filesystem).
func (r *NotesRepo) Upsert(ctx context.Context, n *models.Note) error {
	if n.ID == "" {
		n.ID = uuid.NewString()
	}
	deleted := 0
	if n.Deleted {
		deleted = 1
	}
	isFolder := 0
	if n.IsFolder {
		isFolder = 1
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO notes (id, user_id, relative_path, updated_at, deleted, checksum, is_folder)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, relative_path) DO UPDATE SET
			updated_at = excluded.updated_at,
			deleted    = excluded.deleted,
			checksum   = excluded.checksum,
			is_folder  = excluded.is_folder
	`, n.ID, n.UserID, n.RelativePath, n.UpdatedAt, deleted, n.Checksum, isFolder)
	return err
}

// Delete rimuove definitivamente i metadati di una nota/cartella per
// (userID, relativePath). A differenza dell'Upsert con Deleted=true (soft
// delete usata dalla sync per propagare la cancellazione agli altri device),
// questa è una hard delete: viene usata quando una nota/cartella cambia
// path (move/rename), per eliminare la vecchia riga ormai priva di senso
// invece di lasciarla come tombstone a quel path.
func (r *NotesRepo) Delete(ctx context.Context, userID, relativePath string) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM notes WHERE user_id = ? AND relative_path = ?`,
		userID, relativePath,
	)
	return err
}

// ListByUser restituisce tutte le note e cartelle (metadati) di un utente.
func (r *NotesRepo) ListByUser(ctx context.Context, userID string) ([]models.Note, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, user_id, relative_path, updated_at, deleted, checksum, is_folder FROM notes WHERE user_id = ?`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var notes []models.Note
	for rows.Next() {
		var n models.Note
		var deleted, isFolder int
		if err := rows.Scan(&n.ID, &n.UserID, &n.RelativePath, &n.UpdatedAt, &deleted, &n.Checksum, &isFolder); err != nil {
			return nil, err
		}
		n.Deleted = deleted == 1
		n.IsFolder = isFolder == 1
		notes = append(notes, n)
	}
	return notes, rows.Err()
}
