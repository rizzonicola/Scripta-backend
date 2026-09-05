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

// Get recupera i metadati di una nota per (userID, relativePath).
// Restituisce (nil, nil) se non esiste.
func (r *NotesRepo) Get(ctx context.Context, userID, relativePath string) (*models.Note, error) {
	var n models.Note
	var deleted int
	err := r.db.QueryRowContext(ctx,
		`SELECT id, user_id, relative_path, updated_at, deleted, checksum
		 FROM notes WHERE user_id = ? AND relative_path = ?`,
		userID, relativePath,
	).Scan(&n.ID, &n.UserID, &n.RelativePath, &n.UpdatedAt, &deleted, &n.Checksum)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	n.Deleted = deleted == 1
	return &n, nil
}

// Upsert inserisce o aggiorna i metadati di una nota (usato dopo la scrittura fisica del file).
func (r *NotesRepo) Upsert(ctx context.Context, n *models.Note) error {
	if n.ID == "" {
		n.ID = uuid.NewString()
	}
	deleted := 0
	if n.Deleted {
		deleted = 1
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO notes (id, user_id, relative_path, updated_at, deleted, checksum)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, relative_path) DO UPDATE SET
			updated_at = excluded.updated_at,
			deleted    = excluded.deleted,
			checksum   = excluded.checksum
	`, n.ID, n.UserID, n.RelativePath, n.UpdatedAt, deleted, n.Checksum)
	return err
}

// ListByUser restituisce tutte le note (metadati) di un utente.
func (r *NotesRepo) ListByUser(ctx context.Context, userID string) ([]models.Note, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, user_id, relative_path, updated_at, deleted, checksum FROM notes WHERE user_id = ?`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var notes []models.Note
	for rows.Next() {
		var n models.Note
		var deleted int
		if err := rows.Scan(&n.ID, &n.UserID, &n.RelativePath, &n.UpdatedAt, &deleted, &n.Checksum); err != nil {
			return nil, err
		}
		n.Deleted = deleted == 1
		notes = append(notes, n)
	}
	return notes, rows.Err()
}
