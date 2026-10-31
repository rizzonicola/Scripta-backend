package auth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"notes-server/internal/db"
)

// Due TokenManager sullo stesso DB simulano due processi/repliche: la revoca
// scritta dal primo non è visibile al secondo finché non ricarica la cache.
func TestReloadRevocationsSeesAnotherProcessRevocation(t *testing.T) {
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "revocations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()

	repo := db.NewRevocationRepo(sqlDB)
	tm1 := NewTokenManager("01234567890123456789012345678901", time.Hour)
	tm2 := NewTokenManager("01234567890123456789012345678901", time.Hour)
	ctx := context.Background()
	if err := tm1.AttachStore(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if err := tm2.AttachStore(ctx, repo); err != nil {
		t.Fatal(err)
	}

	token, _, err := tm1.GenerateToken("u1", "mario", 1)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := tm1.ParseToken(token)
	if err != nil {
		t.Fatal(err)
	}
	if err := tm1.RevokeUser(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	if !tm1.IsRevoked(claims) {
		t.Fatal("il processo che ha revocato non vede la revoca")
	}
	if tm2.IsRevoked(claims) {
		t.Fatal("la revoca è già visibile senza reload: il test non esercita il caso multi-processo")
	}
	if err := tm2.ReloadRevocations(ctx); err != nil {
		t.Fatal(err)
	}
	if !tm2.IsRevoked(claims) {
		t.Fatal("dopo il reload la revoca non è visibile al secondo processo")
	}
}

// Il reload non deve far regredire una revoca locale più recente di quella
// presente nel DB (es. scrittura DB fallita ma revoca già applicata in RAM).
func TestReloadRevocationsNeverMovesLocalRevocationBackwards(t *testing.T) {
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "revocations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()

	repo := db.NewRevocationRepo(sqlDB)
	tm := NewTokenManager("01234567890123456789012345678901", time.Hour)
	ctx := context.Background()
	if err := tm.AttachStore(ctx, repo); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	tm.mu.Lock()
	tm.userRevoked["u1"] = future
	tm.mu.Unlock()

	if err := tm.ReloadRevocations(ctx); err != nil {
		t.Fatal(err)
	}
	tm.mu.RLock()
	got := tm.userRevoked["u1"]
	tm.mu.RUnlock()
	if !got.Equal(future) {
		t.Fatalf("revoca arretrata dopo il reload: %s, attesa %s", got, future)
	}
}
