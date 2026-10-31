package db

import (
	"context"
	"path/filepath"
	"testing"

	"notes-server/internal/models"
)

func TestNotesUpsertLWWReportsWhetherItWon(t *testing.T) {
	conn, err := Open(filepath.Join(t.TempDir(), "notes.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx := context.Background()
	for _, id := range []string{"u1", "u2"} {
		if _, err := conn.ExecContext(ctx, `INSERT INTO users(id, username, password_hash, created_at) VALUES(?, ?, 'h', 1)`, id, id); err != nil {
			t.Fatal(err)
		}
	}

	repo := NewNotesRepo(conn)
	note := &models.Note{ID: "n1", UserID: "u1", Title: "v1", UpdatedAt: 100, SyncedAt: 100}
	if won, err := repo.UpsertLWW(ctx, note); err != nil || !won {
		t.Fatalf("insert: won=%v err=%v", won, err)
	}

	note.Title = "v2"
	note.UpdatedAt, note.SyncedAt = 200, 200
	if won, err := repo.UpsertLWW(ctx, note); err != nil || !won {
		t.Fatalf("update più recente: won=%v err=%v", won, err)
	}

	note.Title = "vecchia"
	note.UpdatedAt, note.SyncedAt = 150, 300
	if won, err := repo.UpsertLWW(ctx, note); err != nil || won {
		t.Fatalf("update più vecchio: won=%v err=%v", won, err)
	}

	note.Title = "pareggio"
	note.UpdatedAt, note.SyncedAt = 200, 400
	if won, err := repo.UpsertLWW(ctx, note); err != nil || !won {
		t.Fatalf("pareggio: won=%v err=%v", won, err)
	}

	got, err := repo.Get(ctx, "u1", "n1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "pareggio" || got.SyncedAt != 400 {
		t.Fatalf("stato inatteso: %+v", got)
	}

	// Stesso ID, altro utente: la chiave composita (user_id, id) lo rende una
	// riga distinta. Non c'è conflitto né possibilità di sovrascrivere.
	other := &models.Note{ID: "n1", UserID: "u2", Title: "intruso", UpdatedAt: 500, SyncedAt: 500}
	if won, err := repo.UpsertLWW(ctx, other); err != nil || !won {
		t.Fatalf("altro utente: won=%v err=%v", won, err)
	}
	gotOther, err := repo.Get(ctx, "u2", "n1")
	if err != nil {
		t.Fatal(err)
	}
	if gotOther == nil || gotOther.Title != "intruso" {
		t.Fatalf("nota dell'altro utente: %+v", gotOther)
	}
	got, err = repo.Get(ctx, "u1", "n1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "pareggio" || got.SyncedAt != 400 {
		t.Fatalf("la nota di u1 è stata modificata: %+v", got)
	}
}
