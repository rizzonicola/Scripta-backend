package db

import (
	"context"
	"path/filepath"
	"testing"

	"notes-server/internal/models"
)

func TestFoldersUpsertLWWReportsWhetherItWon(t *testing.T) {
	conn, err := Open(filepath.Join(t.TempDir(), "folders.db"))
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

	repo := NewFoldersRepo(conn)
	folder := &models.Folder{ID: "f1", UserID: "u1", Name: "v1", UpdatedAt: 100, SyncedAt: 100}
	if won, err := repo.UpsertLWW(ctx, folder); err != nil || !won {
		t.Fatalf("insert: won=%v err=%v", won, err)
	}

	folder.Name = "v2"
	folder.UpdatedAt, folder.SyncedAt = 200, 200
	if won, err := repo.UpsertLWW(ctx, folder); err != nil || !won {
		t.Fatalf("update più recente: won=%v err=%v", won, err)
	}

	folder.Name = "vecchia"
	folder.UpdatedAt, folder.SyncedAt = 150, 300
	if won, err := repo.UpsertLWW(ctx, folder); err != nil || won {
		t.Fatalf("update più vecchio: won=%v err=%v", won, err)
	}

	folder.Name = "pareggio"
	folder.UpdatedAt, folder.SyncedAt = 200, 400
	if won, err := repo.UpsertLWW(ctx, folder); err != nil || !won {
		t.Fatalf("pareggio: won=%v err=%v", won, err)
	}

	got, err := repo.Get(ctx, "u1", "f1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "pareggio" || got.SyncedAt != 400 {
		t.Fatalf("stato inatteso: %+v", got)
	}

	// Stesso ID, altro utente: la chiave composita (user_id, id) lo rende una
	// riga distinta. Non c'è conflitto né possibilità di sovrascrivere.
	other := &models.Folder{ID: "f1", UserID: "u2", Name: "intruso", UpdatedAt: 500, SyncedAt: 500}
	if won, err := repo.UpsertLWW(ctx, other); err != nil || !won {
		t.Fatalf("altro utente: won=%v err=%v", won, err)
	}
	gotOther, err := repo.Get(ctx, "u2", "f1")
	if err != nil {
		t.Fatal(err)
	}
	if gotOther == nil || gotOther.Name != "intruso" {
		t.Fatalf("cartella dell'altro utente: %+v", gotOther)
	}
	got, err = repo.Get(ctx, "u1", "f1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "pareggio" || got.SyncedAt != 400 {
		t.Fatalf("la cartella di u1 è stata modificata: %+v", got)
	}
}
