package auth

import (
	"strings"
	"testing"
)

func TestNormalizeUsernamePreservesCase(t *testing.T) {
	for _, raw := range []string{"  Mario.Rossi  ", "mario_2-test", "MARIO"} {
		got, err := NormalizeUsername(raw)
		if err != nil {
			t.Fatalf("NormalizeUsername(%q): %v", raw, err)
		}
		want := strings.TrimSpace(raw)
		if got != want {
			t.Fatalf("username = %q, atteso %q", got, want)
		}
	}
}

func TestNormalizeUsernameRejectsInvalid(t *testing.T) {
	for _, raw := range []string{
		"ab",
		"mario rossi",
		"mario\trossi",
		"mario\nrossi",
		"Màrio",
		"NICCOLÒ",
		"mario@example",
		"mario/rossi",
		"mario+tag",
		"mario!",
		"' OR 1=1 --",
		"mario;DROP TABLE users",
	} {
		if _, err := NormalizeUsername(raw); err == nil {
			t.Fatalf("NormalizeUsername(%q) accettato", raw)
		}
	}
}

func TestCanonicalUsernameIsCaseInsensitive(t *testing.T) {
	for _, raw := range []string{"  MARIO  ", "mario", "Mario"} {
		if got := CanonicalUsername(raw); got != "mario" {
			t.Fatalf("CanonicalUsername(%q) = %q, atteso %q", raw, got, "mario")
		}
	}
}
