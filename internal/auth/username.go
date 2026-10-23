package auth

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	MinUsernameLen = 3
	MaxUsernameLen = 64
)

var usernameChars = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ErrInvalidUsername è restituito da NormalizeUsername.
var ErrInvalidUsername = errors.New("username non valido: 3-64 caratteri tra lettere ASCII, numeri, punto, trattino e underscore")

// NormalizeUsername valida l'username e ne conserva il case originale: trim
// degli spazi ai bordi, lunghezza tra MinUsernameLen e MaxUsernameLen
// caratteri e solo lettere ASCII, numeri, punto, trattino e underscore.
// L'unicità e la lookup restano case-insensitive, così "Mario", " mario " e
// "MARIO" sono lo stesso utente ma il nome mostrato mantiene le maiuscole
// scelte alla creazione.
func NormalizeUsername(raw string) (string, error) {
	u := strings.TrimSpace(raw)
	n := utf8.RuneCountInString(u)
	if n < MinUsernameLen || n > MaxUsernameLen || !usernameChars.MatchString(u) {
		return "", ErrInvalidUsername
	}
	return u, nil
}

// CanonicalUsername è la versione "tollerante" usata dal LOGIN: applica solo
// trim + minuscolo, senza rifiutare nulla, così gli utenti creati prima
// dell'introduzione delle regole di normalizzazione continuano a poter
// accedere (la lookup nel DB è comunque case-insensitive).
func CanonicalUsername(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}
