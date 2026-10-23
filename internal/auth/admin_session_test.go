package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestAdminSessionRejectsNonHS256(t *testing.T) {
	m := NewAdminSessionManager("01234567890123456789012345678901", time.Hour)
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
