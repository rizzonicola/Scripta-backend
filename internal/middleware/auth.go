package middleware

import (
	"context"
	"net/http"
	"strings"

	"notes-server/internal/auth"
	"notes-server/internal/db"
	"notes-server/internal/httpx"
)

type ctxKey string

const (
	CtxUserID        ctxKey = "user_id"
	CtxUsername      ctxKey = "username"
	CtxClaims        ctxKey = "claims"
	CtxAccountStatus ctxKey = "account_status"
)

// BearerToken estrae il token dall'header Authorization. La sintassi
// "Bearer <token>" è una convenzione del protocollo: vive qui, non nei singoli
// handler che devono accettarla (middleware e logout).
func BearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	token := strings.TrimPrefix(header, prefix)
	return token, token != ""
}

// RequireJWT protegge gli endpoint API verificando l'header Authorization: Bearer <token>.
// Non avvia goroutine né mantiene stato tra le richieste: ParseToken è
// sincrono e a costo costante, quindi non c'è rischio di leak o di goroutine
// bloccate sotto carico concorrente.
func RequireJWT(tm *auth.TokenManager, users *db.UsersRepo) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tokenStr, ok := BearerToken(r)
			if !ok {
				httpx.WriteErrorCode(w, http.StatusUnauthorized, "token mancante", "missing_token")
				return
			}

			claims, err := tm.ParseToken(tokenStr)
			if err != nil {
				httpx.WriteErrorCode(w, http.StatusUnauthorized, "token non valido o scaduto", "invalid_token")
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
				httpx.WriteErrorCode(w, http.StatusInternalServerError, "errore interno", "internal_error")
				return
			}
			if !found {
				httpx.WriteErrorCode(w, http.StatusUnauthorized, "account non valido", "invalid_account")
				return
			}
			if status.Blocked {
				httpx.WriteErrorCode(w, http.StatusForbidden,
					"account bloccato dall'amministratore", "account_blocked")
				return
			}

			// Epoch della password: un token emesso prima di un reset password
			// non è valido, anche se il suo iat è successivo alla revoca (race
			// reset/login). I token emessi prima di questa versione non hanno
			// il claim: valgono come epoch 1, che è il default dello schema.
			effectiveVersion := claims.PasswordVersion
			if effectiveVersion == 0 {
				effectiveVersion = 1
			}
			if effectiveVersion != status.PasswordVersion {
				httpx.WriteErrorCode(w, http.StatusUnauthorized, "token non più valido", "revoked_token")
				return
			}

			// Revoca (logout / reset password / cancellazione utente):
			// lookup O(1) in RAM, senza query aggiuntiva. La cache è popolata
			// all'avvio dalla tabella persistente delle revoche (vedi
			// TokenManager.AttachStore), quindi la revoca resta valida anche
			// dopo un riavvio del server.
			if tm.IsRevoked(claims) {
				httpx.WriteErrorCode(w, http.StatusUnauthorized, "token revocato", "revoked_token")
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
			if err != nil || sm.Validate(r.Context(), cookie.Value) != nil {
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
