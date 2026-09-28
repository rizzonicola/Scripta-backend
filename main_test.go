package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"notes-server/internal/auth"
	"notes-server/internal/db"
)

const testSecret = "test-secret-test-secret-test-secret-0123456789"

type testEnv struct {
	t   *testing.T
	srv *httptest.Server
	users *db.UsersRepo
	tm    *auth.TokenManager
	repo  *db.RevocationRepo
	path  string
}

func newEnv(t *testing.T, trustedProxies ...string) *testEnv {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	sqlDB, err := db.Open(path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	cfg := config{
		DBPath: path, JWTSecret: testSecret, AdminUser: "root", AdminPass: "a-very-long-admin-pass",
		JWTTTL: time.Hour, AdminSessionTTL: time.Hour, TrustedProxies: trustedProxies,
	}
	tm := auth.NewTokenManager(cfg.JWTSecret, cfg.JWTTTL)
	repo := db.NewRevocationRepo(sqlDB)
	if err := tm.AttachStore(context.Background(), repo); err != nil {
		t.Fatalf("attach store: %v", err)
	}
	h, err := buildRouter(cfg, sqlDB, tm)
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	users := db.NewUsersRepo(sqlDB)
	hash, _ := auth.HashPassword("password-di-prova")
	if _, err := users.Create(context.Background(), "mario", hash); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return &testEnv{t: t, srv: srv, users: users, tm: tm, repo: repo, path: path}
}

func (e *testEnv) do(method, path, token string, body any, hdr map[string]string) (int, []byte, http.Header) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out, resp.Header
}

func (e *testEnv) login(username string) string {
	e.t.Helper()
	code, body, _ := e.do("POST", "/api/v1/auth/login", "", map[string]string{"username": username, "password": "password-di-prova"}, nil)
	if code != 200 {
		e.t.Fatalf("login %q: %d %s", username, code, body)
	}
	var r struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(body, &r)
	return r.Token
}

type syncResp struct {
	ServerTime int64 `json:"server_time"`
	FullResync bool  `json:"full_resync"`
	Notes      []struct {
		ID        string `json:"id"`
		UpdatedAt int64  `json:"updated_at"`
		Title     string `json:"title"`
	} `json:"notes"`
	Folders []struct {
		ID string `json:"id"`
	} `json:"folders"`
}

func (e *testEnv) sync(token string, last int64, folders, notes []map[string]any) (int, syncResp, []byte) {
	e.t.Helper()
	if folders == nil {
		folders = []map[string]any{}
	}
	if notes == nil {
		notes = []map[string]any{}
	}
	code, body, _ := e.do("POST", "/api/v1/sync", token, map[string]any{"last_synced_at": last, "folders": folders, "notes": notes}, nil)
	var r syncResp
	_ = json.Unmarshal(body, &r)
	return code, r, body
}

func note(id, title string, updated int64, folder any) map[string]any {
	return map[string]any{"id": id, "title": title, "content": "c", "folder_id": folder, "is_favorite": false, "is_pinned": false, "order_index": 0, "updated_at": updated, "deleted_at": nil}
}

func TestHealth(t *testing.T) {
	e := newEnv(t)
	code, body, _ := e.do("GET", "/healthz", "", nil, nil)
	if code != 200 || !strings.Contains(string(body), serverVersion) {
		t.Fatalf("healthz: %d %s", code, body)
	}
}

func TestLoginIsCaseInsensitive(t *testing.T) {
	e := newEnv(t)
	if e.login("  MaRiO ") == "" {
		t.Fatal("token vuoto")
	}
}

func TestSyncRequiresAuth(t *testing.T) {
	e := newEnv(t)
	if code, _, _ := e.sync("", 0, nil, nil); code != 401 {
		t.Fatalf("atteso 401, ottenuto %d", code)
	}
}

// Il cursore si basa sullo stamp SERVER: una modifica con updated_at molto
// vecchio (client con orologio indietro) deve comunque arrivare a un altro
// dispositivo che ha già un cursore recente.
func TestPullCursorIsServerStamped(t *testing.T) {
	e := newEnv(t)
	tok := e.login("mario")

	_, first, _ := e.sync(tok, 0, nil, nil)
	if first.ServerTime == 0 {
		t.Fatal("server_time assente")
	}
	oldTs := time.Now().Add(-48 * time.Hour).UnixMilli()
	if code, _, b := e.sync(tok, first.ServerTime, nil, []map[string]any{note("n1", "vecchia", oldTs, nil)}); code != 200 {
		t.Fatalf("push: %d %s", code, b)
	}
	_, pulled, _ := e.sync(tok, first.ServerTime, nil, nil)
	if len(pulled.Notes) != 1 || pulled.Notes[0].ID != "n1" {
		t.Fatalf("la nota con updated_at vecchio non è stata consegnata: %+v", pulled.Notes)
	}
	// Cursore avanzato: nessuna ripetizione.
	_, again, _ := e.sync(tok, pulled.ServerTime, nil, nil)
	if len(again.Notes) != 0 {
		t.Fatalf("attese 0 note, trovate %d", len(again.Notes))
	}
}

func TestFutureTimestampIsClamped(t *testing.T) {
	e := newEnv(t)
	tok := e.login("mario")
	future := time.Now().Add(72 * time.Hour).UnixMilli()
	_, r, _ := e.sync(tok, 0, nil, []map[string]any{note("n1", "futura", future, nil)})
	_, r2, _ := e.sync(tok, 0, nil, nil)
	_ = r
	if len(r2.Notes) != 1 {
		t.Fatalf("attesa 1 nota, trovate %d", len(r2.Notes))
	}
	limit := time.Now().Add(6 * time.Minute).UnixMilli()
	if r2.Notes[0].UpdatedAt > limit {
		t.Fatalf("updated_at nel futuro non corretto: %d", r2.Notes[0].UpdatedAt)
	}
}

func TestInvalidFolderIsRejectedExplicitly(t *testing.T) {
	e := newEnv(t)
	tok := e.login("mario")
	code, _, body := e.sync(tok, 0, nil, []map[string]any{note("n1", "x", time.Now().UnixMilli(), "cartella-inesistente")})
	if code != 422 {
		t.Fatalf("atteso 422, ottenuto %d (%s)", code, body)
	}
	var r struct {
		Rejected   []map[string]string `json:"rejected"`
		ServerTime int64               `json:"server_time"`
	}
	_ = json.Unmarshal(body, &r)
	if len(r.Rejected) != 1 || r.Rejected[0]["id"] != "n1" {
		t.Fatalf("rejected inatteso: %s", body)
	}
	if r.ServerTime != 0 {
		t.Fatal("un batch rifiutato non deve restituire server_time (il client non deve avanzare il cursore)")
	}
	// Nessuna modifica applicata (rollback dell'intero batch).
	_, all, _ := e.sync(tok, 0, nil, nil)
	if len(all.Notes) != 0 {
		t.Fatal("la nota rifiutata è stata comunque salvata")
	}
}

func TestLogoutRevokesTokenAndPersists(t *testing.T) {
	e := newEnv(t)
	tok := e.login("mario")
	if code, _, _ := e.sync(tok, 0, nil, nil); code != 200 {
		t.Fatalf("sync pre-logout: %d", code)
	}
	if code, _, _ := e.do("POST", "/api/v1/auth/logout", tok, nil, nil); code != 200 {
		t.Fatalf("logout: %d", code)
	}
	if code, _, _ := e.sync(tok, 0, nil, nil); code != 401 {
		t.Fatalf("token revocato ancora accettato: %d", code)
	}

	// "Riavvio": nuovo TokenManager che ricarica le revoche dal DB.
	tm2 := auth.NewTokenManager(testSecret, time.Hour)
	if err := tm2.AttachStore(context.Background(), e.repo); err != nil {
		t.Fatal(err)
	}
	claims, err := tm2.ParseToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	if !tm2.IsRevoked(claims) {
		t.Fatal("la revoca non è sopravvissuta al riavvio")
	}
}

func TestUserRevocationPersists(t *testing.T) {
	e := newEnv(t)
	tok := e.login("mario")
	claims, _ := e.tm.ParseToken(tok)
	time.Sleep(5 * time.Millisecond)
	if err := e.tm.RevokeUser(context.Background(), claims.UserID); err != nil {
		t.Fatal(err)
	}
	tm2 := auth.NewTokenManager(testSecret, time.Hour)
	if err := tm2.AttachStore(context.Background(), e.repo); err != nil {
		t.Fatal(err)
	}
	if !tm2.IsRevoked(claims) {
		t.Fatal("revoca per utente non persistita")
	}
}

// Senza proxy fidati, Cf-Connecting-IP / X-Forwarded-For NON devono
// permettere di aggirare il rate limit del login.
func TestRateLimitIgnoresSpoofedHeaders(t *testing.T) {
	e := newEnv(t)
	got429 := false
	for i := 0; i < 30; i++ {
		code, _, _ := e.do("POST", "/api/v1/auth/login", "", map[string]string{"username": "mario", "password": "sbagliata"},
			map[string]string{"Cf-Connecting-IP": "203.0.113." + string(rune('1'+i%9)) + "0", "X-Forwarded-For": "198.51.100.7"})
		if code == 429 {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatal("il rate limit è stato aggirato con header falsificati")
	}
}

func TestAdminPagesHaveSecurityHeadersAndNonce(t *testing.T) {
	e := newEnv(t)
	code, body, h := e.do("GET", "/admin/login", "", nil, nil)
	if code != 200 {
		t.Fatalf("login page: %d", code)
	}
	csp := h.Get("Content-Security-Policy")
	if !strings.Contains(csp, "nonce-") || strings.Contains(csp, "unsafe-inline") {
		t.Fatalf("CSP inattesa: %q", csp)
	}
	nonce := strings.TrimSpace(strings.SplitN(strings.SplitN(csp, "script-src 'nonce-", 2)[1], "'", 2)[0])
	if !strings.Contains(string(body), `nonce="`+nonce+`"`) {
		t.Fatal("il nonce della CSP non compare nel markup")
	}
	if h.Get("X-Frame-Options") != "DENY" || h.Get("Strict-Transport-Security") == "" {
		t.Fatal("mancano X-Frame-Options / HSTS")
	}
}

func TestFlashMessageFromQueryIsNotReflected(t *testing.T) {
	e := newEnv(t)
	_, body, _ := e.do("GET", "/admin/login?flash=<b>PHISHING</b>&f=nonexistent", "", nil, nil)
	if strings.Contains(string(body), "PHISHING") {
		t.Fatal("testo arbitrario riflesso dalla query string")
	}
}

func TestConfigRejectsInsecureDefaults(t *testing.T) {
	t.Setenv("JWT_SECRET", "")
	t.Setenv("ADMIN_USER", "admin")
	t.Setenv("ADMIN_PASS", "admin")
	if _, err := loadConfig(); err == nil {
		t.Fatal("configurazione insicura accettata")
	}
	t.Setenv("JWT_SECRET", "CAMBIAMI-con-un-valore-casuale-lungo-almeno-32")
	t.Setenv("ADMIN_PASS", "una-password-lunga-e-casuale")
	if _, err := loadConfig(); err == nil {
		t.Fatal("JWT_SECRET segnaposto accettato")
	}
	t.Setenv("JWT_SECRET", testSecret)
	if _, err := loadConfig(); err != nil {
		t.Fatalf("configurazione valida rifiutata: %v", err)
	}
}

func TestForeignKeysSurviveConnectionRecycling(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fk.db")
	conn, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Forza la chiusura delle connessioni inattive: la successiva ne apre
	// una nuova, che deve avere foreign_keys=ON (init-hook per-connessione).
	conn.SetMaxIdleConns(0)
	var on int
	if err := conn.QueryRow("PRAGMA foreign_keys").Scan(&on); err != nil || on != 1 {
		t.Fatalf("foreign_keys non attivo su nuova connessione: %d %v", on, err)
	}
}

func TestUsernameNormalizationAndDuplicates(t *testing.T) {
	if u, err := auth.NormalizeUsername("  Luigi "); err != nil || u != "luigi" {
		t.Fatalf("normalizzazione: %q %v", u, err)
	}
	if _, err := auth.NormalizeUsername("ab"); err == nil {
		t.Fatal("username troppo corto accettato")
	}
	if _, err := auth.NormalizeUsername(strings.Repeat("x", 65)); err == nil {
		t.Fatal("username troppo lungo accettato")
	}
	e := newEnv(t)
	if _, err := e.users.Create(context.Background(), "MARIO", "hash"); err == nil {
		t.Fatal("duplicato case-insensitive accettato")
	}
}
