package main

// Y-14: `caps` e `X-Scripta-Proto` sono additivi. Un client che non li conosce
// non vede differenze; un client nuovo li legge per decidere le funzioni
// opzionali.

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestY14HealthzAnnouncesCaps(t *testing.T) {
	e := newEnv(t)
	code, body, h := e.do("GET", "/healthz", "", nil, nil)
	if code != 200 {
		t.Fatalf("healthz: %d", code)
	}
	var r struct {
		Status  string `json:"status"`
		Version string `json:"version"`
		Caps    struct {
			Proto int `json:"proto"`
		} `json:"caps"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("healthz non è JSON: %v (%s)", err, body)
	}
	if r.Status != "ok" || r.Version != serverVersion {
		t.Fatalf("campi storici alterati: %+v", r)
	}
	if r.Caps.Proto != 2 {
		t.Fatalf("caps.proto = %d, atteso 2", r.Caps.Proto)
	}
	if got := h.Get("X-Scripta-Proto"); got != "2" {
		t.Fatalf("X-Scripta-Proto = %q, atteso \"2\"", got)
	}
}

func TestY14SyncSuccessCarriesCapsAndHeader(t *testing.T) {
	e := newEnv(t)
	tok := e.login("mario")
	code, body, h := e.do("POST", "/api/v1/sync", tok, map[string]any{"last_synced_at": 0, "folders": []any{}, "notes": []any{}}, nil)
	if code != 200 {
		t.Fatalf("sync: %d %s", code, body)
	}
	var r map[string]json.RawMessage
	_ = json.Unmarshal(body, &r)
	for _, k := range []string{"server_time", "full_resync", "folders", "notes", "caps"} {
		if _, ok := r[k]; !ok {
			t.Fatalf("campo %q assente nella risposta: %s", k, body)
		}
	}
	var caps struct {
		Proto int `json:"proto"`
	}
	_ = json.Unmarshal(r["caps"], &caps)
	if caps.Proto != 2 || h.Get("X-Scripta-Proto") != "2" {
		t.Fatalf("caps %+v, header %q", caps, h.Get("X-Scripta-Proto"))
	}
}

// Il 422 e il 401 NON portano `caps`: il contratto dice «risposta 200 della sync».
func TestY14ErrorResponsesHaveNoCaps(t *testing.T) {
	e := newEnv(t)
	tok := e.login("mario")
	_, body422, _ := e.do("POST", "/api/v1/sync", tok, map[string]any{"last_synced_at": 0, "folders": []any{},
		"notes": []any{note("n1", "x", time.Now().UnixMilli(), "non-esiste")}}, nil)
	_, body401, _ := e.do("POST", "/api/v1/sync", "", map[string]any{"last_synced_at": 0}, nil)
	for name, b := range map[string][]byte{"422": body422, "401": body401} {
		var m map[string]json.RawMessage
		_ = json.Unmarshal(b, &m)
		if _, ok := m["caps"]; ok {
			t.Errorf("%s: `caps` non previsto: %s", name, b)
		}
	}
}

// X-Scripta-Client serve solo ai log: la risposta non cambia.
func TestY14ClientHeaderDoesNotChangeBehavior(t *testing.T) {
	e := newEnv(t)
	tok := e.login("mario")
	n := note("n1", "titolo", time.Now().Add(-time.Hour).UnixMilli(), nil)
	if code, _, b := e.sync(tok, 0, nil, []map[string]any{n}); code != 200 {
		t.Fatalf("push: %d %s", code, b)
	}

	pull := func(hdr map[string]string) (json.RawMessage, json.RawMessage) {
		code, body, _ := e.do("POST", "/api/v1/sync", tok, map[string]any{"last_synced_at": 0, "folders": []any{}, "notes": []any{}}, hdr)
		if code != 200 {
			t.Fatalf("pull: %d %s", code, body)
		}
		var m map[string]json.RawMessage
		_ = json.Unmarshal(body, &m)
		return m["folders"], m["notes"]
	}
	f1, n1 := pull(nil)
	f2, n2 := pull(map[string]string{"X-Scripta-Client": "scripta/1.0.0"})
	if !reflect.DeepEqual(f1, f2) || !reflect.DeepEqual(n1, n2) {
		t.Fatalf("la risposta cambia con X-Scripta-Client:\n%s\n%s", n1, n2)
	}
}
