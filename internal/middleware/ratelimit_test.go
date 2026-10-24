package middleware

import (
	"testing"
	"time"
)

func TestKeyedRateLimiterConsumesBurstAndSeparatesKeys(t *testing.T) {
	limiter := newKeyedRateLimiter(0, 1, time.Hour)
	if !limiter.allow("a") {
		t.Fatal("il primo token del burst deve essere disponibile")
	}
	if limiter.allow("a") {
		t.Fatal("il secondo token deve essere rifiutato con burst 1 e refill 0")
	}
	if !limiter.allow("b") {
		t.Fatal("le chiavi devono avere bucket indipendenti")
	}
}
