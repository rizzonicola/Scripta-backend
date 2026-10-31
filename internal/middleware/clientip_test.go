package middleware

import (
	"net/http/httptest"
	"testing"
)

// clientIPFor costruisce una richiesta con peer e header indicati e restituisce
// l'IP calcolato dal resolver.
func clientIPFor(t *testing.T, trusted []string, header, remote string, hdrs map[string]string) string {
	t.Helper()
	res, err := NewClientIPResolver(trusted, header)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/v1/auth/login", nil)
	r.RemoteAddr = remote
	for k, v := range hdrs {
		r.Header.Set(k, v)
	}
	return res.ClientIP(r)
}

func TestClientIPIgnoresHeadersFromUntrustedPeer(t *testing.T) {
	got := clientIPFor(t, nil, "cf-connecting-ip", "192.0.2.10:1234", map[string]string{
		"Cf-Connecting-IP": "203.0.113.7",
		"X-Forwarded-For":  "198.51.100.9",
	})
	if got != "192.0.2.10" {
		t.Fatalf("header creduto da peer non fidato: %q", got)
	}
}

func TestClientIPUsesOnlyConfiguredHeader(t *testing.T) {
	hdrs := map[string]string{
		"Cf-Connecting-IP": "203.0.113.7",
		"X-Forwarded-For":  "198.51.100.9, 127.0.0.1",
	}
	if got := clientIPFor(t, []string{"127.0.0.1"}, "cf-connecting-ip", "127.0.0.1:1234", hdrs); got != "203.0.113.7" {
		t.Fatalf("cf-connecting-ip: %q", got)
	}
	if got := clientIPFor(t, []string{"127.0.0.1"}, "x-forwarded-for", "127.0.0.1:1234", hdrs); got != "198.51.100.9" {
		t.Fatalf("x-forwarded-for: %q", got)
	}
	if got := clientIPFor(t, []string{"127.0.0.1"}, "none", "127.0.0.1:1234", hdrs); got != "127.0.0.1" {
		t.Fatalf("none: %q", got)
	}
}

func TestClientIPRejectsMalformedHeaderAndInvalidConfig(t *testing.T) {
	if got := clientIPFor(t, []string{"127.0.0.1"}, "cf-connecting-ip", "127.0.0.1:1234", map[string]string{
		"Cf-Connecting-IP": "not-an-ip",
	}); got != "127.0.0.1" {
		t.Fatalf("header malformato accettato: %q", got)
	}
	if _, err := NewClientIPResolver(nil, "x-real-ip"); err == nil {
		t.Fatal("header non supportato accettato")
	}
}
