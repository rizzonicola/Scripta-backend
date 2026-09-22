package handlers

import (
	"net/http"
	"time"

	"notes-server/internal/auth"
	"notes-server/internal/db"
	"notes-server/internal/models"
)

// minLoginResponseTime appiattisce la differenza di tempo osservabile tra
// "username inesistente" (una sola query DB, quasi gratis) e "username
// esistente ma password sbagliata" (query + bcrypt, qualche decina di ms),
// che altrimenti permetterebbe la user enumeration via timing.
//
// Deliberatamente un floor con time.Sleep e NON un bcrypt.CompareHashAndPassword
// contro un hash "decoy" quando l'utente non esiste: quell'approccio,
// proposto in un primo momento, trasformerebbe ogni tentativo di login con
// username inventato (oggi quasi gratuito) in uno che consuma sempre un
// hash bcrypt completo, dando a un attaccante un moltiplicatore di costo
// enorme per un DoS volumetrico — un rischio più concreto del timing leak
// che dovrebbe mitigare. time.Sleep blocca la sola goroutine su un timer
// (nessun uso di CPU in busy-loop), quindi non è amplificabile allo stesso
// modo. Resta comunque una misura di secondo piano: la difesa primaria
// contro il volume di tentativi è la Cloudflare WAF Rate Limiting Rule a
// monte (vedi main.go).
const minLoginResponseTime = 100 * time.Millisecond

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

	start := time.Now()

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
		sleepUntilMinResponseTime(start)
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

// sleepUntilMinResponseTime attende, se necessario, fino a raggiungere
// minLoginResponseTime dall'istante di partenza start. Se il tentativo era
// già più lento del floor (caso tipico: utente esistente, bcrypt già speso)
// non attende affatto.
func sleepUntilMinResponseTime(start time.Time) {
	if elapsed := time.Since(start); elapsed < minLoginResponseTime {
		time.Sleep(minLoginResponseTime - elapsed)
	}
}
