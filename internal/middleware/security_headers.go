package middleware

import "net/http"

// SecurityHeaders imposta header di difesa in profondità economici da
// tenere comunque nel backend anche quando Cloudflare è davanti: WAF/CDN
// possono aggiungerne di propri, ma non è mai corretto fare affidamento
// solo sul livello di rete per proprietà che riguardano il rendering della
// singola risposta HTML (clickjacking, MIME sniffing).
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
