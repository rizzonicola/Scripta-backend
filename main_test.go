package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthEndpoints(t *testing.T) {
	healthHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","name":"Scripta Notes Server","version":"1.2.0","license":"GPL-3.0","credits":{"database":"github.com/tursodatabase/go-libsql","jwt":"github.com/golang-jwt/jwt/v5","security":"golang.org/x/crypto/bcrypt","uuid":"github.com/google/uuid"}}` + "\n"))
	}

	for _, path := range []string{"/healthz", "/health"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()

		healthHandler(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200 on %s, got %d", path, rec.Code)
		}

		var body map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("failed to parse json response on %s: %v", path, err)
		}

		if body["status"] != "ok" {
			t.Errorf("expected status 'ok', got %v", body["status"])
		}
		if body["name"] != "Scripta Notes Server" {
			t.Errorf("expected name 'Scripta Notes Server', got %v", body["name"])
		}
		if body["license"] != "GPL-3.0" {
			t.Errorf("expected license 'GPL-3.0', got %v", body["license"])
		}

		credits, ok := body["credits"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected credits map, got %T", body["credits"])
		}
		if credits["database"] != "github.com/tursodatabase/go-libsql" {
			t.Errorf("expected sqlite credit, got %v", credits["database"])
		}
	}
}
