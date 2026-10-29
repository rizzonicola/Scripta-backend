package main

import (
	"encoding/json"
	"testing"
)

// created_at è obbligatorio: un push che lo omette viene rifiutato con 422 e
// nessuna modifica viene applicata.
func TestSyncRejectsMissingCreatedAt(t *testing.T) {
	e := newEnv(t)
	tok := e.login("mario")

	missing := note("n-senza-created", "titolo", 3000, nil)
	delete(missing, "created_at")
	code, _, body := e.sync(tok, 0, nil, []map[string]any{missing})
	if code != 422 {
		t.Fatalf("atteso 422, ottenuto %d (%s)", code, body)
	}

	var r struct {
		Rejected   []map[string]string `json:"rejected"`
		ServerTime int64               `json:"server_time"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("risposta non JSON: %v (%s)", err, body)
	}
	if len(r.Rejected) != 1 || r.Rejected[0]["reason"] != "invalid_created_at" {
		t.Fatalf("rejected inatteso: %s", body)
	}
	if r.ServerTime != 0 {
		t.Fatalf("server_time non deve avanzare su 422: %s", body)
	}
}
