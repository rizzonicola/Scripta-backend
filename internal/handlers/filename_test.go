package handlers

import (
	"strings"
	"testing"
)

func TestSanitizeFilenameRemovesControlChars(t *testing.T) {
	got := sanitizeFilename("Riunione\r\nSet-Cookie: x")
	for _, r := range got {
		if r < 0x20 || r == 0x7f {
			t.Fatalf("carattere di controllo %q sopravvissuto in %q", r, got)
		}
	}
	if strings.ContainsAny(got, "\r\n\"") {
		t.Fatalf("nome file non sicuro per Content-Disposition: %q", got)
	}
}

func TestSanitizeFilenameRemovesPathSeparators(t *testing.T) {
	if got := sanitizeFilename("../etc/passwd"); strings.ContainsAny(got, `/\`) {
		t.Fatalf("separatore di percorso sopravvissuto: %q", got)
	}
}

func TestSanitizeFilenameFallbackAndUnicode(t *testing.T) {
	if got := sanitizeFilename(" \r\n "); got != "nota" {
		t.Fatalf("fallback = %q, atteso \"nota\"", got)
	}
	if got := sanitizeFilename("Riunione àèì"); got != "Riunione àèì" {
		t.Fatalf("caratteri Unicode alterati: %q", got)
	}
}
