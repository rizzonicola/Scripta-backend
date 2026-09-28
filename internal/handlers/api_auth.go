package handlers

import (
	"log"
	"net/http"
	"time"
	"unicode/utf8"

	"notes-server/internal/auth"
	"notes-server/internal/db"
	"notes-server/internal/middleware"
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
	// Username: trim + minuscolo (lookup case-insensitive). Un username
	// vuoto o più lungo del massimo non può esistere: stessa risposta e
	// stessa latenza di "credenziali errate", senza toccare il DB.
	username := auth.CanonicalUsername(req.Username)
	if username == "" || req.Password == "" {
		writeJSONError(w, http.StatusBadRequest, "username e password sono obbligatori")
		return
	}
	if utf8.RuneCountInString(username) > auth.MaxUsernameLen || len(req.Password) > auth.MaxPasswordBytes {
		sleepUntilMinResponseTime(start)
		writeJSONError(w, http.StatusUnauthorized, "credenziali non valide")
		return
	}

	user, err := h.Users.GetByUsername(r.Context(), username)
	if err != nil {
		log.Printf("login: errore lettura utente: %v", err)
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

// Logout gestisce POST /api/v1/auth/logout (protetto da JWT): revoca il token
// usato per la richiesta. La revoca è persistita su DB (vedi
// auth.TokenManager.AttachStore), quindi resta valida anche dopo un riavvio
// del server. Prima non esisteva alcun logout lato server: il token restava
// valido fino alla scadenza anche dopo il "logout" dell'app.
func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "metodo non consentito")
		return
	}
	claims, ok := middleware.ClaimsFromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "utente non autenticato")
		return
	}
	if err := h.Tokens.RevokeToken(r.Context(), claims); err != nil {
		log.Printf("logout: revoca token fallita: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "impossibile revocare il token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
