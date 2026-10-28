package handlers

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

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
	Users         *db.UsersRepo
	Tokens        *auth.TokenManager
	RefreshTokens *db.RefreshTokensRepo
	RefreshTTL    time.Duration
}

func NewAuthHandler(users *db.UsersRepo, tokens *auth.TokenManager, refreshTokens *db.RefreshTokensRepo, refreshTTL time.Duration) *AuthHandler {
	if refreshTTL <= 0 {
		refreshTTL = 90 * 24 * time.Hour
	}
	return &AuthHandler{
		Users:         users,
		Tokens:        tokens,
		RefreshTokens: refreshTokens,
		RefreshTTL:    refreshTTL,
	}
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

	// Il floor parte DOPO la lettura del body: un client lento non deve poter
	// consumare il tempo minimo e rendere osservabile la differenza tra un
	// utente esistente (bcrypt) e uno inesistente (solo query).
	start := time.Now()

	// Username: solo trim, il case è significativo. Un username vuoto o più
	// lungo del massimo non può esistere: stessa risposta e stessa latenza di
	// "credenziali errate", senza toccare il DB.
	username := auth.LoginUsername(req.Username)
	if username == "" || req.Password == "" {
		writeJSONError(w, http.StatusBadRequest, "username e password sono obbligatori")
		return
	}
	// Il driver libSQL converte le stringhe in C string: un NUL verrebbe
	// troncato e "mario\x00x" autenticherebbe "mario". Il regex di creazione
	// lo impedisce, ma il login non passa da NormalizeUsername.
	if strings.IndexByte(username, 0) >= 0 {
		sleepUntilMinResponseTime(start)
		writeJSONErrorCode(w, http.StatusUnauthorized, "credenziali non valide", "invalid_credentials")
		return
	}
	if utf8.RuneCountInString(username) > auth.MaxUsernameLen || len(req.Password) > auth.MaxPasswordBytes {
		sleepUntilMinResponseTime(start)
		writeJSONErrorCode(w, http.StatusUnauthorized, "credenziali non valide", "invalid_credentials")
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
		writeJSONErrorCode(w, http.StatusUnauthorized, "credenziali non valide", "invalid_credentials")
		return
	}

	// Il blocco viene rivelato SOLO dopo password corretta: un tentativo con
	// password sbagliata resta indistinguibile da un account inesistente e
	// non permette di scoprire quali username sono bloccati.
	if user.BlockedAt != nil {
		writeJSONErrorCode(w, http.StatusForbidden,
			"account bloccato dall'amministratore", "account_blocked")
		return
	}

	token, expiresAt, err := h.Tokens.GenerateToken(user.ID, user.Username)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "errore nella generazione del token")
		return
	}

	now := time.Now()
	refreshToken, _, refreshExpiresAt, err := h.createRefreshToken(r, user.ID, auth.NewRefreshFamilyID(), now)
	if err != nil {
		log.Printf("login: creazione refresh token fallita: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "errore nella generazione della sessione")
		return
	}

	writeJSON(w, http.StatusOK, models.LoginResponse{
		Token:            token,
		ExpiresAt:        expiresAt,
		RefreshToken:     refreshToken,
		RefreshExpiresAt: refreshExpiresAt.Unix(),
		UserID:           user.ID,
		Username:         user.Username,
	})
}

// createRefreshToken genera e persiste un nuovo refresh token nella famiglia
// indicata. Restituisce anche il valore in chiaro (da consegnare al client) e
// la scadenza.
func (h *AuthHandler) createRefreshToken(r *http.Request, userID, familyID string, now time.Time) (string, string, time.Time, error) {
	raw, hash, err := auth.NewRefreshToken()
	if err != nil {
		return "", "", time.Time{}, err
	}
	expiresAt := now.Add(h.RefreshTTL)
	id := uuid.NewString()
	err = h.RefreshTokens.Create(r.Context(), db.RefreshToken{
		ID:        id,
		UserID:    userID,
		FamilyID:  familyID,
		TokenHash: hash,
		IssuedAt:  now.UnixMilli(),
		ExpiresAt: expiresAt.UnixMilli(),
	})
	if err != nil {
		return "", "", time.Time{}, err
	}
	return raw, id, expiresAt, nil
}

// Refresh gestisce POST /api/v1/auth/refresh. Ruota il refresh token: ogni
// valore è monouso. Il riuso di un token già ruotato revoca l'intera
// famiglia, perché significa che una copia è finita nelle mani sbagliate.
func (h *AuthHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "metodo non consentito")
		return
	}

	var req models.RefreshRequest
	if !decodeJSONBody(w, r, &req, maxLoginBodyBytes) {
		return
	}
	raw := strings.TrimSpace(req.RefreshToken)
	if raw == "" {
		writeJSONErrorCode(w, http.StatusUnauthorized, "refresh token non valido", "invalid_grant")
		return
	}

	current, err := h.RefreshTokens.GetByHash(r.Context(), auth.HashRefreshToken(raw))
	if err != nil {
		log.Printf("refresh: lettura token fallita: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "errore interno")
		return
	}
	if current == nil {
		writeJSONErrorCode(w, http.StatusUnauthorized, "refresh token non valido", "invalid_grant")
		return
	}

	now := time.Now()
	if current.RevokedAt != nil || current.ReplacedBy != nil || current.ExpiresAt <= now.UnixMilli() {
		// Riuso (o token scaduto): la famiglia non è più affidabile.
		if err := h.RefreshTokens.RevokeFamily(r.Context(), current.FamilyID, now); err != nil {
			log.Printf("refresh: revoca famiglia dopo riuso fallita: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "errore interno")
			return
		}
		writeJSONErrorCode(w, http.StatusUnauthorized, "refresh token non valido", "invalid_grant")
		return
	}

	user, err := h.Users.GetByID(r.Context(), current.UserID)
	if err != nil {
		log.Printf("refresh: lettura utente fallita: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "errore interno")
		return
	}
	if user == nil {
		_ = h.RefreshTokens.RevokeFamily(r.Context(), current.FamilyID, now)
		writeJSONErrorCode(w, http.StatusUnauthorized, "refresh token non valido", "invalid_grant")
		return
	}
	if user.BlockedAt != nil {
		_ = h.RefreshTokens.RevokeFamily(r.Context(), current.FamilyID, now)
		writeJSONErrorCode(w, http.StatusForbidden,
			"account bloccato dall'amministratore", "account_blocked")
		return
	}
	if h.Tokens.IsUserRevoked(current.UserID, time.UnixMilli(current.IssuedAt)) {
		_ = h.RefreshTokens.RevokeFamily(r.Context(), current.FamilyID, now)
		writeJSONErrorCode(w, http.StatusUnauthorized, "refresh token non valido", "invalid_grant")
		return
	}

	token, expiresAt, err := h.Tokens.GenerateToken(user.ID, user.Username)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "errore nella generazione del token")
		return
	}

	nextRaw, nextHash, err := auth.NewRefreshToken()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "errore nella generazione della sessione")
		return
	}
	next := db.RefreshToken{
		ID:        uuid.NewString(),
		UserID:    user.ID,
		FamilyID:  current.FamilyID,
		TokenHash: nextHash,
		IssuedAt:  now.UnixMilli(),
		ExpiresAt: now.Add(h.RefreshTTL).UnixMilli(),
	}
	if err := h.RefreshTokens.Rotate(r.Context(), current.ID, next, now); err != nil {
		if errors.Is(err, db.ErrRefreshTokenInactive) {
			_ = h.RefreshTokens.RevokeFamily(r.Context(), current.FamilyID, now)
			writeJSONErrorCode(w, http.StatusUnauthorized, "refresh token non valido", "invalid_grant")
			return
		}
		log.Printf("refresh: rotazione fallita: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "errore interno")
		return
	}

	writeJSON(w, http.StatusOK, models.LoginResponse{
		Token:            token,
		ExpiresAt:        expiresAt,
		RefreshToken:     nextRaw,
		RefreshExpiresAt: next.ExpiresAt / 1000,
		UserID:           user.ID,
		Username:         user.Username,
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
	// Il logout non passa da RequireJWT: deve funzionare anche quando
	// l'access token è scaduto, purché il client presenti il refresh token
	// della famiglia. La presenza di uno dei due è la credenziale.
	var claims *auth.Claims
	if header := r.Header.Get("Authorization"); strings.HasPrefix(header, "Bearer ") {
		claims, _ = h.Tokens.ParseToken(strings.TrimPrefix(header, "Bearer "))
	}

	var req models.LogoutRequest
	if !decodeOptionalJSONBody(w, r, &req, maxLoginBodyBytes) {
		return
	}

	var current *db.RefreshToken
	if raw := strings.TrimSpace(req.RefreshToken); raw != "" {
		rt, err := h.RefreshTokens.GetByHash(r.Context(), auth.HashRefreshToken(raw))
		if err != nil {
			log.Printf("logout: lettura refresh token fallita: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "impossibile revocare la sessione")
			return
		}
		current = rt
	}

	if claims == nil && current == nil {
		writeJSONErrorCode(w, http.StatusUnauthorized, "utente non autenticato", "invalid_token")
		return
	}

	// Un access token valido non deve poter revocare la famiglia di un altro
	// utente presentando un refresh token che non gli appartiene. Senza
	// access token (scaduto) il possesso del refresh token è sufficiente.
	if current != nil && (claims == nil || current.UserID == claims.UserID) {
		if err := h.RefreshTokens.RevokeFamily(r.Context(), current.FamilyID, time.Now()); err != nil {
			log.Printf("logout: revoca famiglia fallita: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "impossibile revocare la sessione")
			return
		}
	}
	if claims != nil {
		if err := h.Tokens.RevokeToken(r.Context(), claims); err != nil {
			log.Printf("logout: revoca token fallita: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "impossibile revocare il token")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// LogoutAll gestisce POST /api/v1/auth/logout-all: chiude tutte le sessioni
// dell'utente, su ogni dispositivo, incluso quello corrente.
func (h *AuthHandler) LogoutAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "metodo non consentito")
		return
	}
	claims, ok := middleware.ClaimsFromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "utente non autenticato")
		return
	}
	now := time.Now()
	if err := h.RefreshTokens.RevokeAllForUser(r.Context(), claims.UserID, now); err != nil {
		log.Printf("logout-all: revoca refresh token fallita: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "impossibile revocare le sessioni")
		return
	}
	if err := h.Tokens.RevokeUser(r.Context(), claims.UserID); err != nil {
		log.Printf("logout-all: revoca JWT fallita: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "impossibile revocare le sessioni")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
