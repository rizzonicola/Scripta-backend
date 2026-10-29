package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"notes-server/internal/models"
)

func TestAddNotesCreatedAtColumnBackfillsAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	conn, err := sql.Open("libsql", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	if _, err := conn.Exec(`CREATE TABLE notes (
		id         TEXT PRIMARY KEY,
		updated_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO notes (id, updated_at) VALUES ('n1', 1234)`); err != nil {
		t.Fatal(err)
	}

	if err := addNotesCreatedAtColumn(conn); err != nil {
		t.Fatalf("prima migrazione: %v", err)
	}
	var createdAt int64
	if err := conn.QueryRow(`SELECT created_at FROM notes WHERE id = 'n1'`).Scan(&createdAt); err != nil {
		t.Fatal(err)
	}
	if createdAt != 1234 {
		t.Fatalf("created_at = %d, atteso 1234", createdAt)
	}

	if _, err := conn.Exec(`INSERT INTO notes (id, updated_at) VALUES ('n2', 5678)`); err != nil {
		t.Fatal(err)
	}
	if err := addNotesCreatedAtColumn(conn); err != nil {
		t.Fatalf("seconda migrazione: %v", err)
	}
	if err := conn.QueryRow(`SELECT created_at FROM notes WHERE id = 'n2'`).Scan(&createdAt); err != nil {
		t.Fatal(err)
	}
	if createdAt != 5678 {
		t.Fatalf("created_at = %d, atteso 5678", createdAt)
	}
}

func TestNotesRepoPersistsCreatedAt(t *testing.T) {
	conn, err := Open(filepath.Join(t.TempDir(), "notes.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx := context.Background()

	if _, err := conn.Exec(`INSERT INTO users (id, username, password_hash, created_at) VALUES ('u1', 'u1', 'h', 1)`); err != nil {
		t.Fatal(err)
	}
	repo := NewNotesRepo(conn)
	want := &models.Note{
		ID:                "n1",
		UserID:            "u1",
		Title:             "titolo",
		Content:           "contenuto",
		CreatedAt:         1000,
		CreatedAtProvided: true,
		UpdatedAt:         2000,
		SyncedAt:          3000,
	}
	if err := repo.UpsertLWW(ctx, want); err != nil {
		t.Fatal(err)
	}

	got, err := repo.Get(ctx, "u1", "n1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.CreatedAt != 1000 {
		t.Fatalf("created_at dopo Get = %+v, atteso 1000", got)
	}

	want.CreatedAt = 500
	want.UpdatedAt = 4000
	want.SyncedAt = 4000
	if err := repo.UpsertLWW(ctx, want); err != nil {
		t.Fatal(err)
	}
	rows, err := repo.ListSyncedBetween(ctx, "u1", 3000, 4000, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].CreatedAt != 500 {
		t.Fatalf("righe dopo ListSyncedBetween = %+v, atteso created_at 500", rows)
	}

	// Un client precedente non invia created_at: il suo updated_at usato come
	// fallback non deve cancellare la data di creazione già nota al server.
	oldClient := &models.Note{
		ID:        "n1",
		UserID:    "u1",
		Title:     "titolo aggiornato",
		Content:   "contenuto",
		CreatedAt: 5000,
		UpdatedAt: 5000,
		SyncedAt:  5000,
	}
	if err := repo.UpsertLWW(ctx, oldClient); err != nil {
		t.Fatal(err)
	}
	got, err = repo.Get(ctx, "u1", "n1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.CreatedAt != 500 || got.UpdatedAt != 5000 {
		t.Fatalf("dopo push client vecchio = %+v, attesi created_at 500 e updated_at 5000", got)
	}
}
