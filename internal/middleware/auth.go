package middleware

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"notes-server/internal/auth"
)

type ctxKey string

const (
	CtxUserID   ctxKey = "user_id"
	CtxUsername ctxKey = "username"
)

// writeJSONError scrive una risposta di errore JSON coerente con quella usata
// dal package handlers (stesso schema {"error": "..."}). Duplicata qui invece
// di importare "notes-server/internal/handlers" per evitare un import ciclico
// (handlers già importa middleware per UserIDFromContext).
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: msg})
}

// RequireJWT protegge gli endpoint API verificando l'header Authorization: Bearer <token>.
// Non avvia goroutine né mantiene stato tra le richieste: ParseToken è
// sincrono e a costo costante, quindi non c'è rischio di leak o di goroutine
// bloccate sotto carico concorrente.
func RequireJWT(tm *auth.TokenManager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Authorization")
			if header == "" || !strings.HasPrefix(header, "Bearer ") {
				writeJSONError(w, http.StatusUnauthorized, "token mancante")
				return
			}
			tokenStr := strings.TrimPrefix(header, "Bearer ")

			claims, err := tm.ParseToken(tokenStr)
			if err != nil {
				writeJSONError(w, http.StatusUnauthorized, "token non valido o scaduto")
				return
			}

			ctx := context.WithValue(r.Context(), CtxUserID, claims.UserID)
			ctx = context.WithValue(ctx, CtxUsername, claims.Username)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// UserIDFromContext estrae l'ID utente autenticato dal contesto della richiesta.
func UserIDFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(CtxUserID).(string)
	return v, ok
}

// BasicAuthAdmin protegge la dashboard /admin con HTTP Basic Auth,
// usando le credenziali definite dalle variabili d'ambiente ADMIN_USER / ADMIN_PASS.
func BasicAuthAdmin(adminUser, adminPass string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, pass, ok := r.BasicAuth()

			// Valutiamo ENTRAMBI i confronti a tempo costante incondizionatamente,
			// invece di combinarli con "||" (che andrebbe in short-circuit): con
			// "||", se lo username è già sbagliato il confronto della password
			// verrebbe saltato del tutto, rendendo il tempo di esecuzione
			// osservabile diverso a seconda che sia sbagliato lo username o la
			// password — un side-channel timing che vanifica in parte lo scopo
			// stesso di usare subtle.ConstantTimeCompare. Calcolando sempre
			// entrambi i risultati prima di combinarli con un semplice "&&"
			// booleano (non short-circuit su valori già calcolati), il tempo
			// impiegato non dipende da quale credenziale sia corretta.
			userOK := subtle.ConstantTimeCompare([]byte(user), []byte(adminUser)) == 1
			passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(adminPass)) == 1

			if !ok || !userOK || !passOK {
				w.Header().Set("WWW-Authenticate", `Basic realm="admin"`)
				writeJSONError(w, http.StatusUnauthorized, "non autorizzato")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
