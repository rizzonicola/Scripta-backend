package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// adminSessionSubject è il valore fisso di "sub" atteso in un cookie di
// sessione admin valido. Verificarlo esplicitamente (oltre alla firma)
// impedisce che un JWT di ALTRO tipo, anche se emesso con lo stesso
// meccanismo HS256, possa mai essere confuso per una sessione admin.
const adminSessionSubject = "admin-session-v1"

// AdminSessionManager firma e valida il cookie di sessione della dashboard
// /admin, sostituendo l'HTTP Basic Auth con un login form-based i cui campi
// sono riconoscibili dai password manager (vedi web/templates/admin_login.html).
//
// La chiave usata NON è mai jwtSecret "grezzo": è derivata con un dominio
// separato ("admin-session|" + jwtSecret) in modo che un token utente
// (auth.TokenManager, stesso algoritmo HS256) non possa mai essere accettato
// come sessione admin né viceversa, anche qualora in futuro le due
// implementazioni finissero per condividere più codice.
type AdminSessionManager struct {
	secret []byte
	ttl    time.Duration

	// revoked è la cache dei jti di sessione admin revocati (logout
	// server-side), con la scadenza naturale. store, se presente, la rende
	// persistente e la ricarica all'avvio.
	mu      sync.RWMutex
	revoked map[string]time.Time
	store   AdminSessionRevocationStore
}

// AdminSessionRevocationStore è la persistenza delle revoche delle sessioni
// admin. Riutilizza la tabella revoked_tokens (jti + scadenza): il jti è
// casuale e non può collidere con quello di un JWT utente.
type AdminSessionRevocationStore interface {
	SaveTokenRevocation(ctx context.Context, jti, userID string, expiresAt time.Time) error
	LoadActive(ctx context.Context, now time.Time) (users map[string]time.Time, tokens map[string]time.Time, err error)
}

const adminRevocationUserID = "admin-session"

func NewAdminSessionManager(jwtSecret string, ttl time.Duration) *AdminSessionManager {
	derived := sha256.Sum256([]byte("admin-session|" + jwtSecret))
	return &AdminSessionManager{
		secret:  derived[:],
		ttl:     ttl,
		revoked: make(map[string]time.Time),
	}
}

// AttachStore collega la persistenza delle revoche e carica le sessioni admin
// già revocate. Va chiamata prima di servire richieste.
func (m *AdminSessionManager) AttachStore(ctx context.Context, store AdminSessionRevocationStore) error {
	_, tokens, err := store.LoadActive(ctx, time.Now())
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store = store
	for jti, exp := range tokens {
		m.revoked[jti] = exp
	}
	return nil
}

type adminSessionClaims struct {
	jwt.RegisteredClaims
}

// GenerateSession crea un cookie di sessione firmato, valido per ttl.
func (m *AdminSessionManager) GenerateSession() (token string, expiresAt time.Time, err error) {
	expiresAt = time.Now().Add(m.ttl)
	claims := adminSessionClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.NewString(),
			Subject:   adminSessionSubject,
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.secret)
	if err != nil {
		return "", time.Time{}, err
	}
	return signed, expiresAt, nil
}

// Validate verifica firma, scadenza e subject del cookie di sessione.
func (m *AdminSessionManager) Validate(token string) error {
	claims := &adminSessionClaims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (interface{}, error) {
		// Solo HS256, lo stesso usato da GenerateSession: accettare qualunque
		// HMAC (HS384/HS512) amplierebbe inutilmente la superficie d'attacco.
		if t.Method != jwt.SigningMethodHS256 {
			return nil, errors.New("metodo di firma inatteso")
		}
		return m.secret, nil
	})
	if err != nil {
		return err
	}
	if !parsed.Valid || claims.Subject != adminSessionSubject {
		return errors.New("sessione admin non valida")
	}
	if claims.ID != "" {
		m.mu.RLock()
		exp, revoked := m.revoked[claims.ID]
		m.mu.RUnlock()
		if revoked && exp.After(time.Now()) {
			return errors.New("sessione admin revocata")
		}
	}
	return nil
}

// Revoke invalida la sessione admin identificata dal token (logout
// server-side). Un token non valido o già scaduto non è un errore: non c'è
// nulla da revocare.
func (m *AdminSessionManager) Revoke(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	claims := &adminSessionClaims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (interface{}, error) {
		if t.Method != jwt.SigningMethodHS256 {
			return nil, errors.New("metodo di firma inatteso")
		}
		return m.secret, nil
	})
	if err != nil || !parsed.Valid || claims.Subject != adminSessionSubject || claims.ID == "" {
		return nil
	}
	exp := time.Now().Add(m.ttl)
	if claims.ExpiresAt != nil {
		exp = claims.ExpiresAt.Time
	}
	m.mu.Lock()
	m.revoked[claims.ID] = exp
	store := m.store
	m.mu.Unlock()
	if store == nil {
		return nil
	}
	return store.SaveTokenRevocation(ctx, claims.ID, adminRevocationUserID, exp)
}
