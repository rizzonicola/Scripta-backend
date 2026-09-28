package auth

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Claims personalizzate incluse nel token JWT. Il claim standard "jti"
// (RegisteredClaims.ID) identifica il singolo token e ne permette la revoca
// puntuale (logout).
type Claims struct {
	UserID   string `json:"uid"`
	Username string `json:"username"`
	jwt.RegisteredClaims
}

// RevocationStore è la persistenza delle revoche (implementata da
// db.RevocationRepo). Definita qui, lato consumatore, per non far dipendere
// il package auth dal package db.
type RevocationStore interface {
	SaveUserRevocation(ctx context.Context, userID string, at time.Time) error
	SaveTokenRevocation(ctx context.Context, jti, userID string, expiresAt time.Time) error
	LoadActive(ctx context.Context, now time.Time) (users map[string]time.Time, tokens map[string]time.Time, err error)
	PurgeExpired(ctx context.Context, now time.Time, userRevocationRetention time.Duration) error
}

// userRevocationRetention: per quanto tempo si conserva una revoca "per
// utente". Volutamente molto più larga di qualunque TTL sensato dei token,
// anche se JWT_TTL venisse abbassato dopo l'emissione di token più longevi.
const userRevocationRetention = 30 * 24 * time.Hour

// TokenManager genera e valida i JWT (HS256) e gestisce la revoca.
//
// Revoca: la lookup a runtime resta un accesso O(1) a mappe in RAM (nessuna
// query per richiesta autenticata), ma le mappe sono la CACHE di una
// tabella persistente: ogni revoca viene scritta su DB (RevocationStore) e
// tutte le revoche ancora rilevanti vengono ricaricate all'avvio (vedi
// AttachStore). In questo modo un logout / reset password / cancellazione
// utente resta valido anche dopo il riavvio del processo.
type TokenManager struct {
	secret []byte
	ttl    time.Duration

	store RevocationStore

	mu           sync.RWMutex
	userRevoked  map[string]time.Time // userID -> istante di revoca (tutti i token emessi prima/al momento)
	tokenRevoked map[string]time.Time // jti -> scadenza naturale del token
}

func NewTokenManager(secret string, ttl time.Duration) *TokenManager {
	return &TokenManager{
		secret:       []byte(secret),
		ttl:          ttl,
		userRevoked:  make(map[string]time.Time),
		tokenRevoked: make(map[string]time.Time),
	}
}

// AttachStore collega la persistenza e carica in RAM le revoche ancora
// attive. Da chiamare una volta all'avvio, prima di servire richieste.
func (tm *TokenManager) AttachStore(ctx context.Context, store RevocationStore) error {
	users, tokens, err := store.LoadActive(ctx, time.Now())
	if err != nil {
		return err
	}
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.store = store
	for k, v := range users {
		tm.userRevoked[k] = v
	}
	for k, v := range tokens {
		tm.tokenRevoked[k] = v
	}
	return nil
}

// RevokeUser invalida ogni JWT già emesso per userID (anche se ancora entro
// la scadenza naturale). Da usare su reset password e cancellazione utente.
// L'invalidazione in RAM è immediata anche se la scrittura su DB fallisce; in
// quel caso l'errore viene restituito perché il chiamante possa segnalarlo.
func (tm *TokenManager) RevokeUser(ctx context.Context, userID string) error {
	now := time.Now()
	tm.mu.Lock()
	tm.userRevoked[userID] = now
	store := tm.store
	tm.mu.Unlock()
	if store == nil {
		return nil
	}
	return store.SaveUserRevocation(ctx, userID, now)
}

// RevokeToken invalida il singolo token (logout esplicito) fino alla sua
// scadenza naturale.
func (tm *TokenManager) RevokeToken(ctx context.Context, c *Claims) error {
	if c == nil || c.ID == "" {
		return errors.New("token senza jti: revoca puntuale impossibile")
	}
	exp := time.Now().Add(tm.ttl)
	if c.ExpiresAt != nil {
		exp = c.ExpiresAt.Time
	}
	tm.mu.Lock()
	tm.tokenRevoked[c.ID] = exp
	store := tm.store
	tm.mu.Unlock()
	if store == nil {
		return nil
	}
	return store.SaveTokenRevocation(ctx, c.ID, c.UserID, exp)
}

// IsRevoked indica se il token è stato revocato, per singolo jti oppure per
// utente (emesso prima o nello stesso istante dell'ultima revoca utente).
func (tm *TokenManager) IsRevoked(c *Claims) bool {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	if c.ID != "" {
		if _, ok := tm.tokenRevoked[c.ID]; ok {
			return true
		}
	}
	if t, ok := tm.userRevoked[c.UserID]; ok {
		if c.IssuedAt == nil || !c.IssuedAt.Time.After(t) {
			return true
		}
	}
	return false
}

// PurgeExpired rimuove dalla cache e dal DB le revoche ormai irrilevanti.
func (tm *TokenManager) PurgeExpired(ctx context.Context) {
	now := time.Now()
	tm.mu.Lock()
	for jti, exp := range tm.tokenRevoked {
		if !exp.After(now) {
			delete(tm.tokenRevoked, jti)
		}
	}
	for uid, at := range tm.userRevoked {
		if at.Before(now.Add(-userRevocationRetention)) {
			delete(tm.userRevoked, uid)
		}
	}
	store := tm.store
	tm.mu.Unlock()
	if store != nil {
		if err := store.PurgeExpired(ctx, now, userRevocationRetention); err != nil {
			log.Printf("purge revoche token fallito: %v", err)
		}
	}
}

// GenerateToken crea un JWT firmato per l'utente indicato.
func (tm *TokenManager) GenerateToken(userID, username string) (string, int64, error) {
	now := time.Now()
	expiresAt := now.Add(tm.ttl)
	claims := Claims{
		UserID:   userID,
		Username: username,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.NewString(),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(now),
			Subject:   userID,
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(tm.secret)
	if err != nil {
		return "", 0, err
	}
	return signed, expiresAt.Unix(), nil
}

// ParseToken valida un JWT (solo HS256, con scadenza obbligatoria) e ne
// estrae le claims. NON controlla la revoca: vedi IsRevoked.
func (tm *TokenManager) ParseToken(tokenStr string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
		return tm.secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil {
		return nil, err
	}
	if !token.Valid {
		return nil, errors.New("token non valido")
	}
	if claims.ExpiresAt == nil || claims.UserID == "" {
		return nil, errors.New("token privo di scadenza o di uid")
	}
	return claims, nil
}
