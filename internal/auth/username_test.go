package auth

import "testing"

func TestNormalizeUsernamePreservesCase(t *testing.T) {
	got, err := NormalizeUsername("  Mario.Rossi  ")
	if err != nil {
		t.Fatalf("NormalizeUsername: %v", err)
	}
	if got != "Mario.Rossi" {
		t.Fatalf("username = %q, atteso %q", got, "Mario.Rossi")
	}

	got, err = NormalizeUsername("Màrio")
	if err != nil {
		t.Fatalf("NormalizeUsername unicode: %v", err)
	}
	if got != "Màrio" {
		t.Fatalf("username unicode = %q, atteso %q", got, "Màrio")
	}
}

func TestNormalizeUsernameRejectsInvalid(t *testing.T) {
	for _, raw := range []string{"ab", "mario rossi", "mario\trossi", "mario\nrossi"} {
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
