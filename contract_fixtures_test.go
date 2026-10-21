package main

// Y-15: fixture JSON del contratto di sync, condivise con l'app.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"notes-server/internal/models"
)

func contractFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "contract", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return raw
}

func contractDecodeStrict(t *testing.T, name string, dst any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(contractFixture(t, name)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		t.Fatalf("fixture %s non conforme ai DTO del server: %v", name, err)
	}
}

// Una fixture con un campo che il server non conosce (o con un tipo sbagliato)
// non è più il contratto reale.
func TestContractFixturesMatchServerDTOs(t *testing.T) {
	contractDecodeStrict(t, "sync_request.json", &models.SyncRequest{})
	contractDecodeStrict(t, "sync_request_422.json", &models.SyncRequest{})
	contractDecodeStrict(t, "sync_response_200.json", &models.SyncResponse{})
	contractDecodeStrict(t, "sync_response_full_resync.json", &models.SyncResponse{})
	contractDecodeStrict(t, "sync_response_422.json", &models.SyncRejectedResponse{})
	contractDecodeStrict(t, "sync_response_503.json", &struct {
		Error string `json:"error"`
	}{})
}

func contractNormalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if n, ok := val.(float64); ok && k == "server_time" && n > 0 {
				out[k] = "<server_time>"
				continue
			}
			out[k] = contractNormalize(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = contractNormalize(val)
		}
		return out
	default:
		return v
	}
}

func contractAssertSame(t *testing.T, name string, got []byte) {
	t.Helper()
	var want, have any
	if err := json.Unmarshal(contractFixture(t, name), &want); err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	if err := json.Unmarshal(got, &have); err != nil {
		t.Fatalf("risposta non JSON: %v (%s)", err, got)
	}
	if !reflect.DeepEqual(contractNormalize(want), contractNormalize(have)) {
		w, _ := json.MarshalIndent(contractNormalize(want), "", "  ")
		g, _ := json.MarshalIndent(contractNormalize(have), "", "  ")
		t.Fatalf("la risposta del server non coincide con %s\natteso:\n%s\nottenuto:\n%s", name, w, g)
	}
}

// Le risposte reali del server coincidono con le fixture (a meno di
// server_time): 200 dopo il push della richiesta, full_resync, 422.
func TestContractFixturesMatchRealServerResponses(t *testing.T) {
	e := newEnv(t)
	tok := e.login("mario")

	// 200: push della richiesta di fixture.
	code, body, h := e.do("POST", "/api/v1/sync", tok, json.RawMessage(contractFixture(t, "sync_request.json")), nil)
	if code != 200 {
		t.Fatalf("sync: %d %s", code, body)
	}
	if h.Get("X-Scripta-Proto") != "2" {
		t.Fatalf("X-Scripta-Proto = %q", h.Get("X-Scripta-Proto"))
	}
	contractAssertSame(t, "sync_response_200.json", body)

	// full_resync: cursore più vecchio della retention dei tombstone.
	code, body, _ = e.do("POST", "/api/v1/sync", tok, map[string]any{"last_synced_at": 1, "folders": []any{}, "notes": []any{}}, nil)
	if code != 200 {
		t.Fatalf("full resync: %d %s", code, body)
	}
	contractAssertSame(t, "sync_response_full_resync.json", body)

	// 422: riferimenti non validi.
	code, body, _ = e.do("POST", "/api/v1/sync", tok, json.RawMessage(contractFixture(t, "sync_request_422.json")), nil)
	if code != 422 {
		t.Fatalf("attesi 422, ottenuto %d (%s)", code, body)
	}
	contractAssertSame(t, "sync_response_422.json", body)
}
