package auth

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type fakeAdminRevocationStore struct {
	tokens map[string]time.Time
}

func (f *fakeAdminRevocationStore) SaveTokenRevocation(_ context.Context, jti, _ string, expiresAt time.Time) error {
	f.tokens[jti] = expiresAt
	return nil
}

func (f *fakeAdminRevocationStore) LoadActive(_ context.Context, _ time.Time) (map[string]time.Time, map[string]time.Time, error) {
	tokens := make(map[string]time.Time, len(f.tokens))
	for jti, exp := range f.tokens {
		tokens[jti] = exp
	}
	return map[string]time.Time{}, tokens, nil
}

func (f *fakeAdminRevocationStore) IsTokenRevoked(_ context.Context, jti string, now time.Time) (bool, error) {
	exp, ok := f.tokens[jti]
	return ok && exp.After(now), nil
}

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
	if err := m.Validate(context.Background(), token); err == nil {
		t.Fatal("sessione HS512 accettata")
	}
}

func TestAdminSessionInvalidatedWhenPasswordChanges(t *testing.T) {
	old := NewAdminSessionManager("01234567890123456789012345678901", "vecchia-password", time.Hour)
	token, _, err := old.GenerateSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Validate(context.Background(), token); err != nil {
		t.Fatalf("sessione appena emessa rifiutata: %v", err)
	}
	fresh := NewAdminSessionManager("01234567890123456789012345678901", "nuova-password", time.Hour)
	if err := fresh.Validate(context.Background(), token); err == nil {
		t.Fatal("cookie emesso con la vecchia ADMIN_PASS ancora accettato")
	}
}

func TestAdminSessionRevocationIsVisibleAcrossInstances(t *testing.T) {
	const secret = "01234567890123456789012345678901"
	const pass = "admin-pass"
	store := &fakeAdminRevocationStore{tokens: map[string]time.Time{}}
	ctx := context.Background()

	// Due istanze sullo stesso store: B ha già caricato la cache prima che A
	// gestisse il logout. Senza il controllo per-request B accetterebbe il
	// cookie fino al reload successivo.
	a := NewAdminSessionManager(secret, pass, time.Hour)
	b := NewAdminSessionManager(secret, pass, time.Hour)
	if err := a.AttachStore(ctx, store); err != nil {
		t.Fatal(err)
	}
	if err := b.AttachStore(ctx, store); err != nil {
		t.Fatal(err)
	}

	token, _, err := a.GenerateSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Validate(ctx, token); err != nil {
		t.Fatalf("sessione valida rifiutata prima del logout: %v", err)
	}
	if err := a.Revoke(ctx, token); err != nil {
		t.Fatal(err)
	}
	if err := b.Validate(ctx, token); err == nil {
		t.Fatal("sessione revocata su un'altra istanza ancora accettata")
	}
}

func TestAdminSessionCredentialIsNotAPlainPasswordHash(t *testing.T) {
	const pass = "admin-pass"
	m := NewAdminSessionManager("01234567890123456789012345678901", pass, time.Hour)
	token, _, err := m.GenerateSession()
	if err != nil {
		t.Fatal(err)
	}
	claims := &adminSessionClaims{}
	if _, err := jwt.ParseWithClaims(token, claims, func(*jwt.Token) (interface{}, error) {
		return m.secret, nil
	}); err != nil {
		t.Fatal(err)
	}
	// SHA-256("admin-credential|"+pass) era il vecchio verifier offline:
	// il claim non deve più coincidere con quell'hash.
	plain := sha256.Sum256([]byte("admin-credential|" + pass))
	if claims.Credential == fmt.Sprintf("%x", plain) {
		t.Fatal("cv è ancora un hash nudo della password")
	}
	if claims.Credential == "" {
		t.Fatal("cv assente")
	}
}
