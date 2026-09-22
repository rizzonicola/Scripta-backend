package handlers

import (
	"crypto/subtle"
	"database/sql"
	"embed"
	"html/template"
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
// A differenza della generazione precedente non serve più alcun riferimento
// allo storage su filesystem: cartelle e note vivono interamente nel
// database (colonna "content" per il testo), quindi cancellare un utente è
// una singola operazione a DB, propagata automaticamente dalle foreign key
// ON DELETE CASCADE su folders/notes/user_settings (vedi DeleteUser sotto e
// lo schema in internal/db/db.go).
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

// constantTimeEqual confronta due stringhe in tempo costante. Vedi il
// commento storico (ex BasicAuthAdmin) sul perché entrambi i confronti
// vanno sempre valutati con "&" e non in short-circuit con "&&" su
// espressioni booleane già note: qui i due bool sono già calcolati prima di
// essere combinati, quindi il tempo non dipende da quale credenziale sia
// sbagliata.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

type loginPageData struct {
	Flash        string
	FlashIsError bool
}

// LoginPage gestisce GET /admin/login: mostra il form di accesso. Se una
// sessione valida è già presente (cookie ancora non scaduto), salta
// direttamente alla dashboard invece di mostrare di nuovo il login.
func (h *AdminHandler) LoginPage(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(middleware.AdminSessionCookieName); err == nil && h.Sessions.Validate(c.Value) == nil {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	h.renderLoginPage(w, r.URL.Query().Get("flash"), r.URL.Query().Get("err") == "1")
}

func (h *AdminHandler) renderLoginPage(w http.ResponseWriter, flash string, isErr bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := loginPageData{Flash: flash, FlashIsError: isErr}
	if err := h.tmpl.ExecuteTemplate(w, "admin_login.html", data); err != nil {
		http.Error(w, "errore rendering: "+err.Error(), http.StatusInternalServerError)
	}
}

// Login gestisce POST /admin/login: verifica ADMIN_USER/ADMIN_PASS (stesse
// variabili d'ambiente di prima, solo il meccanismo di verifica cambia da
// header Basic Auth a form POST) ed emette il cookie di sessione.
//
// A differenza di HTTP Basic Auth, il fallimento qui riporta l'utente sul
// form di login con un messaggio d'errore invece che riaprire il popup
// nativo del browser: è compito del rate limiter (vedi main.go, applicato a
// questa rotta) e della Cloudflare WAF Rate Limiting Rule a monte limitare
// i tentativi ripetuti.
func (h *AdminHandler) Login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirectFlash(w, r, "/admin/login", "Richiesta non valida", true)
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")

	userOK := constantTimeEqual(username, h.adminUser)
	passOK := constantTimeEqual(password, h.adminPass)
	if !userOK || !passOK {
		redirectFlash(w, r, "/admin/login", "Credenziali non valide", true)
		return
	}

	token, expiresAt, err := h.Sessions.GenerateSession()
	if err != nil {
		redirectFlash(w, r, "/admin/login", "Errore interno, riprovare", true)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     middleware.AdminSessionCookieName,
		Value:    token,
		Path:     adminSessionCookiePath,
		Expires:  expiresAt,
		HttpOnly: true, // mai leggibile da JS: mitiga furto via XSS
		Secure:   true, // il browser lo invia solo su https (Cloudflare termina sempre TLS all'edge)
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// Logout gestisce POST /admin/logout: cancella il cookie di sessione.
// Un vero logout esplicito non era possibile con il precedente HTTP Basic
// Auth (le credenziali restavano cachate dal browser finché non si
// chiudeva la finestra): con un cookie di sessione, invece, è immediato.
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
	Users        []userView
	Flash        string
	FlashIsError bool
}

// UsersPage gestisce GET /admin -> elenco utenti + form nuovo utente.
func (h *AdminHandler) UsersPage(w http.ResponseWriter, r *http.Request) {
	flash := r.URL.Query().Get("flash")
	isErr := r.URL.Query().Get("err") == "1"

	users, err := h.Users.List(r.Context())
	if err != nil {
		http.Error(w, "errore caricamento utenti: "+err.Error(), http.StatusInternalServerError)
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

	data := usersPageData{Users: views, Flash: flash, FlashIsError: isErr}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "users.html", data); err != nil {
		http.Error(w, "errore rendering: "+err.Error(), http.StatusInternalServerError)
	}
}

// CreateUser gestisce POST /admin/users/create.
// La password in chiaro arriva dal form, viene immediatamente cifrata con bcrypt
// e non viene mai più mostrata né salvata in chiaro.
func (h *AdminHandler) CreateUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "metodo non consentito", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "form non valido", http.StatusBadRequest)
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")
	if username == "" || password == "" {
		redirectFlash(w, r, "/admin", "Username e password sono obbligatori", true)
		return
	}
	if len(password) < 8 {
		redirectFlash(w, r, "/admin", "La password deve avere almeno 8 caratteri", true)
		return
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		redirectFlash(w, r, "/admin", "Errore nella cifratura della password", true)
		return
	}
	// A questo punto 'password' in chiaro non serve più: viene scartata (garbage collected).

	if _, err := h.Users.Create(r.Context(), username, hash); err != nil {
		redirectFlash(w, r, "/admin", "Impossibile creare l'utente (username già esistente?)", true)
		return
	}

	redirectFlash(w, r, "/admin", "Utente '"+username+"' creato con successo", false)
}

// ResetPassword gestisce POST /admin/users/reset-password.
// Non è mai possibile visualizzare la password precedente: viene solo sostituito l'hash.
func (h *AdminHandler) ResetPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "metodo non consentito", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "form non valido", http.StatusBadRequest)
		return
	}

	userID := r.FormValue("user_id")
	newPassword := r.FormValue("new_password")
	if userID == "" || newPassword == "" {
		redirectFlash(w, r, "/admin", "Utente e nuova password sono obbligatori", true)
		return
	}
	if len(newPassword) < 8 {
		redirectFlash(w, r, "/admin", "La password deve avere almeno 8 caratteri", true)
		return
	}

	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		redirectFlash(w, r, "/admin", "Errore nella cifratura della password", true)
		return
	}

	if err := h.Users.UpdatePassword(r.Context(), userID, hash); err != nil {
		if err == sql.ErrNoRows {
			redirectFlash(w, r, "/admin", "Utente non trovato", true)
			return
		}
		redirectFlash(w, r, "/admin", "Errore nel reset della password", true)
		return
	}

	// Un JWT emesso prima del reset non deve restare valido fino alla sua
	// scadenza naturale: senza questa riga, un token rubato sopravviverebbe
	// al "logout forzato" che l'admin pensa di star facendo. Vedi il
	// commento su TokenManager.Revoke per il perché questo resta un lookup
	// O(1) in RAM e non una query DB ad ogni richiesta successiva.
	if h.Tokens != nil {
		h.Tokens.Revoke(userID)
	}

	redirectFlash(w, r, "/admin", "Password aggiornata con successo", false)
}

// DeleteUser gestisce POST /admin/users/delete: eliminazione sicura e
// irreversibile di un utente. Cancellare la riga utente dal database è
// l'UNICA operazione necessaria: le foreign key ON DELETE CASCADE su
// folders, notes e user_settings (vedi schema in internal/db/db.go)
// ripuliscono automaticamente e atomicamente tutti i dati associati.
// Nessun filesystem da ripulire separatamente, quindi nessuna finestra in
// cui i dati potrebbero risultare cancellati a metà (o dal DB ma non dal
// disco, come nella generazione precedente basata su file).
//
// La conferma "sei sicuro?" è responsabilità del template (modale JS lato
// client, vedi web/templates/users.html): questo handler esegue la
// cancellazione non appena riceve la richiesta POST, assumendo che il
// consenso sia già stato raccolto dall'interfaccia.
func (h *AdminHandler) DeleteUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "metodo non consentito", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "form non valido", http.StatusBadRequest)
		return
	}

	userID := r.FormValue("user_id")
	if userID == "" {
		redirectFlash(w, r, "/admin", "ID utente obbligatorio", true)
		return
	}

	user, err := h.Users.GetByID(r.Context(), userID)
	if err != nil {
		redirectFlash(w, r, "/admin", "Errore nel recupero dell'utente: "+err.Error(), true)
		return
	}
	if user == nil {
		redirectFlash(w, r, "/admin", "Utente non trovato", true)
		return
	}

	if err := h.Users.Delete(r.Context(), userID); err != nil {
		redirectFlash(w, r, "/admin", "Errore nell'eliminazione dell'utente dal database: "+err.Error(), true)
		return
	}

	// Come nel reset password: un JWT già emesso per l'utente cancellato non
	// deve restare accettato fino a scadenza naturale.
	if h.Tokens != nil {
		h.Tokens.Revoke(userID)
	}

	redirectFlash(w, r, "/admin", "Utente '"+user.Username+"' e tutti i suoi dati (cartelle e note) sono stati eliminati definitivamente", false)
}

// redirectFlash reindirizza a path con un messaggio flash in query string
// (letto e mostrato dal template corrispondente: users.html per "/admin",
// admin_login.html per "/admin/login").
func redirectFlash(w http.ResponseWriter, r *http.Request, path, msg string, isErr bool) {
	q := "?flash=" + template.URLQueryEscaper(msg)
	if isErr {
		q += "&err=1"
	}
	http.Redirect(w, r, path+q, http.StatusSeeOther)
}
