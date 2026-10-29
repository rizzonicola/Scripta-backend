package db

import (
	"context"
	"path/filepath"
	"testing"

	"notes-server/internal/models"
)

func TestSettingsRepoGetReturnsDefaultsWithoutWriting(t *testing.T) {
	conn, err := Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, `INSERT INTO users(id, username, password_hash, created_at) VALUES('u1', 'u1', 'h', 1)`); err != nil {
		t.Fatal(err)
	}

	repo := NewSettingsRepo(conn)
	got, err := repo.Get(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	want := models.DefaultUserSettings()
	if got != want {
		t.Fatalf("default inattesi: %+v, attesi %+v", got, want)
	}

	var count int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_settings WHERE user_id = 'u1'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("GET ha scritto %d righe", count)
	}
}

func TestSettingsRepoUpsertRoundTrip(t *testing.T) {
	conn, err := Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, `INSERT INTO users(id, username, password_hash, created_at) VALUES('u1', 'u1', 'h', 1)`); err != nil {
		t.Fatal(err)
	}

	repo := NewSettingsRepo(conn)
	in := models.UserSettings{
		Theme: "dark", ColorScheme: "nord", Language: "en",
		FontFamily: "JetBrains Mono", FontSize: 18, LineSpacing: 1.8, Layout: "split",
	}
	saved, err := repo.Upsert(ctx, "u1", in)
	if err != nil {
		t.Fatal(err)
	}
	if saved.UpdatedAt == 0 {
		t.Fatal("updated_at non impostato")
	}
	got, err := repo.Get(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if got != saved {
		t.Fatalf("round trip: got %+v, saved %+v", got, saved)
	}
}
