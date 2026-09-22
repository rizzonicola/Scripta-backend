package middleware

import (
	"context"
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

			// Revoca istantanea (reset password / cancellazione utente):
			// lookup O(1) in RAM, NESSUNA query a DB su questo percorso
			// caldo. Vedi il commento su TokenManager.Revoke per il motivo
			// per cui questo resta compatibile con la natura stateless di JWT.
			if claims.IssuedAt != nil && tm.IsRevoked(claims.UserID, claims.IssuedAt.Time) {
				writeJSONError(w, http.StatusUnauthorized, "token revocato")
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

// AdminSessionCookieName è il nome del cookie di sessione della dashboard
// /admin. Esportata da questo package (letto da SessionAuthAdmin per
// validarlo) e riusata identica da internal/handlers/admin.go (che lo
// scrive/cancella), per evitare che le due stringhe letterali finiscano per
// divergere.
const AdminSessionCookieName = "admin_session"

// SessionAuthAdmin protegge la dashboard /admin con un cookie di sessione
// firmato (HttpOnly, Secure, SameSite=Strict), generato dal login form-based
// di AdminHandler.Login dopo aver verificato ADMIN_USER/ADMIN_PASS.
//
// Sostituisce il precedente HTTP Basic Auth: a differenza del popup nativo
// del browser, un login via <form> standard è riconosciuto e proposto in
// salvataggio dai password manager (nativi o di terze parti), e permette
// un vero logout esplicito (impossibile con Basic Auth, le cui credenziali
// restano cachate dal browser finché non si chiude la finestra).
func SessionAuthAdmin(sm *auth.AdminSessionManager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie(AdminSessionCookieName)
			if err != nil || sm.Validate(cookie.Value) != nil {
				// Redirect (non 401 JSON): queste rotte sono navigate da
				// browser, non chiamate da un client API. Il redirect
				// riporta l'utente al login mantenendo l'esperienza
				// coerente con il resto della dashboard.
				http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
