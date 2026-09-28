package auth

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MinUsernameLen = 3
	MaxUsernameLen = 64
)

// ErrInvalidUsername è restituito da NormalizeUsername.
var ErrInvalidUsername = errors.New("username non valido: 3-64 caratteri, senza spazi né caratteri di controllo")

// NormalizeUsername è la forma CANONICA con cui gli username vengono
// validati e salvati alla creazione: trim degli spazi ai bordi, minuscolo,
// lunghezza tra MinUsernameLen e MaxUsernameLen caratteri, nessuno spazio o
// carattere di controllo interno. Così "Mario", " mario " e "MARIO" sono lo
// stesso utente (e non tre account distinti / confondibili).
func NormalizeUsername(raw string) (string, error) {
	u := strings.ToLower(strings.TrimSpace(raw))
	n := utf8.RuneCountInString(u)
	if n < MinUsernameLen || n > MaxUsernameLen {
		return "", ErrInvalidUsername
	}
	for _, r := range u {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", ErrInvalidUsername
		}
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
