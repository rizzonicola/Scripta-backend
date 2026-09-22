package middleware

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// RateLimit è pensata come SECONDA linea di difesa su login/admin: la difesa
// primaria contro credential stuffing e brute force resta la Cloudflare WAF
// Rate Limiting Rule configurata davanti al tunnel (valutata al edge, prima
// ancora che la richiesta raggiunga questo processo, e senza consumare CPU
// locale). Questo middleware protegge comunque l'istanza in caso di
// misconfigurazione/bypass del WAF, o quando l'app viene eseguita senza
// Cloudflare davanti (es. sviluppo locale).
//
// Implementazione volutamente senza dipendenze esterne (niente
// golang.org/x/time/rate): un semplice token bucket per IP.
func RateLimit(ratePerSec, burst float64) func(http.Handler) http.Handler {
	limiter := newIPRateLimiter(ratePerSec, burst, 30*time.Minute)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !limiter.allow(clientIP(r)) {
				w.Header().Set("Retry-After", "5")
				writeJSONError(w, http.StatusTooManyRequests, "troppi tentativi, riprova più tardi")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// clientIP estrae l'IP reale del client. Dietro Cloudflare Tunnel,
// r.RemoteAddr è SEMPRE l'IP del processo cloudflared locale (o localhost):
// usarlo direttamente farebbe collassare tutti gli utenti su un unico
// bucket, bloccandoli o sbloccandoli tutti insieme. L'unico IP affidabile è
// quello che l'edge Cloudflare stesso inietta nell'header Cf-Connecting-IP,
// non falsificabile dal client finché l'origin accetta traffico SOLO dal
// tunnel (nessuna porta esposta direttamente su Internet).
func clientIP(r *http.Request) string {
	if ip := r.Header.Get("Cf-Connecting-IP"); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateLimitEntry è lo stato del token bucket di un singolo IP.
type rateLimitEntry struct {
	mu       sync.Mutex
	tokens   float64
	lastSeen time.Time
}

// ipRateLimiter implementa un token bucket per IP con refill continuo
// (non a finestre fisse: evita il classico "doppio burst" a cavallo di due
// finestre adiacenti).
//
// Pulizia: la mappa NON viene mai svuotata per intero. Un ticker periodico
// rimuove SOLO le singole entry inattive da più di staleAfter, così la
// mappa non cresce indefinitamente ma il contatore di un IP sotto attacco
// attivo non viene mai azzerato a metà (a differenza di un reset globale
// periodico, che regalerebbe gratuitamente un nuovo bucket pieno a
// chiunque stia proprio in quel momento tentando un brute force).
type ipRateLimiter struct {
	mu         sync.Mutex
	entries    map[string]*rateLimitEntry
	ratePerSec float64
	burst      float64
	staleAfter time.Duration
}

func newIPRateLimiter(ratePerSec, burst float64, staleAfter time.Duration) *ipRateLimiter {
	l := &ipRateLimiter{
		entries:    make(map[string]*rateLimitEntry),
		ratePerSec: ratePerSec,
		burst:      burst,
		staleAfter: staleAfter,
	}
	go l.pruneLoop()
	return l
}

func (l *ipRateLimiter) pruneLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-l.staleAfter)
		l.mu.Lock()
		for ip, e := range l.entries {
			e.mu.Lock()
			last := e.lastSeen
			e.mu.Unlock()
			if last.Before(cutoff) {
				delete(l.entries, ip)
			}
		}
		l.mu.Unlock()
	}
}

func (l *ipRateLimiter) allow(ip string) bool {
	l.mu.Lock()
	e, ok := l.entries[ip]
	if !ok {
		e = &rateLimitEntry{tokens: l.burst, lastSeen: time.Now()}
		l.entries[ip] = e
	}
	l.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now()
	e.tokens += now.Sub(e.lastSeen).Seconds() * l.ratePerSec
	if e.tokens > l.burst {
		e.tokens = l.burst
	}
	e.lastSeen = now

	if e.tokens < 1 {
		return false
	}
	e.tokens--
	return true
}
