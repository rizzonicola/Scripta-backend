package main

// Y-02: ogni risposta JSON dichiara charset=utf-8 e il testo non-ASCII
// attraversa push e pull senza alterazioni.

import (
	"encoding/json"
	"testing"
	"time"
)

const jsonUTF8 = "application/json; charset=utf-8"

func TestY02JSONResponsesDeclareUTF8(t *testing.T) {
	e := newEnv(t)
	tok := e.login("mario")

	checks := []struct {
		name   string
		method string
		path   string
		token  string
		body   any
		status int
	}{
		{"healthz", "GET", "/healthz", "", nil, 200},
		{"health", "GET", "/health", "", nil, 200},
		{"login", "POST", "/api/v1/auth/login", "", map[string]string{"username": "mario", "password": "password-di-prova"}, 200},
		{"sync 200", "POST", "/api/v1/sync", tok, map[string]any{"last_synced_at": 0, "folders": []any{}, "notes": []any{}}, 200},
		{"sync 401 (middleware)", "POST", "/api/v1/sync", "", map[string]any{"last_synced_at": 0}, 401},
		{"sync 405", "GET", "/api/v1/sync", tok, nil, 405},
		{"settings 405 (main.go)", "DELETE", "/api/v1/user/settings", tok, nil, 405},
		{"admin login 405 (main.go)", "PUT", "/admin/login", "", nil, 405},
	}
	for _, c := range checks {
		code, _, h := e.do(c.method, c.path, c.token, c.body, nil)
		if code != c.status {
			t.Errorf("%s: status %d, atteso %d", c.name, code, c.status)
			continue
		}
		if got := h.Get("Content-Type"); got != jsonUTF8 {
			t.Errorf("%s: Content-Type %q, atteso %q", c.name, got, jsonUTF8)
		}
	}

	// Anche il 422 della sync.
	code, _, h := e.do("POST", "/api/v1/sync", tok, map[string]any{"last_synced_at": 0, "folders": []any{},
		"notes": []any{note("n1", "x", time.Now().UnixMilli(), "non-esiste")}}, nil)
	if code != 422 || h.Get("Content-Type") != jsonUTF8 {
		t.Errorf("sync 422: status %d, Content-Type %q", code, h.Get("Content-Type"))
	}
}

// Il titolo con accenti, freccia, emoji e caratteri che Go potrebbe
// scappare (<, >, &) torna identico dopo push e pull.
func TestY02UTF8RoundTrip(t *testing.T) {
	e := newEnv(t)
	tok := e.login("mario")

	const title = "è → 😀 <>&"
	const content = "Perché → già: «ciao» 😀\nriga 2 — fine"
	n := note("n1", title, time.Now().Add(-time.Hour).UnixMilli(), nil)
	n["content"] = content
	if code, _, b := e.sync(tok, 0, nil, []map[string]any{n}); code != 200 {
		t.Fatalf("push: %d %s", code, b)
	}

	code, _, body := e.sync(tok, 0, nil, nil)
	if code != 200 {
		t.Fatalf("pull: %d %s", code, body)
	}
	var r struct {
		Notes []struct {
			Title   string `json:"title"`
			Content string `json:"content"`
		} `json:"notes"`
	}
	if err := json.Unmarshal(body, &r); err != nil || len(r.Notes) != 1 {
		t.Fatalf("risposta inattesa: %v %s", err, body)
	}
	if r.Notes[0].Title != title || r.Notes[0].Content != content {
		t.Fatalf("testo alterato: %q / %q", r.Notes[0].Title, r.Notes[0].Content)
	}
}
