package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestServerSettingsDefaultOnAndPersist(t *testing.T) {
	conn, err := Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx := context.Background()
	repo := NewServerSettingsRepo(conn)

	settings, err := repo.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !settings.UserRateLimitEnabled {
		t.Fatal("il rate limit globale deve essere attivo di default")
	}

	if err := repo.SetUserRateLimitEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	settings, err = repo.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings.UserRateLimitEnabled {
		t.Fatal("il rate limit globale è rimasto attivo dopo il toggle")
	}
	if settings.UpdatedAt == 0 {
		t.Fatal("updated_at non valorizzato")
	}
}
