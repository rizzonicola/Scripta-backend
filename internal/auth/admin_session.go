package auth

import (
	"crypto/sha256"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
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
}

func NewAdminSessionManager(jwtSecret string, ttl time.Duration) *AdminSessionManager {
	derived := sha256.Sum256([]byte("admin-session|" + jwtSecret))
	return &AdminSessionManager{secret: derived[:], ttl: ttl}
}

type adminSessionClaims struct {
	jwt.RegisteredClaims
}

// GenerateSession crea un cookie di sessione firmato, valido per ttl.
func (m *AdminSessionManager) GenerateSession() (token string, expiresAt time.Time, err error) {
	expiresAt = time.Now().Add(m.ttl)
	claims := adminSessionClaims{
		RegisteredClaims: jwt.RegisteredClaims{
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
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
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
	return nil
}
