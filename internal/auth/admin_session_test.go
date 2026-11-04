package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestAdminSessionRejectsNonHS256(t *testing.T) {
	m := NewAdminSessionManager("01234567890123456789012345678901", "admin-pass", time.Hour)
	claims := adminSessionClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   adminSessionSubject,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS512, claims).SignedString(m.secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(token); err == nil {
		t.Fatal("sessione HS512 accettata")
	}
}

func TestAdminSessionInvalidatedWhenPasswordChanges(t *testing.T) {
	old := NewAdminSessionManager("01234567890123456789012345678901", "vecchia-password", time.Hour)
	token, _, err := old.GenerateSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Validate(token); err != nil {
		t.Fatalf("sessione appena emessa rifiutata: %v", err)
	}
	fresh := NewAdminSessionManager("01234567890123456789012345678901", "nuova-password", time.Hour)
	if err := fresh.Validate(token); err == nil {
		t.Fatal("cookie emesso con la vecchia ADMIN_PASS ancora accettato")
	}
}
