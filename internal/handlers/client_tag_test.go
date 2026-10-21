package handlers

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientTagIsSafeForLogs(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "-"},
		{"scripta/1.0.0", "scripta/1.0.0"},
		{"scripta/1.0.0\nFALSO: riga di log", "scripta/1.0.0FALSO: riga di log"},
		{"\x00\x1b[31mrosso", "[31mrosso"},
		{"è→😀", "-"}, // solo ASCII stampabile: il resto viene scartato
		{strings.Repeat("a", 200), strings.Repeat("a", 64)},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "/api/v1/sync", nil)
		if c.in != "" {
			r.Header.Set("X-Scripta-Client", c.in)
		}
		if got := clientTag(r); got != c.want {
			t.Errorf("clientTag(%q) = %q, atteso %q", c.in, got, c.want)
		}
	}
}
