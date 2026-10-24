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
// L'unicità e la lookup sono case-sensitive: "Mario" e "mario" sono due
// utenti distinti, con il case scelto alla creazione conservato e mostrato.
func NormalizeUsername(raw string) (string, error) {
	u := strings.TrimSpace(raw)
	n := utf8.RuneCountInString(u)
	if n < MinUsernameLen || n > MaxUsernameLen || !usernameChars.MatchString(u) {
		return "", ErrInvalidUsername
	}
	return u, nil
}

// LoginUsername prepara lo username ricevuto dal LOGIN: applica solo il trim
// degli spazi ai bordi, senza cambiare il case e senza rifiutare nulla. Il
// case è significativo, quindi "Mario" e "mario" sono account diversi; il
// trim resta tollerante per gli utenti creati prima delle regole attuali.
func LoginUsername(raw string) string {
	return strings.TrimSpace(raw)
}
