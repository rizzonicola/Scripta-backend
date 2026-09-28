package handlers

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"errors"
	"html/template"
	"log"
	"net/http"
	"time"

	"notes-server/internal/auth"
	"notes-server/internal/db"
	"notes-server/internal/middleware"
)

type AdminHandler struct {
	Users    *db.UsersRepo
	Tokens   *auth.TokenManager        // per revocare i JWT utente su reset password / cancellazione
	Sessions *auth.AdminSessionManager // firma/valida il cookie di sessione della dashboard

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
func NewAdminHandler(users *db.UsersRepo, tokens *auth.TokenManager, sessions *auth.AdminSessionManager, adminUser, adminPass string, templatesFS embed.FS) (*AdminHandler, error) {
	tmpl, err := template.ParseFS(templatesFS, "web/templates/*.html")
	if err != nil {
		return nil, err
	}
	return &AdminHandler{
		Users:     users,
		Tokens:    tokens,
		Sessions:  sessions,
		adminUser: adminUser,
		adminPass: adminPass,
		tmpl:      tmpl,
	}, nil
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
	"login_invalid":               {"Credenziali non valide", true},
	"bad_request":                 {"Richiesta non valida", true},
	"internal":                    {"Errore interno, riprovare", true},
	"fields_required":             {"Username e password sono obbligatori", true},
	"username_invalid":            {"Username non valido: 3-64 caratteri, senza spazi", true},
	"password_short":              {"La password deve avere almeno 8 caratteri", true},
	"password_long":               {"La password non può superare i 72 byte", true},
	"user_exists":                 {"Impossibile creare l'utente: username già in uso", true},
	"user_create_failed":          {"Impossibile creare l'utente", true},
	"user_created":                {"Utente creato con successo", false},
	"reset_fields_required":       {"Utente e nuova password sono obbligatori", true},
	"user_not_found":              {"Utente non trovato", true},
	"reset_failed":                {"Errore nel reset della password", true},
	"password_updated":            {"Password aggiornata con successo", false},
	"password_updated_revoke_err": {"Password aggiornata, ma la revoca persistente dei token attivi non è stata salvata (vedere i log del server)", true},
	"user_id_required":            {"ID utente obbligatorio", true},
	"delete_failed":               {"Errore nell'eliminazione dell'utente", true},
	"user_deleted":                {"Utente e tutti i suoi dati (cartelle e note) eliminati definitivamente", false},
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
	http.Redirect(w, r, path+"?f="+template.URLQueryEscaper(code), http.StatusSeeOther)
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
	ID        string
	Username  string
	CreatedAt string
}

type usersPageData struct {
	Nonce        string
	Users        []userView
	Flash        string
	FlashIsError bool
}

// UsersPage gestisce GET /admin -> elenco utenti + form nuovo utente.
func (h *AdminHandler) UsersPage(w http.ResponseWriter, r *http.Request) {
	msg, isErr := flashFromRequest(r)

	users, err := h.Users.List(r.Context())
	if err != nil {
		// Il dettaglio dell'errore DB resta nei log: mai mostrato nella UI.
		log.Printf("admin: errore caricamento utenti: %v", err)
		http.Error(w, "errore interno", http.StatusInternalServerError)
		return
	}

	views := make([]userView, 0, len(users))
	for _, u := range users {
		views = append(views, userView{
			ID:        u.ID,
			Username:  u.Username,
			CreatedAt: time.UnixMilli(u.CreatedAt).Format("2006-01-02 15:04"),
		})
	}

	data := usersPageData{Nonce: middleware.NonceFromContext(r.Context()), Users: views, Flash: msg, FlashIsError: isErr}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "users.html", data); err != nil {
		log.Printf("admin: errore rendering dashboard: %v", err)
		http.Error(w, "errore interno", http.StatusInternalServerError)
	}
}

// passwordFlashCode traduce un errore di validazione password nel codice flash.
func passwordFlashCode(err error) string {
	if errors.Is(err, auth.ErrPasswordTooLong) {
		return "password_long"
	}
	return "password_short"
}

// CreateUser gestisce POST /admin/users/create. L'username viene normalizzato
// (trim + minuscolo + limiti di lunghezza); la password in chiaro viene
// cifrata subito con bcrypt e mai più mostrata né salvata.
func (h *AdminHandler) CreateUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "metodo non consentito", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		redirectFlash(w, r, "/admin", "bad_request")
		return
	}

	rawUsername := r.FormValue("username")
	password := r.FormValue("password")
	if rawUsername == "" || password == "" {
		redirectFlash(w, r, "/admin", "fields_required")
		return
	}
	username, err := auth.NormalizeUsername(rawUsername)
	if err != nil {
		redirectFlash(w, r, "/admin", "username_invalid")
		return
	}
	if err := auth.ValidatePassword(password); err != nil {
		redirectFlash(w, r, "/admin", passwordFlashCode(err))
		return
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		log.Printf("admin: errore hashing password: %v", err)
		redirectFlash(w, r, "/admin", "internal")
		return
	}

	if _, err := h.Users.Create(r.Context(), username, hash); err != nil {
		if errors.Is(err, db.ErrUsernameTaken) {
			redirectFlash(w, r, "/admin", "user_exists")
			return
		}
		log.Printf("admin: errore creazione utente: %v", err)
		redirectFlash(w, r, "/admin", "user_create_failed")
		return
	}

	redirectFlash(w, r, "/admin", "user_created")
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
		redirectFlash(w, r, "/admin", "bad_request")
		return
	}

	userID := r.FormValue("user_id")
	newPassword := r.FormValue("new_password")
	if userID == "" || newPassword == "" {
		redirectFlash(w, r, "/admin", "reset_fields_required")
		return
	}
	if err := auth.ValidatePassword(newPassword); err != nil {
		redirectFlash(w, r, "/admin", passwordFlashCode(err))
		return
	}

	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		log.Printf("admin: errore hashing password: %v", err)
		redirectFlash(w, r, "/admin", "internal")
		return
	}

	if err := h.Users.UpdatePassword(r.Context(), userID, hash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			redirectFlash(w, r, "/admin", "user_not_found")
			return
		}
		log.Printf("admin: errore reset password: %v", err)
		redirectFlash(w, r, "/admin", "reset_failed")
		return
	}

	// Un JWT emesso prima del reset non deve restare valido fino alla sua
	// scadenza naturale. La revoca è persistita su DB (sopravvive al
	// riavvio): se il salvataggio fallisce lo si segnala all'amministratore.
	if h.Tokens != nil {
		if err := h.Tokens.RevokeUser(r.Context(), userID); err != nil {
			log.Printf("admin: revoca token dopo reset password non persistita (utente %s): %v", userID, err)
			redirectFlash(w, r, "/admin", "password_updated_revoke_err")
			return
		}
	}

	redirectFlash(w, r, "/admin", "password_updated")
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
		redirectFlash(w, r, "/admin", "bad_request")
		return
	}

	userID := r.FormValue("user_id")
	if userID == "" {
		redirectFlash(w, r, "/admin", "user_id_required")
		return
	}

	user, err := h.Users.GetByID(r.Context(), userID)
	if err != nil {
		log.Printf("admin: errore lettura utente %s: %v", userID, err)
		redirectFlash(w, r, "/admin", "internal")
		return
	}
	if user == nil {
		redirectFlash(w, r, "/admin", "user_not_found")
		return
	}

	if err := h.Users.Delete(r.Context(), userID); err != nil {
		log.Printf("admin: errore eliminazione utente %s: %v", userID, err)
		redirectFlash(w, r, "/admin", "delete_failed")
		return
	}

	// Un JWT già emesso per l'utente cancellato non deve restare accettato.
	if h.Tokens != nil {
		if err := h.Tokens.RevokeUser(r.Context(), userID); err != nil {
			log.Printf("admin: revoca token dopo eliminazione non persistita (utente %s): %v", userID, err)
		}
	}

	redirectFlash(w, r, "/admin", "user_deleted")
}
