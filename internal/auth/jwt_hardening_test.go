package auth

import (
	"context"
	"testing"
	"time"
)

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
