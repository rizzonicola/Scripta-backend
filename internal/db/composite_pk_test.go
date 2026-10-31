package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"notes-server/internal/models"
)

// TestMigrateCompositePrimaryKeysPreservesData simula un database creato
// prima della chiave composita (PK globale su id), lo apre con Open (che
// esegue la migrazione) e verifica che:
//   - i dati esistenti siano intatti;
//   - due utenti possano ora avere lo stesso id senza conflitto;
//   - indici e trigger siano stati ricreati.
func TestMigrateCompositePrimaryKeysPreservesData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	raw, err := sql.Open("libsql", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	oldSchema := []string{
		`CREATE TABLE users (
			id                TEXT PRIMARY KEY,
			username          TEXT NOT NULL UNIQUE,
			password_hash     TEXT NOT NULL,
			created_at        INTEGER NOT NULL,
			blocked_at        INTEGER,
			rate_limit_exempt INTEGER NOT NULL DEFAULT 0,
			password_version  INTEGER NOT NULL DEFAULT 1
		)`,
		`CREATE TABLE folders (
			id         TEXT PRIMARY KEY,
			user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			name       TEXT NOT NULL,
			parent_id  TEXT,
			updated_at INTEGER NOT NULL,
			synced_at  INTEGER NOT NULL DEFAULT 0,
			deleted_at INTEGER
		)`,
		`CREATE TABLE notes (
			id          TEXT PRIMARY KEY,
			user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			title       TEXT NOT NULL DEFAULT '',
			content     TEXT NOT NULL DEFAULT '',
			folder_id   TEXT,
			is_favorite INTEGER NOT NULL DEFAULT 0,
			is_pinned   INTEGER NOT NULL DEFAULT 0,
			order_index INTEGER NOT NULL DEFAULT 0,
			created_at  INTEGER NOT NULL DEFAULT 0,
			updated_at  INTEGER NOT NULL,
			synced_at   INTEGER NOT NULL DEFAULT 0,
			deleted_at  INTEGER
		)`,
		`INSERT INTO users(id, username, password_hash, created_at) VALUES
			('u1', 'mario', 'h', 1),
			('u2', 'luigi', 'h', 1)`,
		`INSERT INTO folders(id, user_id, name, updated_at, synced_at) VALUES('f1', 'u1', 'Mario', 10, 10)`,
		`INSERT INTO notes(id, user_id, title, content, updated_at, synced_at) VALUES('n1', 'u1', 'Nota', 'testo', 10, 10)`,
	}
	for _, stmt := range oldSchema {
		if _, err := raw.Exec(stmt); err != nil {
			_ = raw.Close()
			t.Fatalf("schema legacy: %v", err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	conn, err := Open(path)
	if err != nil {
		t.Fatalf("open migrato: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx := context.Background()

	// Dati preesistenti intatti.
	var folderName, noteTitle string
	if err := conn.QueryRow(`SELECT name FROM folders WHERE user_id = 'u1' AND id = 'f1'`).Scan(&folderName); err != nil {
		t.Fatalf("cartella migrata: %v", err)
	}
	if err := conn.QueryRow(`SELECT title FROM notes WHERE user_id = 'u1' AND id = 'n1'`).Scan(&noteTitle); err != nil {
		t.Fatalf("nota migrata: %v", err)
	}
	if folderName != "Mario" || noteTitle != "Nota" {
		t.Fatalf("dati alterati: folder=%q note=%q", folderName, noteTitle)
	}

	// Stesso id per un altro utente: ora è una riga distinta.
	folders := NewFoldersRepo(conn)
	notes := NewNotesRepo(conn)
	if won, err := folders.UpsertLWW(ctx, &models.Folder{
		ID: "f1", UserID: "u2", Name: "Luigi", UpdatedAt: 20, SyncedAt: 20,
	}); err != nil || !won {
		t.Fatalf("cartella stesso id altro utente: won=%v err=%v", won, err)
	}
	if won, err := notes.UpsertLWW(ctx, &models.Note{
		ID: "n1", UserID: "u2", Title: "Nota di Luigi", UpdatedAt: 20, SyncedAt: 20,
	}); err != nil || !won {
		t.Fatalf("nota stesso id altro utente: won=%v err=%v", won, err)
	}

	if f, err := folders.Get(ctx, "u1", "f1"); err != nil || f == nil || f.Name != "Mario" {
		t.Fatalf("cartella di u1 alterata: %+v err=%v", f, err)
	}
	if n, err := notes.Get(ctx, "u1", "n1"); err != nil || n == nil || n.Title != "Nota" {
		t.Fatalf("nota di u1 alterata: %+v err=%v", n, err)
	}

	// Indici e trigger ricreati dopo il rebuild.
	for _, idx := range []string{"idx_folders_parent", "idx_folders_deleted_at", "idx_notes_folder", "idx_notes_deleted_at"} {
		var count int
		if err := conn.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, idx).Scan(&count); err != nil {
			t.Fatalf("indice %s: %v", idx, err)
		}
		if count != 1 {
			t.Fatalf("indice %s mancante dopo la migrazione", idx)
		}
	}
	var triggers int
	if err := conn.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'trigger' AND name LIKE 'trg_%_ownership_%'`).Scan(&triggers); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if triggers != 4 {
		t.Fatalf("trigger di ownership=%d, attesi 4", triggers)
	}

	// Idempotenza: un secondo migrate non ricostruisce nulla e non perde righe.
	if err := migrate(conn); err != nil {
		t.Fatalf("seconda migrazione: %v", err)
	}
	var notesCount int
	if err := conn.QueryRow(`SELECT count(*) FROM notes WHERE id = 'n1'`).Scan(&notesCount); err != nil {
		t.Fatalf("conteggio note: %v", err)
	}
	if notesCount != 2 {
		t.Fatalf("note con id n1=%d, attese 2", notesCount)
	}
}
