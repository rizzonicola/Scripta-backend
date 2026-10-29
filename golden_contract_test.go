package main

// Golden contract: richieste e risposte del server corrente, registrate una
// volta e poi riconfrontate a ogni test.
//
// Il confronto è «contiene»: ogni campo atteso deve esserci con lo stesso
// valore; campi NUOVI sono ammessi. Un campo tolto, rinominato o con un
// valore diverso fa fallire il test.
//
// PROCEDURA:
//
//	UPDATE_GOLDEN=1 go test -run TestGoldenContract -count=1 .
//
// I JSON generati in testdata/golden_contract/ vanno committati insieme al
// codice. Lo script scripts/record_golden_contract.sh automatizza la
// rigenerazione.
//
// Non coperto: il 503 («database temporaneamente occupato»), perché richiede
// una contesa reale su SQLite e non è riproducibile in modo deterministico;
// la sua forma è nelle fixture di contratto (testdata/contract).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"notes-server/internal/auth"
	"notes-server/internal/db"
)

// goldenRecord è il formato su disco di un singolo scambio.
type goldenRecord struct {
	Request     any    `json:"request"`
	Status      int    `json:"status"`
	ContentType string `json:"content_type"` // solo il media type, senza parametri
	RetryAfter  string `json:"retry_after,omitempty"`
	Response    any    `json:"response"`
}

// goldenEnv avvia un server su un DB temporaneo con l'utente «mario» e
// restituisce l'URL e un token valido. syncMaxBody=0 lascia il default.
func goldenEnv(t *testing.T, syncMaxBody int64) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "golden.db")
	sqlDB, err := db.Open(path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	cfg := config{
		DBPath: path, JWTSecret: testSecret, AdminUser: "root", AdminPass: "a-very-long-admin-pass",
		JWTTTL: time.Hour, AdminSessionTTL: time.Hour, SyncMaxBody: syncMaxBody,
	}
	tm := auth.NewTokenManager(cfg.JWTSecret, cfg.JWTTTL)
	if err := tm.AttachStore(context.Background(), db.NewRevocationRepo(sqlDB)); err != nil {
		t.Fatalf("attach store: %v", err)
	}
	h, err := buildRouter(cfg, sqlDB, tm)
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	hash, _ := auth.HashPassword("password-di-prova")
	if _, err := db.NewUsersRepo(sqlDB).Create(context.Background(), "mario", hash); err != nil {
		t.Fatalf("create user: %v", err)
	}

	loginBody, _ := json.Marshal(map[string]string{"username": "mario", "password": "password-di-prova"})
	resp, err := http.Post(srv.URL+"/api/v1/auth/login", "application/json", bytes.NewReader(loginBody))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	var lr struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&lr)
	if lr.Token == "" {
		t.Fatalf("login senza token (status %d)", resp.StatusCode)
	}
	return srv.URL, lr.Token
}

// goldenNormalize sostituisce i soli valori non deterministici (lo stamp
// del server) con un segnaposto stabile.
func goldenNormalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if k == "server_time" {
				if n, ok := val.(float64); ok && n > 0 {
					out[k] = "<server_time>"
					continue
				}
			}
			out[k] = goldenNormalize(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = goldenNormalize(val)
		}
		return out
	default:
		return v
	}
}

// goldenContains è vero se `got` contiene tutto ciò che c'è in `want`: gli
// oggetti possono avere chiavi in più, gli array devono avere la stessa
// lunghezza (e ogni elemento è confrontato allo stesso modo).
func goldenContains(want, got any) bool {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return false
		}
		for k, wv := range w {
			gv, present := g[k]
			if !present || !goldenContains(wv, gv) {
				return false
			}
		}
		return true
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			if !goldenContains(w[i], g[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(want, got)
	}
}

type goldenStep struct {
	name   string
	method string
	path   string
	auth   bool
	body   any    // serializzato in JSON, se raw è vuoto
	raw    string // corpo grezzo (JSON volutamente non valido)
}

func goldenDo(t *testing.T, base, token string, s goldenStep) goldenRecord {
	t.Helper()
	var rd io.Reader
	switch {
	case s.raw != "":
		rd = bytes.NewReader([]byte(s.raw))
	case s.body != nil:
		b, _ := json.Marshal(s.body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(s.method, base+s.path, rd)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if s.auth {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", s.name, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("%s: risposta non JSON: %v", s.name, err)
	}
	reqDoc := s.body
	if s.raw != "" {
		reqDoc = s.raw
	}
	return goldenRecord{
		Request:     goldenNormalize(reqDoc),
		Status:      resp.StatusCode,
		ContentType: mt,
		RetryAfter:  resp.Header.Get("Retry-After"),
		Response:    goldenNormalize(decoded),
	}
}

func TestGoldenContract(t *testing.T) {
	// Valori fissi e nel passato: nessun clamp, nessuna dipendenza dall'ora.
	const ts = int64(1700000000000)
	folderDTO := func(id string, parent any) map[string]any {
		return map[string]any{"id": id, "name": "Cartella " + id, "parent_id": parent, "updated_at": ts, "deleted_at": nil}
	}
	noteDTO := func(id string, folder any, title, content string) map[string]any {
		return map[string]any{"id": id, "title": title, "content": content, "folder_id": folder,
			"is_favorite": true, "is_pinned": false, "order_index": 3, "created_at": ts, "updated_at": ts, "deleted_at": nil}
	}

	steps := []goldenStep{
		{name: "healthz", method: "GET", path: "/healthz"},
		{name: "sync_401_senza_token", method: "POST", path: "/api/v1/sync", body: map[string]any{"last_synced_at": 0, "folders": []any{}, "notes": []any{}}},
		{name: "sync_405_get", method: "GET", path: "/api/v1/sync", auth: true},
		{name: "sync_400_json_non_valido", method: "POST", path: "/api/v1/sync", auth: true, raw: `{non json`},
		{name: "sync_200_primo_giro_vuoto", method: "POST", path: "/api/v1/sync", auth: true,
			body: map[string]any{"last_synced_at": 0, "folders": []any{}, "notes": []any{}}},
		{name: "sync_200_push_cartelle_e_nota_utf8", method: "POST", path: "/api/v1/sync", auth: true,
			body: map[string]any{"last_synced_at": 0,
				"folders": []any{folderDTO("f1", nil), folderDTO("f2", "f1")},
				"notes":   []any{noteDTO("n1", "f2", "è → 😀 <>&", "Perché → già: «ciao» 😀\n")}}},
		{name: "sync_422_riferimenti_non_validi", method: "POST", path: "/api/v1/sync", auth: true,
			body: map[string]any{"last_synced_at": 0,
				"folders": []any{folderDTO("f-orfana", "non-esiste")},
				"notes":   []any{noteDTO("n-orfana", "non-esiste-neanche-questa", "x", "y")}}},
		{name: "sync_200_full_resync_cursore_vecchio", method: "POST", path: "/api/v1/sync", auth: true,
			body: map[string]any{"last_synced_at": 1, "folders": []any{}, "notes": []any{}}},
	}

	base, token := goldenEnv(t, 0)
	// Il 413 richiede un tetto di body piccolo: secondo server dedicato.
	base413, token413 := goldenEnv(t, 256)
	big := map[string]any{"last_synced_at": 0, "folders": []any{},
		"notes": []any{noteDTO("n-grande", nil, "grande", string(bytes.Repeat([]byte("a"), 2048)))}}
	step413 := goldenStep{name: "sync_413_body_troppo_grande", method: "POST", path: "/api/v1/sync", auth: true, body: big}

	update := os.Getenv("UPDATE_GOLDEN") == "1"
	dir := filepath.Join("testdata", "golden_contract")

	check := func(rec goldenRecord, name string) {
		path := filepath.Join(dir, name+".json")
		if update {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			b, _ := json.MarshalIndent(rec, "", "  ")
			if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			return
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: golden mancante (%v). Rigenerarlo con: UPDATE_GOLDEN=1 go test -run TestGoldenContract -count=1 .", name, err)
		}
		var want goldenRecord
		if err := json.Unmarshal(raw, &want); err != nil {
			t.Fatalf("%s: golden illeggibile: %v", name, err)
		}
		if rec.Status != want.Status {
			t.Errorf("%s: status %d, atteso %d", name, rec.Status, want.Status)
		}
		if rec.ContentType != want.ContentType {
			t.Errorf("%s: media type %q, atteso %q", name, rec.ContentType, want.ContentType)
		}
		if rec.RetryAfter != want.RetryAfter {
			t.Errorf("%s: Retry-After %q, atteso %q", name, rec.RetryAfter, want.RetryAfter)
		}
		if !goldenContains(want.Response, rec.Response) {
			g, _ := json.MarshalIndent(rec.Response, "", "  ")
			w, _ := json.MarshalIndent(want.Response, "", "  ")
			t.Errorf("%s: la risposta non contiene più il golden contract\natteso (almeno):\n%s\nottenuto:\n%s", name, w, g)
		}
	}

	for _, s := range steps {
		check(goldenDo(t, base, token, s), s.name)
	}
	check(goldenDo(t, base413, token413, step413), step413.name)
}
