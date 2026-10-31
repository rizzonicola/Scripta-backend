package auth

import (
	"context"
	"testing"
	"time"
)

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
