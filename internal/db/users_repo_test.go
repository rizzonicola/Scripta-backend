package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestUsersRepoMatchesUsernameCaseSensitively(t *testing.T) {
	conn, err := Open(filepath.Join(t.TempDir(), "users.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx := context.Background()
	repo := NewUsersRepo(conn)

	user, err := repo.Create(ctx, "Mario", "hash")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if user.Username != "Mario" {
		t.Fatalf("username salvato = %q, atteso %q", user.Username, "Mario")
	}

	got, err := repo.GetByUsername(ctx, "Mario")
	if err != nil {
		t.Fatalf("GetByUsername: %v", err)
	}
	if got == nil || got.ID != user.ID || got.Username != "Mario" {
		t.Fatalf("utente = %+v, atteso id %s e username Mario", got, user.ID)
	}

	for _, variant := range []string{"mARIO", "mario", "MARIO"} {
		got, err := repo.GetByUsername(ctx, variant)
		if err != nil {
			t.Fatalf("GetByUsername(%q): %v", variant, err)
		}
		if got != nil {
			t.Fatalf("GetByUsername(%q) = %+v, atteso nessun utente", variant, got)
		}

		created, err := repo.Create(ctx, variant, "hash")
		if err != nil {
			t.Fatalf("Create(%q): %v", variant, err)
		}
		if created.ID == user.ID {
			t.Fatalf("Create(%q) ha riutilizzato l'id di Mario", variant)
		}
	}

	if _, err := repo.Create(ctx, "Mario", "hash"); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("duplicato esatto = %v, atteso ErrUsernameTaken", err)
	}

	injected, err := repo.GetByUsername(ctx, "' OR 1=1 --")
	if err != nil {
		t.Fatalf("GetByUsername injection: %v", err)
	}
	if injected != nil {
		t.Fatalf("la lookup ha restituito un utente per un input di injection: %+v", injected)
	}
}

func TestMigrateDropsCaseInsensitiveUsernameIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migration.db")
	conn, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if _, err := conn.Exec(`CREATE UNIQUE INDEX idx_users_username_nocase ON users(username COLLATE NOCASE)`); err != nil {
		t.Fatalf("creazione indice legacy: %v", err)
	}
	repo := NewUsersRepo(conn)
	if _, err := repo.Create(ctx, "Nicola", "hash"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	// Il riavvio esegue di nuovo la migrazione: l'indice legacy deve sparire
	// e l'UNIQUE nativo sulla colonna deve restare case-sensitive.
	conn, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_users_username_nocase'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("l'indice case-insensitive legacy non è stato rimosso")
	}

	repo = NewUsersRepo(conn)
	got, err := repo.GetByUsername(ctx, "Nicola")
	if err != nil || got == nil {
		t.Fatalf("GetByUsername(Nicola) = %+v, %v", got, err)
	}
	got, err = repo.GetByUsername(ctx, "nicola")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("GetByUsername(nicola) = %+v, atteso nessun utente", got)
	}
	if _, err := repo.Create(ctx, "nicola", "hash"); err != nil {
		t.Fatalf("creazione variante di case: %v", err)
	}
}
