package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

// failingUserRevocationStore simula un DB che rifiuta la scrittura della
// revoca per utente.
type failingUserRevocationStore struct {
	saveErr error
}

func (s failingUserRevocationStore) SaveUserRevocation(context.Context, string, time.Time) error {
	return s.saveErr
}

func (failingUserRevocationStore) SaveTokenRevocation(context.Context, string, string, time.Time) error {
	return nil
}

func (failingUserRevocationStore) LoadActive(context.Context, time.Time) (map[string]time.Time, map[string]time.Time, error) {
	return map[string]time.Time{}, map[string]time.Time{}, nil
}

func (failingUserRevocationStore) PurgeExpired(context.Context, time.Time, time.Duration) error {
	return nil
}

func TestRevokeUserRevokesInMemoryEvenIfPersistenceFails(t *testing.T) {
	tm := NewTokenManager("01234567890123456789012345678901", time.Hour)
	ctx := context.Background()
	saveErr := errors.New("db non disponibile")
	if err := tm.AttachStore(ctx, failingUserRevocationStore{saveErr: saveErr}); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}
	token, _, err := tm.GenerateToken("u1", "mario", 1)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := tm.ParseToken(token)
	if err != nil {
		t.Fatal(err)
	}

	if err := tm.RevokeUser(ctx, "u1"); !errors.Is(err, saveErr) {
		t.Fatalf("RevokeUser: errore %v, atteso %v", err, saveErr)
	}
	if !tm.IsRevoked(claims) {
		t.Fatal("token ancora valido in RAM dopo una persistenza fallita")
	}
}

func TestRevocationUsesMillisecondIssueTime(t *testing.T) {
	tm := NewTokenManager("01234567890123456789012345678901", time.Hour)
	revokedAt := time.Now().Truncate(time.Second).Add(500 * time.Millisecond)
	tm.userRevoked["u1"] = revokedAt

	before := &Claims{UserID: "u1", IssuedAtMillis: revokedAt.Add(-time.Millisecond).UnixMilli()}
	if !tm.IsRevoked(before) {
		t.Fatal("token emesso prima della revoca non revocato")
	}
	after := &Claims{UserID: "u1", IssuedAtMillis: revokedAt.Add(time.Millisecond).UnixMilli()}
	if tm.IsRevoked(after) {
		t.Fatal("token emesso dopo la revoca (stesso secondo) erroneamente revocato")
	}

	token, _, err := tm.GenerateToken("u2", "mario", 1)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := tm.ParseToken(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.IssuedAtMillis == 0 {
		t.Fatal("iat_ms assente dal token generato")
	}
}

func TestRevokeUserDoesNotMoveRevocationBackwards(t *testing.T) {
	tm := NewTokenManager("01234567890123456789012345678901", time.Hour)
	future := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	tm.userRevoked["u1"] = future

	if err := tm.RevokeUser(context.Background(), "u1"); err != nil {
		t.Fatalf("RevokeUser: %v", err)
	}
	if got := tm.userRevoked["u1"]; !got.Equal(future) {
		t.Fatalf("revoca arretrata: %s, attesa %s", got, future)
	}
}
