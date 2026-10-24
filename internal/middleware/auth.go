package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"notes-server/internal/auth"
	"notes-server/internal/db"
)

type ctxKey string

const (
	CtxUserID        ctxKey = "user_id"
	CtxUsername      ctxKey = "username"
	CtxClaims        ctxKey = "claims"
	CtxAccountStatus ctxKey = "account_status"
)

// writeJSONError scrive una risposta di errore JSON coerente con quella usata
// dal package handlers (stesso schema {"error": "..."}). Duplicata qui invece
// di importare "notes-server/internal/handlers" per evitare un import ciclico
// (handlers già importa middleware per UserIDFromContext).
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSONErrorCode(w, status, msg, "")
}

// writeJSONErrorCode aggiunge un codice stabile che i client possono usare
// per distinguere casi diversi con lo stesso status HTTP (es. account
// bloccato vs token scaduto, oppure rate limit vs errore transitorio).
func writeJSONErrorCode(w http.ResponseWriter, status int, msg, code string) {
	// charset esplicito: stessa motivazione di handlers.jsonContentType.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	body := struct {
		Error string `json:"error"`
		Code  string `json:"code,omitempty"`
	}{Error: msg, Code: code}
	_ = json.NewEncoder(w).Encode(body)
}

// RequireJWT protegge gli endpoint API verificando l'header Authorization: Bearer <token>.
// Non avvia goroutine né mantiene stato tra le richieste: ParseToken è
// sincrono e a costo costante, quindi non c'è rischio di leak o di goroutine
// bloccate sotto carico concorrente.
func RequireJWT(tm *auth.TokenManager, users *db.UsersRepo) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Authorization")
			if header == "" || !strings.HasPrefix(header, "Bearer ") {
				writeJSONErrorCode(w, http.StatusUnauthorized, "token mancante", "missing_token")
				return
			}
			tokenStr := strings.TrimPrefix(header, "Bearer ")

			claims, err := tm.ParseToken(tokenStr)
			if err != nil {
				writeJSONErrorCode(w, http.StatusUnauthorized, "token non valido o scaduto", "invalid_token")
				return
			}

			// Blocco account: il controllo è a DB (PK lookup) perché deve
			// avere effetto immediato anche sui token già emessi. Un account
			// eliminato non deve più poter usare un JWT ancora non scaduto.
			// Va PRIMA della revoca: così un client bloccato riceve il 403
			// "account_blocked" (e mostra il messaggio giusto) anche se
			// l'admin ha revocato i suoi token; dopo lo sblocco quegli stessi
			// token restano revocati e il client ottiene 401.
			status, found, err := users.GetAccountStatus(r.Context(), claims.UserID)
			if err != nil {
				writeJSONErrorCode(w, http.StatusInternalServerError, "errore interno", "internal_error")
				return
			}
			if !found {
				writeJSONErrorCode(w, http.StatusUnauthorized, "account non valido", "invalid_account")
				return
			}
			if status.Blocked {
				writeJSONErrorCode(w, http.StatusForbidden,
					"account bloccato dall'amministratore", "account_blocked")
				return
			}

			// Revoca (logout / reset password / cancellazione utente):
			// lookup O(1) in RAM, senza query aggiuntiva. La cache è popolata
			// all'avvio dalla tabella persistente delle revoche (vedi
			// TokenManager.AttachStore), quindi la revoca resta valida anche
			// dopo un riavvio del server.
			if tm.IsRevoked(claims) {
				writeJSONErrorCode(w, http.StatusUnauthorized, "token revocato", "revoked_token")
				return
			}

			ctx := context.WithValue(r.Context(), CtxUserID, claims.UserID)
			ctx = context.WithValue(ctx, CtxUsername, claims.Username)
			ctx = context.WithValue(ctx, CtxClaims, claims)
			ctx = context.WithValue(ctx, CtxAccountStatus, status)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// AccountStatusFromContext restituisce lo stato account caricato da RequireJWT.
func AccountStatusFromContext(ctx context.Context) (db.AccountStatus, bool) {
	s, ok := ctx.Value(CtxAccountStatus).(db.AccountStatus)
	return s, ok
}

// ClaimsFromContext restituisce le claims del token già validato da RequireJWT.
func ClaimsFromContext(ctx context.Context) (*auth.Claims, bool) {
	c, ok := ctx.Value(CtxClaims).(*auth.Claims)
	return c, ok
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
