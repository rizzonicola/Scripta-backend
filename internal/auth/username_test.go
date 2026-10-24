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

func TestLoginUsernamePreservesCase(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{"  MARIO  ", "MARIO"},
		{"mario", "mario"},
		{"Mario", "Mario"},
	} {
		if got := LoginUsername(tc.raw); got != tc.want {
			t.Fatalf("LoginUsername(%q) = %q, atteso %q", tc.raw, got, tc.want)
		}
	}
}
