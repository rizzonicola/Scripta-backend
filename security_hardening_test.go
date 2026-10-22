package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"notes-server/internal/auth"
	"notes-server/internal/handlers"
)

func TestConfigRejectsJWTTTLBeyondRevocationRetention(t *testing.T) {
	validEnv(t)
	t.Setenv("JWT_TTL", (auth.UserRevocationRetention + time.Minute).String())
	if _, err := loadConfig(); err == nil {
		t.Fatal("JWT_TTL oltre la retention delle revoche accettato")
	}

	t.Setenv("JWT_TTL", auth.UserRevocationRetention.String())
	if _, err := loadConfig(); err != nil {
		t.Fatalf("JWT_TTL pari alla retention rifiutato: %v", err)
	}
}

func TestTombstonePurgeCutoffIncludesSafetyMargin(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	want := now.Add(-handlers.TombstoneRetention - handlers.TombstonePurgeSafetyMargin).UnixMilli()
	if got := tombstonePurgeCutoff(now); got != want {
		t.Fatalf("cutoff = %d, atteso %d", got, want)
	}
}

func TestSyncRejectsOverBatchSizeBeforeDecoding(t *testing.T) {
	e := newEnv(t)
	token := e.login("mario")

	// Il preflight conta gli elementi grezzi: con 100001 elementi il decode
	// dei DTO non deve nemmeno partire. L'ultimo elemento non è un oggetto
	// valido: se il server decodificasse prima di contare, risponderebbe 400
	// invece di 413.
	folders := json.RawMessage("[" + strings.Repeat("{},", 100000) + "123]")
	code, body, _ := e.do("POST", "/api/v1/sync", token, map[string]any{
		"last_synced_at": 0,
		"folders":        folders,
		"notes":          []any{},
	}, nil)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, atteso 413: %s", code, body)
	}
}
