package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestUsersRepoPreservesUsernameCaseAndMatchesCaseInsensitively(t *testing.T) {
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

	got, err := repo.GetByUsername(ctx, "mARIO")
	if err != nil {
		t.Fatalf("GetByUsername: %v", err)
	}
	if got == nil || got.ID != user.ID || got.Username != "Mario" {
		t.Fatalf("utente = %+v, atteso id %s e username Mario", got, user.ID)
	}

	if _, err := repo.Create(ctx, "MARIO", "hash"); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("seconda creazione = %v, atteso ErrUsernameTaken", err)
	}

	injected, err := repo.GetByUsername(ctx, "' OR 1=1 --")
	if err != nil {
		t.Fatalf("GetByUsername injection: %v", err)
	}
	if injected != nil {
		t.Fatalf("la lookup ha restituito un utente per un input di injection: %+v", injected)
	}
}
