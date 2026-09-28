package middleware

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
)

type nonceCtxKey struct{}

// NonceFromContext restituisce il nonce CSP generato per la richiesta
// corrente (vuoto fuori dalle pagine /admin). I template lo inseriscono in
// <style nonce="..."> e <script nonce="...">.
func NonceFromContext(ctx context.Context) string {
	n, _ := ctx.Value(nonceCtxKey{}).(string)
	return n
}

func newNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// SecurityHeaders imposta gli header di difesa in profondità su ogni
// risposta, anche quando Cloudflare è davanti (WAF/CDN non possono
// sostituire proprietà legate al rendering della singola risposta).
//
//   - Content-Security-Policy: sulle pagine /admin è basata su NONCE per
//     richiesta (script e stili inline solo se marcati col nonce; niente
//     'unsafe-inline'), form-action 'self', nessun framing. Sulle API JSON è
//     "default-src 'none'".
//   - Strict-Transport-Security: i browser lo ignorano su HTTP semplice, ma
//     dietro il tunnel TLS (Cloudflare) impone HTTPS per un anno.
//   - X-Frame-Options / frame-ancestors: anti-clickjacking.
//   - Cache-Control: no-store su /admin e /api (dati e sessioni).
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")

		switch {
		case r.URL.Path == "/admin" || strings.HasPrefix(r.URL.Path, "/admin/"):
			nonce, err := newNonce()
			if err != nil {
				http.Error(w, "errore interno", http.StatusInternalServerError)
				return
			}
			h.Set("Content-Security-Policy",
				"default-src 'none'; "+
					"script-src 'nonce-"+nonce+"'; "+
					"style-src 'nonce-"+nonce+"'; "+
					"img-src 'self' data:; "+
					"connect-src 'self'; "+
					"form-action 'self'; "+
					"base-uri 'none'; "+
					"frame-ancestors 'none'")
			h.Set("Cache-Control", "no-store")
			r = r.WithContext(context.WithValue(r.Context(), nonceCtxKey{}, nonce))
		default:
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			if strings.HasPrefix(r.URL.Path, "/api/") {
				h.Set("Cache-Control", "no-store")
			}
		}
		next.ServeHTTP(w, r)
	})
}
