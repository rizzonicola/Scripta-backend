package auth

import (
	"errors"

	"golang.org/x/crypto/bcrypt"
)

const (
	MinPasswordLen = 8
	// MaxPasswordBytes: bcrypt considera solo i primi 72 byte. Rifiutare di
	// più evita sia il troncamento silenzioso sia hashing costoso su input
	// arbitrariamente grandi.
	MaxPasswordBytes = 72
)

var (
	ErrPasswordTooShort = errors.New("la password deve avere almeno 8 caratteri")
	ErrPasswordTooLong  = errors.New("la password non può superare i 72 byte")
)

// ValidatePassword applica i requisiti minimi di una nuova password.
func ValidatePassword(plain string) error {
	if len(plain) < MinPasswordLen {
		return ErrPasswordTooShort
	}
	if len(plain) > MaxPasswordBytes {
		return ErrPasswordTooLong
	}
	return nil
}

// HashPassword cifra una password in chiaro con bcrypt.
// La password in chiaro non viene mai persistita né loggata.
func HashPassword(plain string) (string, error) {
	if len(plain) > MaxPasswordBytes {
		return "", ErrPasswordTooLong
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// CheckPassword verifica una password in chiaro contro l'hash bcrypt salvato.
func CheckPassword(hash, plain string) bool {
	if len(plain) > MaxPasswordBytes {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}
