package middleware

import "net/http"

// RequireSameOrigin blocca le richieste POST che non provengono dalla stessa
// origin del sito, come difesa CSRF per le rotte /admin/* protette da
// cookie di sessione.
//
// Deliberatamente STATELESS: nessun token da generare/salvare/invalidare
// lato server (niente mappe in-memory che si romperebbero con schede
// multiple, pulsante "Indietro", o riavvio/scaling del processo). Il
// controllo si basa su due header che il browser stesso allega alla
// richiesta, in ordine di preferenza:
//
//  1. Sec-Fetch-Site: inviato da tutti i browser moderni (Fetch Metadata),
//     NON disattivabile da estensioni privacy (a differenza di Referer) e
//     non falsificabile da pagine web. "same-origin" è l'unico valore che
//     accettiamo per una POST.
//  2. Origin: fallback per client che non inviano Fetch Metadata. Viene
//     confrontato con l'host della richiesta stessa (r.Host, valorizzato
//     correttamente anche dietro Cloudflare Tunnel), quindi non serve
//     configurare alcun dominio fisso: funziona automaticamente su
//     qualunque hostname pubblico sia stato configurato per il tunnel.
//
// Se nessuno dei due header è presente la richiesta viene RIFIUTATA: un
// client che parla con la dashboard deve dichiarare la propria origine. I
// browser moderni inviano sempre almeno uno dei due header; curl e altri
// client non-browser non sono il target di /admin e possono essere adattati.
func RequireSameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			site := r.Header.Get("Sec-Fetch-Site")
			origin := r.Header.Get("Origin")
			if site != "" {
				if site != "same-origin" {
					writeJSONError(w, http.StatusForbidden, "richiesta cross-site non consentita")
					return
				}
			} else if origin != "" {
				expectedHTTPS := "https://" + r.Host
				expectedHTTP := "http://" + r.Host
				if origin != expectedHTTPS && origin != expectedHTTP {
					writeJSONError(w, http.StatusForbidden, "origin non consentita")
					return
				}
			} else {
				writeJSONError(w, http.StatusForbidden, "origine della richiesta non verificabile")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
