package handlers

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"errors"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"notes-server/internal/auth"
	"notes-server/internal/db"
	"notes-server/internal/middleware"
)

type AdminHandler struct {
	Users         *db.UsersRepo
	Tokens        *auth.TokenManager        // per revocare i JWT utente su reset password / cancellazione
	RefreshTokens *db.RefreshTokensRepo     // per revocare le famiglie refresh sugli stessi eventi
	Sessions      *auth.AdminSessionManager // firma/valida il cookie di sessione della dashboard
	Settings      *db.ServerSettingsRepo    // toggle globale del rate limit

	adminUser string
	adminPass string

	tmpl *template.Template
}

// adminSessionCookiePath limita il cookie alle sole rotte /admin*: non ha
// motivo di essere inviato dal browser sulle chiamate API /api/v1/*.
const adminSessionCookiePath = "/admin"

// NewAdminHandler carica i template HTML dal filesystem embedded.
//
// Cartelle e note vivono interamente nel database: cancellare un utente è
// una singola operazione a DB, propagata dalle foreign key ON DELETE CASCADE
// (che restano attive su ogni connessione: vedi internal/db/db.go).
func NewAdminHandler(users *db.UsersRepo, tokens *auth.TokenManager, refreshTokens *db.RefreshTokensRepo, sessions *auth.AdminSessionManager, settings *db.ServerSettingsRepo, adminUser, adminPass string, templatesFS embed.FS) (*AdminHandler, error) {
	tmpl, err := template.ParseFS(templatesFS, "web/templates/*.html")
	if err != nil {
		return nil, err
	}
	return &AdminHandler{
		Users:         users,
		Tokens:        tokens,
		RefreshTokens: refreshTokens,
		Sessions:      sessions,
		Settings:      settings,
		adminUser:     adminUser,
		adminPass:     adminPass,
		tmpl:          tmpl,
	}, nil
}

// revokeUserSessions invalida sia i JWT sia le famiglie refresh dell'utente.
// Le due revoche sono indipendenti: se la prima fallisce non si tenta la
// seconda, così il chiamante può annullare l'operazione pericolosa.
func (h *AdminHandler) revokeUserSessions(ctx context.Context, userID string) error {
	if h.RefreshTokens != nil {
		if err := h.RefreshTokens.RevokeAllForUser(ctx, userID, time.Now()); err != nil {
			return err
		}
	}
	if h.Tokens != nil {
		return h.Tokens.RevokeUser(ctx, userID)
	}
	return nil
}

// constantTimeEqual confronta due stringhe in tempo costante rispetto sia al
// contenuto sia alla LUNGHEZZA: si confrontano gli hash SHA-256 (di
// lunghezza fissa), così subtle.ConstantTimeCompare non restituisce subito
// 0 su lunghezze diverse rivelandole tramite timing.
func constantTimeEqual(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

// ---------------------------------------------------------------------------
// Messaggi flash
//
// PRIMA: il testo del messaggio viaggiava in chiaro nella query string
// (?flash=...), quindi chiunque potesse far aprire a un admin un link
// costruito ad arte poteva far mostrare nella dashboard un testo arbitrario
// (content spoofing / phishing interno), e i messaggi includevano
// direttamente errori del DB e username.
//
// ORA: in query string viaggia solo un CODICE breve (?f=user_created); il
// testo è una costante lato server scelta da questa whitelist. Un codice
// sconosciuto non produce alcun messaggio.
// ---------------------------------------------------------------------------

type flashDef struct {
	msg   string
	isErr bool
}

var flashMessages = map[string]flashDef{
	"login_invalid":         {"Credenziali non valide", true},
	"bad_request":           {"Richiesta non valida", true},
	"internal":              {"Errore interno, riprovare", true},
	"fields_required":       {"Username e password sono obbligatori", true},
	"username_invalid":      {"Username non valido: 3-64 caratteri tra lettere ASCII, numeri, punto, trattino e underscore", true},
	"password_short":        {"La password deve avere almeno 8 caratteri", true},
	"password_long":         {"La password non può superare i 72 byte", true},
	"user_exists":           {"Impossibile creare l'utente: username già in uso", true},
	"user_create_failed":    {"Impossibile creare l'utente", true},
	"user_created":          {"Utente creato con successo", false},
	"reset_fields_required": {"Utente e nuova password sono obbligatori", true},
	"user_not_found":        {"Utente non trovato", true},
	"reset_failed":          {"Errore nel reset della password", true},
	"reset_revoke_failed":   {"Revoca dei token non riuscita: password non modificata", true},
	"password_updated":      {"Password aggiornata con successo", false},
	"user_id_required":      {"ID utente obbligatorio", true},
	"delete_failed":         {"Errore nell'eliminazione dell'utente", true},
	"delete_revoke_failed":  {"Revoca dei token non riuscita: utente non eliminato", true},
	"user_deleted":          {"Utente e tutti i suoi dati (cartelle e note) eliminati definitivamente", false},
	"rate_limit_enabled":    {"Rate limit per-utente attivato per tutti gli account", false},
	"rate_limit_disabled":   {"Rate limit per-utente disattivato per tutti gli account", false},
	"settings_failed":       {"Impossibile aggiornare le impostazioni del server", true},
	"user_limit_exempt":     {"Rate limit disattivato per questo utente", false},
	"user_limit_restored":   {"Rate limit riattivato per questo utente", false},
	"user_update_failed":    {"Impossibile aggiornare l'utente", true},
	"user_blocked":          {"Account bloccato: le sessioni attive verranno rifiutate", false},
	"user_unblocked":        {"Account sbloccato", false},
	"block_revoke_warning":  {"Account bloccato, ma la revoca delle sessioni non è riuscita", true},
}

// flashFromRequest legge il codice ?f= e lo traduce nel testo whitelisted.
func flashFromRequest(r *http.Request) (msg string, isErr bool) {
	def, ok := flashMessages[r.URL.Query().Get("f")]
	if !ok {
		return "", false
	}
	return def.msg, def.isErr
}

// redirectFlash reindirizza a path con un CODICE flash (mai testo libero).
func redirectFlash(w http.ResponseWriter, r *http.Request, path, code string) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	http.Redirect(w, r, path+sep+"f="+template.URLQueryEscaper(code), http.StatusSeeOther)
}

// adminSearchQuery estrae il filtro di ricerca dalla richiesta, con un limite
// difensivo: la query finisce in un LIKE e non deve poter essere arbitrariamente
// grande. Il testo resta visibile nel form e viene riescapato dal template.
func adminSearchQuery(r *http.Request) string {
	q := strings.TrimSpace(r.FormValue("q"))
	if utf8.RuneCountInString(q) > 64 {
		q = string([]rune(q)[:64])
	}
	return q
}

// adminPathWithSearch preserva il filtro dopo un'azione POST.
func adminPathWithSearch(q string) string {
	if q == "" {
		return "/admin"
	}
	return "/admin?q=" + url.QueryEscape(q)
}

// redirectAdminFlash reindirizza alla dashboard mantenendo la ricerca.
func redirectAdminFlash(w http.ResponseWriter, r *http.Request, code string) {
	redirectFlash(w, r, adminPathWithSearch(adminSearchQuery(r)), code)
}

type loginPageData struct {
	Nonce        string
	Flash        string
	FlashIsError bool
}

// LoginPage gestisce GET /admin/login: mostra il form di accesso. Se una
// sessione valida è già presente, salta direttamente alla dashboard.
func (h *AdminHandler) LoginPage(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(middleware.AdminSessionCookieName); err == nil && h.Sessions.Validate(c.Value) == nil {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	msg, isErr := flashFromRequest(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := loginPageData{Nonce: middleware.NonceFromContext(r.Context()), Flash: msg, FlashIsError: isErr}
	if err := h.tmpl.ExecuteTemplate(w, "admin_login.html", data); err != nil {
		log.Printf("admin: errore rendering login: %v", err)
		http.Error(w, "errore interno", http.StatusInternalServerError)
	}
}

// Login gestisce POST /admin/login: verifica ADMIN_USER/ADMIN_PASS ed emette
// il cookie di sessione. Il rate limiting è applicato a monte (main.go).
func (h *AdminHandler) Login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		redirectFlash(w, r, "/admin/login", "bad_request")
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")

	// Entrambi i confronti vengono sempre eseguiti (niente short-circuit).
	userOK := constantTimeEqual(username, h.adminUser)
	passOK := constantTimeEqual(password, h.adminPass)
	if !(userOK && passOK) {
		redirectFlash(w, r, "/admin/login", "login_invalid")
		return
	}

	token, expiresAt, err := h.Sessions.GenerateSession()
	if err != nil {
		log.Printf("admin: errore generazione sessione: %v", err)
		redirectFlash(w, r, "/admin/login", "internal")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     middleware.AdminSessionCookieName,
		Value:    token,
		Path:     adminSessionCookiePath,
		Expires:  expiresAt,
		HttpOnly: true, // mai leggibile da JS: mitiga furto via XSS
		Secure:   true, // il browser lo invia solo su https (Cloudflare termina TLS all'edge)
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// Logout gestisce POST /admin/logout: cancella il cookie di sessione.
func (h *AdminHandler) Logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "metodo non consentito", http.StatusMethodNotAllowed)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     middleware.AdminSessionCookieName,
		Value:    "",
		Path:     adminSessionCookiePath,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

type userView struct {
	ID              string
	Username        string
	CreatedAt       string
	Blocked         bool
	RateLimitExempt bool
}

type usersPageData struct {
	Nonce            string
	Users            []userView
	Flash            string
	FlashIsError     bool
	SearchQuery      string
	HasSearch        bool
	RateLimitEnabled bool
	UserCount        int
	ActiveCount      int
	BlockedCount     int
	ExemptCount      int
}

// UsersPage gestisce GET /admin -> elenco utenti + form nuovo utente.
func (h *AdminHandler) UsersPage(w http.ResponseWriter, r *http.Request) {
	msg, isErr := flashFromRequest(r)
	search := adminSearchQuery(r)

	users, err := h.Users.List(r.Context(), search)
	if err != nil {
		// Il dettaglio dell'errore DB resta nei log: mai mostrato nella UI.
		log.Printf("admin: errore caricamento utenti: %v", err)
		http.Error(w, "errore interno", http.StatusInternalServerError)
		return
	}

	settings, err := h.Settings.Get(r.Context())
	if err != nil {
		log.Printf("admin: errore lettura impostazioni: %v", err)
		http.Error(w, "errore interno", http.StatusInternalServerError)
		return
	}

	views := make([]userView, 0, len(users))
	active, blocked, exempt := 0, 0, 0
	for _, u := range users {
		isBlocked := u.BlockedAt != nil
		if isBlocked {
			blocked++
		} else {
			active++
		}
		if u.RateLimitExempt {
			exempt++
		}
		views = append(views, userView{
			ID:              u.ID,
			Username:        u.Username,
			CreatedAt:       time.UnixMilli(u.CreatedAt).Format("2006-01-02 15:04"),
			Blocked:         isBlocked,
			RateLimitExempt: u.RateLimitExempt,
		})
	}

	data := usersPageData{
		Nonce:            middleware.NonceFromContext(r.Context()),
		Users:            views,
		Flash:            msg,
		FlashIsError:     isErr,
		SearchQuery:      search,
		HasSearch:        search != "",
		RateLimitEnabled: settings.UserRateLimitEnabled,
		UserCount:        len(views),
		ActiveCount:      active,
		BlockedCount:     blocked,
		ExemptCount:      exempt,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "users.html", data); err != nil {
		log.Printf("admin: errore rendering dashboard: %v", err)
		http.Error(w, "errore interno", http.StatusInternalServerError)
	}
}

// UpdateRateLimitSetting gestisce il toggle globale del rate limit sync.
// Default ON: la casella arriva valorizzata solo quando l'admin la lascia
// selezionata, quindi l'assenza del campo significa "disattiva".
func (h *AdminHandler) UpdateRateLimitSetting(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "metodo non consentito", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		redirectAdminFlash(w, r, "bad_request")
		return
	}
	enabled := r.FormValue("enabled") == "on"
	if err := h.Settings.SetUserRateLimitEnabled(r.Context(), enabled); err != nil {
		log.Printf("admin: aggiornamento rate limit globale: %v", err)
		redirectAdminFlash(w, r, "settings_failed")
		return
	}
	if enabled {
		redirectAdminFlash(w, r, "rate_limit_enabled")
	} else {
		redirectAdminFlash(w, r, "rate_limit_disabled")
	}
}

// UpdateUserRateLimit gestisce l'esenzione per-utente. Il campo "exempt"
// contiene lo stato NUOVO desiderato (1 = esente), non quello corrente.
func (h *AdminHandler) UpdateUserRateLimit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "metodo non consentito", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		redirectAdminFlash(w, r, "bad_request")
		return
	}
	userID := strings.TrimSpace(r.FormValue("user_id"))
	if userID == "" {
		redirectAdminFlash(w, r, "user_id_required")
		return
	}
	exempt := r.FormValue("exempt") == "1"
	if err := h.Users.SetRateLimitExempt(r.Context(), userID, exempt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			redirectAdminFlash(w, r, "user_not_found")
			return
		}
		log.Printf("admin: aggiornamento esenzione rate limit utente %s: %v", userID, err)
		redirectAdminFlash(w, r, "user_update_failed")
		return
	}
	if exempt {
		redirectAdminFlash(w, r, "user_limit_exempt")
	} else {
		redirectAdminFlash(w, r, "user_limit_restored")
	}
}

// SetUserBlocked blocca/sblocca un account.
//
// Blocco: blocked_at viene scritto PRIMA della revoca, così il middleware
// rifiuta immediatamente ogni richiesta anche se la revoca dei token dovesse
// fallire. Sblocco: si revocano prima i token emessi durante il blocco e solo
// dopo si azzera blocked_at; in caso di errore l'account resta bloccato.
func (h *AdminHandler) SetUserBlocked(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "metodo non consentito", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		redirectAdminFlash(w, r, "bad_request")
		return
	}
	userID := strings.TrimSpace(r.FormValue("user_id"))
	if userID == "" {
		redirectAdminFlash(w, r, "user_id_required")
		return
	}
	block := r.FormValue("blocked") == "1"

	if block {
		if err := h.Users.SetBlocked(r.Context(), userID, true); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				redirectAdminFlash(w, r, "user_not_found")
				return
			}
			log.Printf("admin: blocco utente %s: %v", userID, err)
			redirectAdminFlash(w, r, "user_update_failed")
			return
		}
		if err := h.revokeUserSessions(r.Context(), userID); err != nil {
			log.Printf("admin: revoca sessioni dopo il blocco di %s: %v", userID, err)
			redirectAdminFlash(w, r, "block_revoke_warning")
			return
		}
		redirectAdminFlash(w, r, "user_blocked")
		return
	}

	if err := h.revokeUserSessions(r.Context(), userID); err != nil {
		log.Printf("admin: revoca sessioni prima dello sblocco di %s: %v", userID, err)
		redirectAdminFlash(w, r, "block_revoke_warning")
		return
	}
	if err := h.Users.SetBlocked(r.Context(), userID, false); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			redirectAdminFlash(w, r, "user_not_found")
			return
		}
		log.Printf("admin: sblocco utente %s: %v", userID, err)
		redirectAdminFlash(w, r, "user_update_failed")
		return
	}
	redirectAdminFlash(w, r, "user_unblocked")
}

// passwordFlashCode traduce un errore di validazione password nel codice flash.
func passwordFlashCode(err error) string {
	if errors.Is(err, auth.ErrPasswordTooLong) {
		return "password_long"
	}
	return "password_short"
}

// CreateUser gestisce POST /admin/users/create. L'username viene validato e
// salvato con il case originale (trim, limiti di lunghezza e soli caratteri
// ASCII ammessi); unicità e login sono case-sensitive, quindi "Mario" e
// "mario" sono due account distinti. La password in chiaro viene cifrata
// subito con bcrypt e mai più mostrata né salvata.
func (h *AdminHandler) CreateUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "metodo non consentito", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		redirectAdminFlash(w, r, "bad_request")
		return
	}

	rawUsername := r.FormValue("username")
	password := r.FormValue("password")
	if rawUsername == "" || password == "" {
		redirectAdminFlash(w, r, "fields_required")
		return
	}
	username, err := auth.NormalizeUsername(rawUsername)
	if err != nil {
		redirectAdminFlash(w, r, "username_invalid")
		return
	}
	if err := auth.ValidatePassword(password); err != nil {
		redirectAdminFlash(w, r, passwordFlashCode(err))
		return
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		log.Printf("admin: errore hashing password: %v", err)
		redirectAdminFlash(w, r, "internal")
		return
	}

	if _, err := h.Users.Create(r.Context(), username, hash); err != nil {
		if errors.Is(err, db.ErrUsernameTaken) {
			redirectAdminFlash(w, r, "user_exists")
			return
		}
		log.Printf("admin: errore creazione utente: %v", err)
		redirectAdminFlash(w, r, "user_create_failed")
		return
	}

	redirectAdminFlash(w, r, "user_created")
}

// ResetPassword gestisce POST /admin/users/reset-password.
// Non è mai possibile visualizzare la password precedente: viene solo sostituito l'hash.
func (h *AdminHandler) ResetPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "metodo non consentito", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		redirectAdminFlash(w, r, "bad_request")
		return
	}

	userID := r.FormValue("user_id")
	newPassword := r.FormValue("new_password")
	if userID == "" || newPassword == "" {
		redirectAdminFlash(w, r, "reset_fields_required")
		return
	}
	if err := auth.ValidatePassword(newPassword); err != nil {
		redirectAdminFlash(w, r, passwordFlashCode(err))
		return
	}

	user, err := h.Users.GetByID(r.Context(), userID)
	if err != nil {
		log.Printf("admin: errore lettura utente %s: %v", userID, err)
		redirectAdminFlash(w, r, "internal")
		return
	}
	if user == nil {
		redirectAdminFlash(w, r, "user_not_found")
		return
	}

	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		log.Printf("admin: errore hashing password: %v", err)
		redirectAdminFlash(w, r, "internal")
		return
	}

	// La revoca viene PRIMA del cambio password: se il salvataggio
	// persistente fallisce, l'operazione pericolosa non avviene e i vecchi
	// token restano validi (come prima del reset), invece di restare
	// validi nella sola RAM fino al prossimo riavvio.
	if err := h.revokeUserSessions(r.Context(), userID); err != nil {
		log.Printf("admin: revoca sessioni prima del reset non riuscita (utente %s): %v", userID, err)
		redirectAdminFlash(w, r, "reset_revoke_failed")
		return
	}

	if err := h.Users.UpdatePassword(r.Context(), userID, hash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			redirectAdminFlash(w, r, "user_not_found")
			return
		}
		log.Printf("admin: errore reset password: %v", err)
		redirectAdminFlash(w, r, "reset_failed")
		return
	}

	redirectAdminFlash(w, r, "password_updated")
}

// DeleteUser gestisce POST /admin/users/delete: eliminazione irreversibile di
// un utente. Le foreign key ON DELETE CASCADE ripuliscono cartelle, note e
// impostazioni. La conferma "sei sicuro?" è nella modale del template.
func (h *AdminHandler) DeleteUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "metodo non consentito", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		redirectAdminFlash(w, r, "bad_request")
		return
	}

	userID := r.FormValue("user_id")
	if userID == "" {
		redirectAdminFlash(w, r, "user_id_required")
		return
	}

	user, err := h.Users.GetByID(r.Context(), userID)
	if err != nil {
		log.Printf("admin: errore lettura utente %s: %v", userID, err)
		redirectAdminFlash(w, r, "internal")
		return
	}
	if user == nil {
		redirectAdminFlash(w, r, "user_not_found")
		return
	}

	// Revoca PRIMA della DELETE: se non viene persistita, l'utente resta e
	// l'amministratore può ritentare. Dopo la DELETE non ci sarebbe più una
	// riga utente a cui riferirsi e un fallimento lascerebbe token validi
	// senza un percorso di recupero.
	if err := h.revokeUserSessions(r.Context(), userID); err != nil {
		log.Printf("admin: revoca sessioni prima dell'eliminazione non riuscita (utente %s): %v", userID, err)
		redirectAdminFlash(w, r, "delete_revoke_failed")
		return
	}

	if err := h.Users.Delete(r.Context(), userID); err != nil {
		log.Printf("admin: errore eliminazione utente %s: %v", userID, err)
		redirectAdminFlash(w, r, "delete_failed")
		return
	}

	redirectAdminFlash(w, r, "user_deleted")
}
