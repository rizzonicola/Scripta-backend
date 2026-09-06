package auth

import "golang.org/x/crypto/bcrypt"

// HashPassword cifra una password in chiaro con bcrypt.
// La password in chiaro non viene mai persistita né loggata.
func HashPassword(plain string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// CheckPassword verifica una password in chiaro contro l'hash bcrypt salvato.
func CheckPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}
