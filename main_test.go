package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"notes-server/internal/auth"
	"notes-server/internal/db"
)

const testSecret = "test-secret-test-secret-test-secret-0123456789"

type testEnv struct {
	t        *testing.T
	srv      *httptest.Server
	users    *db.UsersRepo
	tm       *auth.TokenManager
	repo     *db.RevocationRepo
	path     string
	settings *db.ServerSettingsRepo
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
	settings := db.NewServerSettingsRepo(sqlDB)
	hash, _ := auth.HashPassword("password-di-prova")
	if _, err := users.Create(context.Background(), "mario", hash); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return &testEnv{t: t, srv: srv, users: users, tm: tm, repo: repo, path: path, settings: settings}
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
	token, _ := e.loginTokens(username)
	return token
}

func (e *testEnv) loginTokens(username string) (string, string) {
	e.t.Helper()
	code, body, _ := e.do("POST", "/api/v1/auth/login", "", map[string]string{"username": username, "password": "password-di-prova"}, nil)
	if code != 200 {
		e.t.Fatalf("login %q: %d %s", username, code, body)
	}
	var r struct {
		Token        string `json:"token"`
		RefreshToken string `json:"refresh_token"`
	}
	_ = json.Unmarshal(body, &r)
	if r.Token == "" || r.RefreshToken == "" {
		e.t.Fatalf("login senza token/refresh token: %s", body)
	}
	return r.Token, r.RefreshToken
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

func TestLoginRequiresExactUsernameCase(t *testing.T) {
	e := newEnv(t)

	if e.login("  mario ") == "" {
		t.Fatal("token vuoto")
	}

	code, body, _ := e.do("POST", "/api/v1/auth/login", "",
		map[string]string{"username": "Mario", "password": "password-di-prova"}, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("login con case errato: %d %s, atteso 401", code, body)
	}
}

func TestLoginRejectsEmbeddedNUL(t *testing.T) {
	e := newEnv(t)
	code, body, _ := e.do("POST", "/api/v1/auth/login", "",
		map[string]string{"username": "mario\x00ignored", "password": "password-di-prova"}, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("login con NUL: %d %s, atteso 401", code, body)
	}
}

func TestLoginSQLInjectionAttemptFails(t *testing.T) {
	e := newEnv(t)
	code, body, _ := e.do("POST", "/api/v1/auth/login", "",
		map[string]string{"username": "' OR 1=1 --", "password": "password-di-prova"}, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("login injection: %d %s", code, body)
	}
}

func TestLoginReturnsStoredUsernameCase(t *testing.T) {
	e := newEnv(t)
	hash, err := auth.HashPassword("password-di-prova")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.users.Create(context.Background(), "Luigi", hash); err != nil {
		t.Fatal(err)
	}

	code, body, _ := e.do("POST", "/api/v1/auth/login", "",
		map[string]string{"username": "Luigi", "password": "password-di-prova"}, nil)
	if code != 200 {
		t.Fatalf("login: %d %s", code, body)
	}
	var r struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	if r.Username != "Luigi" {
		t.Fatalf("username restituito = %q, atteso %q", r.Username, "Luigi")
	}
}

func TestCaseVariantAccountsAreDistinct(t *testing.T) {
	e := newEnv(t)
	hash, err := auth.HashPassword("password-di-prova")
	if err != nil {
		t.Fatal(err)
	}

	upper, err := e.users.Create(context.Background(), "Nicola", hash)
	if err != nil {
		t.Fatalf("Create(Nicola): %v", err)
	}
	lower, err := e.users.Create(context.Background(), "nicola", hash)
	if err != nil {
		t.Fatalf("Create(nicola): %v", err)
	}
	if upper.ID == lower.ID {
		t.Fatal("le varianti di case hanno lo stesso user id")
	}

	login := func(username string) (string, string) {
		t.Helper()
		code, body, _ := e.do("POST", "/api/v1/auth/login", "",
			map[string]string{"username": username, "password": "password-di-prova"}, nil)
		if code != 200 {
			t.Fatalf("login %q: %d %s", username, code, body)
		}
		var r struct {
			Token  string `json:"token"`
			UserID string `json:"user_id"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			t.Fatal(err)
		}
		return r.Token, r.UserID
	}

	upperToken, upperID := login("Nicola")
	lowerToken, lowerID := login("nicola")
	if upperID != upper.ID || lowerID != lower.ID || upperID == lowerID {
		t.Fatalf("id login: %q/%q, attesi %q/%q", upperID, lowerID, upper.ID, lower.ID)
	}

	code, _, syncBody := e.sync(upperToken, 0, nil, []map[string]any{
		note("nota-di-nicola-maiuscola", "Solo Nicola", 1, nil),
	})
	if code != 200 {
		t.Fatalf("sync Nicola: %d %s", code, syncBody)
	}
	code, resp, _ := e.sync(lowerToken, 0, nil, nil)
	if code != 200 {
		t.Fatalf("sync nicola: %d", code)
	}
	if len(resp.Notes) != 0 {
		t.Fatalf("l'account minuscolo vede %d note dell'altro account", len(resp.Notes))
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

func TestRefreshRotatesAndReuseRevokesFamily(t *testing.T) {
	e := newEnv(t)
	access, refresh := e.loginTokens("mario")

	code, body, _ := e.do("POST", "/api/v1/auth/refresh", "", map[string]string{"refresh_token": refresh}, nil)
	if code != http.StatusOK {
		t.Fatalf("refresh: %d %s", code, body)
	}
	var r struct {
		Token            string `json:"token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresAt        int64  `json:"expires_at"`
		RefreshExpiresAt int64  `json:"refresh_expires_at"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	if r.Token == "" || r.RefreshToken == "" || r.RefreshToken == refresh || r.Token == access {
		t.Fatalf("rotazione non avvenuta: %s", body)
	}
	if r.ExpiresAt <= time.Now().Unix() || r.RefreshExpiresAt <= r.ExpiresAt {
		t.Fatalf("scadenze non valide: %+v", r)
	}
	if code, _, _ := e.sync(r.Token, 0, nil, nil); code != http.StatusOK {
		t.Fatalf("nuovo access token non valido: %d", code)
	}

	// Il riuso del token già consumato è un segnale di furto: revoca la famiglia.
	code, body, _ = e.do("POST", "/api/v1/auth/refresh", "", map[string]string{"refresh_token": refresh}, nil)
	if code != http.StatusUnauthorized || !strings.Contains(string(body), `"code":"invalid_grant"`) {
		t.Fatalf("riuso: %d %s", code, body)
	}
	code, body, _ = e.do("POST", "/api/v1/auth/refresh", "", map[string]string{"refresh_token": r.RefreshToken}, nil)
	if code != http.StatusUnauthorized || !strings.Contains(string(body), `"code":"invalid_grant"`) {
		t.Fatalf("famiglia non revocata: %d %s", code, body)
	}
}

func TestRefreshRejectsUnknownToken(t *testing.T) {
	e := newEnv(t)
	code, body, _ := e.do("POST", "/api/v1/auth/refresh", "", map[string]string{"refresh_token": "sconosciuto"}, nil)
	if code != http.StatusUnauthorized || !strings.Contains(string(body), `"code":"invalid_grant"`) {
		t.Fatalf("refresh sconosciuto: %d %s", code, body)
	}
}

func TestLogoutRevokesRefreshFamily(t *testing.T) {
	e := newEnv(t)
	access, refresh := e.loginTokens("mario")

	code, body, _ := e.do("POST", "/api/v1/auth/logout", access, map[string]string{"refresh_token": refresh}, nil)
	if code != http.StatusOK {
		t.Fatalf("logout: %d %s", code, body)
	}
	if code, _, _ := e.sync(access, 0, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("access token ancora valido: %d", code)
	}
	code, body, _ = e.do("POST", "/api/v1/auth/refresh", "", map[string]string{"refresh_token": refresh}, nil)
	if code != http.StatusUnauthorized || !strings.Contains(string(body), `"code":"invalid_grant"`) {
		t.Fatalf("refresh token ancora valido: %d %s", code, body)
	}
}

func TestLogoutWorksWithExpiredAccessToken(t *testing.T) {
	e := newEnv(t)
	_, refresh := e.loginTokens("mario")

	code, body, _ := e.do("POST", "/api/v1/auth/logout", "", map[string]string{"refresh_token": refresh}, nil)
	if code != http.StatusOK {
		t.Fatalf("logout con solo refresh: %d %s", code, body)
	}
	if code, _, _ := e.do("POST", "/api/v1/auth/refresh", "", map[string]string{"refresh_token": refresh}, nil); code != http.StatusUnauthorized {
		t.Fatalf("refresh token non revocato: %d", code)
	}
}

func TestLogoutAllRevokesEveryFamilyAndAccessToken(t *testing.T) {
	e := newEnv(t)
	accessA, refreshA := e.loginTokens("mario")
	accessB, refreshB := e.loginTokens("mario")

	code, body, _ := e.do("POST", "/api/v1/auth/logout-all", accessA, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("logout-all: %d %s", code, body)
	}
	for name, tok := range map[string]string{"A": accessA, "B": accessB} {
		if code, _, _ := e.sync(tok, 0, nil, nil); code != http.StatusUnauthorized {
			t.Fatalf("access token %s ancora valido: %d", name, code)
		}
	}
	for name, tok := range map[string]string{"A": refreshA, "B": refreshB} {
		if code, _, _ := e.do("POST", "/api/v1/auth/refresh", "", map[string]string{"refresh_token": tok}, nil); code != http.StatusUnauthorized {
			t.Fatalf("refresh token %s ancora valido: %d", name, code)
		}
	}
}

func TestAdminBlockRevokesRefreshFamily(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user, _ := e.users.GetByUsername(ctx, "mario")
	_, refresh := e.loginTokens("mario")
	cookie := e.adminCookie()

	if code, _ := e.doAdmin("POST", "/admin/users/block", cookie, url.Values{"user_id": {user.ID}, "blocked": {"1"}}); code != http.StatusSeeOther {
		t.Fatalf("blocco: %d", code)
	}
	code, body, _ := e.do("POST", "/api/v1/auth/refresh", "", map[string]string{"refresh_token": refresh}, nil)
	if code != http.StatusUnauthorized || !strings.Contains(string(body), `"code":"invalid_grant"`) {
		t.Fatalf("famiglia sopravvissuta al blocco: %d %s", code, body)
	}

	if code, _ := e.doAdmin("POST", "/admin/users/block", cookie, url.Values{"user_id": {user.ID}, "blocked": {"0"}}); code != http.StatusSeeOther {
		t.Fatalf("sblocco: %d", code)
	}
	code, body, _ = e.do("POST", "/api/v1/auth/refresh", "", map[string]string{"refresh_token": refresh}, nil)
	if code != http.StatusUnauthorized || !strings.Contains(string(body), `"code":"invalid_grant"`) {
		t.Fatalf("famiglia tornata valida dopo lo sblocco: %d %s", code, body)
	}
}

func TestAdminResetPasswordRevokesRefreshFamily(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user, _ := e.users.GetByUsername(ctx, "mario")
	access, refresh := e.loginTokens("mario")
	cookie := e.adminCookie()

	code, _ := e.doAdmin("POST", "/admin/users/reset-password", cookie, url.Values{
		"user_id": {user.ID}, "new_password": {"nuova-password-lunga"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("reset password: %d", code)
	}
	if code, _, _ := e.sync(access, 0, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("access token ancora valido dopo il reset: %d", code)
	}
	if code, _, _ := e.do("POST", "/api/v1/auth/refresh", "", map[string]string{"refresh_token": refresh}, nil); code != http.StatusUnauthorized {
		t.Fatalf("refresh token ancora valido dopo il reset: %d", code)
	}
}

func TestAdminDeleteRevokesRefreshFamily(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user, _ := e.users.GetByUsername(ctx, "mario")
	access, refresh := e.loginTokens("mario")
	cookie := e.adminCookie()

	if code, _ := e.doAdmin("POST", "/admin/users/delete", cookie, url.Values{"user_id": {user.ID}}); code != http.StatusSeeOther {
		t.Fatalf("eliminazione: %d", code)
	}
	if code, _, _ := e.sync(access, 0, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("access token ancora valido dopo l'eliminazione: %d", code)
	}
	if code, _, _ := e.do("POST", "/api/v1/auth/refresh", "", map[string]string{"refresh_token": refresh}, nil); code != http.StatusUnauthorized {
		t.Fatalf("refresh token ancora valido dopo l'eliminazione: %d", code)
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
	// html/template scrive il valore dell'attributo con le entità HTML ('+' del
	// base64 diventa "&#43;"): il browser le decodifica prima di confrontare il
	// nonce con la CSP, quindi la pagina è corretta. Il confronto va fatto sul
	// markup decodificato, altrimenti il test fallisce a caso (~29% delle volte,
	// quando il nonce casuale contiene '+').
	if !strings.Contains(html.UnescapeString(string(body)), `nonce="`+nonce+`"`) {
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
	if u, err := auth.NormalizeUsername("  Luigi "); err != nil || u != "Luigi" {
		t.Fatalf("normalizzazione: %q %v", u, err)
	}
	if _, err := auth.NormalizeUsername("ab"); err == nil {
		t.Fatal("username troppo corto accettato")
	}
	if _, err := auth.NormalizeUsername(strings.Repeat("x", 65)); err == nil {
		t.Fatal("username troppo lungo accettato")
	}
	e := newEnv(t)
	if _, err := e.users.Create(context.Background(), "MARIO", "hash"); err != nil {
		t.Fatalf("variante di case rifiutata: %v", err)
	}
	if _, err := e.users.Create(context.Background(), "mario", "hash"); !errors.Is(err, db.ErrUsernameTaken) {
		t.Fatalf("duplicato esatto = %v, atteso ErrUsernameTaken", err)
	}
}

func TestBlockedAccountGetsDistinctLoginAndSyncErrors(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	user, err := e.users.GetByUsername(ctx, "mario")
	if err != nil || user == nil {
		t.Fatalf("utente: %+v, %v", user, err)
	}
	tok := e.login("mario")

	if err := e.users.SetBlocked(ctx, user.ID, true); err != nil {
		t.Fatal(err)
	}
	code, body, _ := e.do("POST", "/api/v1/auth/login", "",
		map[string]string{"username": "mario", "password": "password-di-prova"}, nil)
	if code != http.StatusForbidden || !strings.Contains(string(body), `"code":"account_blocked"`) {
		t.Fatalf("login bloccato: %d %s", code, body)
	}

	// Il blocco vince sulla revoca: il client deve ricevere il messaggio
	// specifico, non un generico 401.
	if err := e.tm.RevokeUser(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	code, body, _ = e.do("POST", "/api/v1/sync", tok,
		map[string]any{"last_synced_at": 0, "folders": []any{}, "notes": []any{}}, nil)
	if code != http.StatusForbidden || !strings.Contains(string(body), `"code":"account_blocked"`) {
		t.Fatalf("sync bloccato: %d %s", code, body)
	}

	if err := e.users.SetBlocked(ctx, user.ID, false); err != nil {
		t.Fatal(err)
	}
	code, body, _ = e.do("POST", "/api/v1/sync", tok,
		map[string]any{"last_synced_at": 0, "folders": []any{}, "notes": []any{}}, nil)
	if code != http.StatusUnauthorized || !strings.Contains(string(body), `"code":"revoked_token"`) {
		t.Fatalf("token dopo sblocco: %d %s", code, body)
	}
}

func (e *testEnv) adminCookie() *http.Cookie {
	e.t.Helper()
	form := url.Values{"username": {"root"}, "password": {"a-very-long-admin-pass"}}
	req, _ := http.NewRequest("POST", e.srv.URL+"/admin/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		e.t.Fatalf("admin login: %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "admin_session" {
			return c
		}
	}
	e.t.Fatal("cookie admin_session mancante")
	return nil
}

func (e *testEnv) doAdmin(method, path string, cookie *http.Cookie, form url.Values) (int, []byte) {
	e.t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.AddCookie(cookie)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func TestAdminUserSearchRateLimitAndBlocking(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cookie := e.adminCookie()

	if code, body := e.doAdmin("GET", "/admin?q=mario", cookie, nil); code != http.StatusOK || !strings.Contains(string(body), "mario") {
		t.Fatalf("ricerca admin: %d %s", code, body)
	}

	if code, _ := e.doAdmin("POST", "/admin/settings/rate-limit", cookie, url.Values{}); code != http.StatusSeeOther {
		t.Fatalf("toggle globale: %d", code)
	}
	settings, err := e.settings.Get(ctx)
	if err != nil || settings.UserRateLimitEnabled {
		t.Fatalf("impostazioni dopo toggle: %+v, %v", settings, err)
	}

	mario, _ := e.users.GetByUsername(ctx, "mario")
	code, _ := e.doAdmin("POST", "/admin/users/rate-limit", cookie, url.Values{"user_id": {mario.ID}, "exempt": {"1"}})
	if code != http.StatusSeeOther {
		t.Fatalf("toggle per-utente: %d", code)
	}
	mario, _ = e.users.GetByID(ctx, mario.ID)
	if mario == nil || !mario.RateLimitExempt {
		t.Fatalf("esenzione non salvata: %+v", mario)
	}

	tok := e.login("mario")
	claims, _ := e.tm.ParseToken(tok)
	code, _ = e.doAdmin("POST", "/admin/users/block", cookie, url.Values{"user_id": {mario.ID}, "blocked": {"1"}})
	if code != http.StatusSeeOther {
		t.Fatalf("blocco: %d", code)
	}
	mario, _ = e.users.GetByID(ctx, mario.ID)
	if mario == nil || mario.BlockedAt == nil {
		t.Fatal("utente non bloccato")
	}
	if !e.tm.IsRevoked(claims) {
		t.Fatal("i token dell'utente bloccato non sono stati revocati")
	}

	// CSRF: la stessa azione da un'origine cross-site non deve passare.
	req, _ := http.NewRequest("POST", e.srv.URL+"/admin/users/block", strings.NewReader("user_id="+mario.ID+"&blocked=0"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.AddCookie(cookie)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("CSRF cross-site: %d, atteso 403", resp.StatusCode)
	}

	code, _ = e.doAdmin("POST", "/admin/users/block", cookie, url.Values{"user_id": {mario.ID}, "blocked": {"0"}})
	if code != http.StatusSeeOther {
		t.Fatalf("sblocco: %d", code)
	}
	mario, _ = e.users.GetByID(ctx, mario.ID)
	if mario == nil || mario.BlockedAt != nil {
		t.Fatal("utente ancora bloccato")
	}
}
