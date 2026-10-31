package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestPurgeRevokedKeepsRecentReuseDetection(t *testing.T) {
	conn, err := Open(filepath.Join(t.TempDir(), "refresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx := context.Background()
	user, err := NewUsersRepo(conn).Create(ctx, "mario", "hash")
	if err != nil {
		t.Fatal(err)
	}
	repo := NewRefreshTokensRepo(conn)
	now := time.Now()

	old := RefreshToken{
		ID: "old", UserID: user.ID, FamilyID: "fam", TokenHash: "hash-old",
		IssuedAt:  now.Add(-10 * 24 * time.Hour).UnixMilli(),
		ExpiresAt: now.Add(80 * 24 * time.Hour).UnixMilli(),
	}
	if err := repo.Create(ctx, old); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx,
		`UPDATE refresh_tokens SET revoked_at = ? WHERE id = ?`,
		now.Add(-10*24*time.Hour).UnixMilli(), old.ID); err != nil {
		t.Fatal(err)
	}

	recent := RefreshToken{
		ID: "recent", UserID: user.ID, FamilyID: "fam", TokenHash: "hash-recent",
		IssuedAt: now.UnixMilli(), ExpiresAt: now.Add(90 * 24 * time.Hour).UnixMilli(),
	}
	if err := repo.Create(ctx, recent); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx,
		`UPDATE refresh_tokens SET revoked_at = ? WHERE id = ?`,
		now.Add(-time.Hour).UnixMilli(), recent.ID); err != nil {
		t.Fatal(err)
	}

	if err := repo.PurgeRevoked(ctx, now, RefreshTokenRevokedRetention); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.GetByHash(ctx, old.TokenHash); err != nil || got != nil {
		t.Fatalf("token revocato vecchio non potato: %+v, %v", got, err)
	}
	if got, err := repo.GetByHash(ctx, recent.TokenHash); err != nil || got == nil {
		t.Fatalf("token revocato recente potato: %+v, %v", got, err)
	}
}
