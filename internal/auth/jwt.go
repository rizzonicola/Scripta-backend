package auth

import (
	"errors"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims personalizzate incluse nel token JWT.
type Claims struct {
	UserID   string `json:"uid"`
	Username string `json:"username"`
	jwt.RegisteredClaims
}

// TokenManager genera e valida i JWT usando una chiave segreta condivisa (HS256).
//
// La revoca (Revoke/IsRevoked) è pensata per restare compatibile con la
// natura stateless di JWT: NON viene fatta alcuna query a DB ad ogni
// richiesta autenticata. Si tiene invece in RAM un piccolissimo set di
// "userID revocati di recente" (solo per gli eventi rari che la
// richiedono: reset password, cancellazione utente), consultato con un
// semplice lookup O(1) dal middleware. La cardinalità della mappa è
// limitata dal TTL stesso dei token: una entry più vecchia del TTL
// significa che ogni token emesso prima di quella revoca sarebbe comunque
// già scaduto naturalmente, quindi può essere rimossa senza perdere
// protezione.
type TokenManager struct {
	secret []byte
	ttl    time.Duration

	revokedMu sync.Mutex
	revoked   map[string]time.Time // userID -> istante di revoca
}

func NewTokenManager(secret string, ttl time.Duration) *TokenManager {
	return &TokenManager{
		secret:  []byte(secret),
		ttl:     ttl,
		revoked: make(map[string]time.Time),
	}
}

// Revoke invalida immediatamente ogni JWT già emesso per userID (anche se
// ancora entro la scadenza naturale). Va chiamata sui soli eventi che
// richiedono revoca istantanea: reset password e cancellazione utente.
func (tm *TokenManager) Revoke(userID string) {
	tm.revokedMu.Lock()
	defer tm.revokedMu.Unlock()
	tm.revoked[userID] = time.Now()
	tm.pruneRevocationsLocked()
}

// IsRevoked indica se un token con la data di emissione (IssuedAt) indicata
// va considerato invalido perché emesso prima (o nello stesso istante) di
// un evento di revoca per quell'utente.
func (tm *TokenManager) IsRevoked(userID string, issuedAt time.Time) bool {
	tm.revokedMu.Lock()
	defer tm.revokedMu.Unlock()
	t, ok := tm.revoked[userID]
	return ok && !issuedAt.After(t)
}

// pruneRevocationsLocked rimuove le revoche ormai più vecchie del TTL dei
// token: qualunque JWT emesso prima di quel taglio sarebbe comunque già
// scaduto per conto proprio, quindi tenerne traccia non serve più. Va
// chiamata con revokedMu già acquisito.
func (tm *TokenManager) pruneRevocationsLocked() {
	cutoff := time.Now().Add(-tm.ttl)
	for userID, t := range tm.revoked {
		if t.Before(cutoff) {
			delete(tm.revoked, userID)
		}
	}
}

// GenerateToken crea un JWT firmato per l'utente indicato.
func (tm *TokenManager) GenerateToken(userID, username string) (string, int64, error) {
	expiresAt := time.Now().Add(tm.ttl)
	claims := Claims{
		UserID:   userID,
		Username: username,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
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

// ParseToken valida un JWT e ne estrae le claims.
func (tm *TokenManager) ParseToken(tokenStr string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("metodo di firma inatteso")
		}
		return tm.secret, nil
	})
	if err != nil {
		return nil, err
	}
	if !token.Valid {
		return nil, errors.New("token non valido")
	}
	return claims, nil
}
