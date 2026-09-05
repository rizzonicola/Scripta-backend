package handlers

import (
	"database/sql"
	"embed"
	"html/template"
	"net/http"
	"time"

	"notes-server/internal/auth"
	"notes-server/internal/db"
)

type AdminHandler struct {
	Users *db.UsersRepo
	tmpl  *template.Template
}

// NewAdminHandler carica i template HTML dal filesystem embedded.
func NewAdminHandler(users *db.UsersRepo, templatesFS embed.FS) (*AdminHandler, error) {
	tmpl, err := template.ParseFS(templatesFS, "web/templates/*.html")
	if err != nil {
		return nil, err
	}
	return &AdminHandler{Users: users, tmpl: tmpl}, nil
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
		redirectFlash(w, r, "Username e password sono obbligatori", true)
		return
	}
	if len(password) < 8 {
		redirectFlash(w, r, "La password deve avere almeno 8 caratteri", true)
		return
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		redirectFlash(w, r, "Errore nella cifratura della password", true)
		return
	}
	// A questo punto 'password' in chiaro non serve più: viene scartata (garbage collected).

	if _, err := h.Users.Create(r.Context(), username, hash); err != nil {
		redirectFlash(w, r, "Impossibile creare l'utente (username già esistente?)", true)
		return
	}

	redirectFlash(w, r, "Utente '"+username+"' creato con successo", false)
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
		redirectFlash(w, r, "Utente e nuova password sono obbligatori", true)
		return
	}
	if len(newPassword) < 8 {
		redirectFlash(w, r, "La password deve avere almeno 8 caratteri", true)
		return
	}

	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		redirectFlash(w, r, "Errore nella cifratura della password", true)
		return
	}

	if err := h.Users.UpdatePassword(r.Context(), userID, hash); err != nil {
		if err == sql.ErrNoRows {
			redirectFlash(w, r, "Utente non trovato", true)
			return
		}
		redirectFlash(w, r, "Errore nel reset della password", true)
		return
	}

	redirectFlash(w, r, "Password aggiornata con successo", false)
}

func redirectFlash(w http.ResponseWriter, r *http.Request, msg string, isErr bool) {
	q := "?flash=" + template.URLQueryEscaper(msg)
	if isErr {
		q += "&err=1"
	}
	http.Redirect(w, r, "/admin"+q, http.StatusSeeOther)
}
