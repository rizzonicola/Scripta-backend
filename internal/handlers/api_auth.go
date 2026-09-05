package handlers

import (
	"net/http"

	"notes-server/internal/auth"
	"notes-server/internal/db"
	"notes-server/internal/models"
)

type AuthHandler struct {
	Users  *db.UsersRepo
	Tokens *auth.TokenManager
}

func NewAuthHandler(users *db.UsersRepo, tokens *auth.TokenManager) *AuthHandler {
	return &AuthHandler{Users: users, Tokens: tokens}
}

// maxLoginBodyBytes limita il body di login: username+password non superano
// mai qualche decina di byte in un uso legittimo; 4 KiB è già ampiamente
// generoso e chiude ogni possibilità di allocazione incontrollata su questo
// endpoint pubblico (raggiungibile senza autenticazione).
const maxLoginBodyBytes = 4 << 10 // 4 KiB

// Login gestisce POST /api/v1/auth/login
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "metodo non consentito")
		return
	}

	var req models.LoginRequest
	if !decodeJSONBody(w, r, &req, maxLoginBodyBytes) {
		return
	}
	if req.Username == "" || req.Password == "" {
		writeJSONError(w, http.StatusBadRequest, "username e password sono obbligatori")
		return
	}

	user, err := h.Users.GetByUsername(r.Context(), req.Username)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "errore interno")
		return
	}
	if user == nil || !auth.CheckPassword(user.PasswordHash, req.Password) {
		writeJSONError(w, http.StatusUnauthorized, "credenziali non valide")
		return
	}

	token, expiresAt, err := h.Tokens.GenerateToken(user.ID, user.Username)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "errore nella generazione del token")
		return
	}

	writeJSON(w, http.StatusOK, models.LoginResponse{
		Token:     token,
		ExpiresAt: expiresAt,
		UserID:    user.ID,
		Username:  user.Username,
	})
}
