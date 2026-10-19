package main

import (
	"regexp"
	"testing"
)

// Prova end-to-end sul router vero (stessa catena di produzione): Server-Timing
// compare sulle API autenticate, non su /healthz e non sul login.
func TestServerTiming_SulRouterVero(t *testing.T) {
	e := newEnv(t)
	tok := e.login("mario")
	re := regexp.MustCompile(`^total;dur=[0-9]+\.[0-9], recv;dur=[0-9]+\.[0-9]$`)

	_, _, h := e.do("POST", "/api/v1/sync", tok, map[string]any{"last_synced_at": 0, "folders": []map[string]any{}, "notes": []map[string]any{}}, nil)
	if v := h.Get("Server-Timing"); !re.MatchString(v) {
		t.Errorf("/api/v1/sync: Server-Timing mancante o malformato: %q", v)
	}

	_, _, h = e.do("GET", "/api/v1/user/settings", tok, nil, map[string]string{"Accept-Encoding": "gzip"})
	if v := h.Get("Server-Timing"); !re.MatchString(v) {
		t.Errorf("/api/v1/user/settings (gzip): Server-Timing mancante o malformato: %q", v)
	}

	// Anche sulle risposte di errore (401 dal middleware JWT, prima dell'handler).
	_, _, h = e.do("POST", "/api/v1/sync", "", nil, nil)
	if v := h.Get("Server-Timing"); !re.MatchString(v) {
		t.Errorf("401: Server-Timing mancante: %q", v)
	}

	_, _, h = e.do("GET", "/healthz", "", nil, nil)
	if v := h.Get("Server-Timing"); v != "" {
		t.Errorf("/healthz non deve avere Server-Timing: %q", v)
	}

	_, _, h = e.do("POST", "/api/v1/auth/login", "", map[string]string{"username": "mario", "password": "password-di-prova"}, nil)
	if v := h.Get("Server-Timing"); v != "" {
		t.Errorf("/api/v1/auth/login non deve avere Server-Timing: %q", v)
	}
}
