package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"

	"github.com/google/uuid"
)

// RefreshTokenBytes è la lunghezza del segreto opaco del refresh token. Il
// client riceve la forma base64url; il server conserva solo l'hash SHA-256.
const RefreshTokenBytes = 32

// NewRefreshToken genera un refresh token opaco e il suo hash da persistere.
// Il valore in chiaro non viene mai salvato: un dump del database non basta
// a impersonare un utente.
func NewRefreshToken() (raw, hash string, err error) {
	buf := make([]byte, RefreshTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	raw = base64.RawURLEncoding.EncodeToString(buf)
	return raw, HashRefreshToken(raw), nil
}

// HashRefreshToken calcola l'hash con cui cercare un refresh token in DB.
func HashRefreshToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// NewRefreshFamilyID identifica la catena di refresh token nata da un login.
// Ogni dispositivo ha la sua famiglia: il logout di uno non tocca gli altri.
func NewRefreshFamilyID() string {
	return uuid.NewString()
}
