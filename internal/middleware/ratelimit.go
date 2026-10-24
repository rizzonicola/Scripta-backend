package middleware

import (
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
// golang.org/x/time/rate): un semplice token bucket per chiave.
func RateLimit(ratePerSec, burst float64, ips *ClientIPResolver) func(http.Handler) http.Handler {
	limiter := newKeyedRateLimiter(ratePerSec, burst, 30*time.Minute)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// ips == nil => nessun proxy fidato: si usa solo r.RemoteAddr
			// (vedi ClientIPResolver: gli header di inoltro non sono mai
			// creduti se la richiesta non arriva da un proxy configurato).
			if !limiter.allow(ips.ClientIP(r)) {
				w.Header().Set("Retry-After", "5")
				writeJSONErrorCode(w, http.StatusTooManyRequests,
					"troppi tentativi, riprova più tardi", "rate_limited")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// UserRateLimiter limita la sync per utente autenticato. È separato dal
// limiter per chiave del login: saturare un client non deve bloccare gli altri
// utenti dietro lo stesso NAT, e un utente non deve poter consumare il budget
// degli altri.
type UserRateLimiter struct {
	limiter *keyedRateLimiter
}

func NewUserRateLimiter(ratePerSec, burst float64) *UserRateLimiter {
	return &UserRateLimiter{limiter: newKeyedRateLimiter(ratePerSec, burst, 30*time.Minute)}
}

func (l *UserRateLimiter) Allow(userID string) bool {
	return l.limiter.allow(userID)
}

// rateLimitEntry è lo stato del token bucket di una singola chiave (IP o
// utente).
type rateLimitEntry struct {
	mu       sync.Mutex
	tokens   float64
	lastSeen time.Time
}

// keyedRateLimiter implementa un token bucket per chiave con refill continuo
// (non a finestre fisse: evita il classico "doppio burst" a cavallo di due
// finestre adiacenti).
//
// Pulizia: la mappa NON viene mai svuotata per intero. Un ticker periodico
// rimuove SOLO le singole entry inattive da più di staleAfter, così la
// mappa non cresce indefinitamente ma il contatore di una chiave sotto attacco
// attivo non viene mai azzerato a metà (a differenza di un reset globale
// periodico, che regalerebbe gratuitamente un nuovo bucket pieno a
// chiunque stia proprio in quel momento tentando un brute force).
type keyedRateLimiter struct {
	mu         sync.Mutex
	entries    map[string]*rateLimitEntry
	ratePerSec float64
	burst      float64
	staleAfter time.Duration
}

func newKeyedRateLimiter(ratePerSec, burst float64, staleAfter time.Duration) *keyedRateLimiter {
	l := &keyedRateLimiter{
		entries:    make(map[string]*rateLimitEntry),
		ratePerSec: ratePerSec,
		burst:      burst,
		staleAfter: staleAfter,
	}
	go l.pruneLoop()
	return l
}

func (l *keyedRateLimiter) pruneLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-l.staleAfter)
		l.mu.Lock()
		for key, e := range l.entries {
			e.mu.Lock()
			last := e.lastSeen
			e.mu.Unlock()
			if last.Before(cutoff) {
				delete(l.entries, key)
			}
		}
		l.mu.Unlock()
	}
}

func (l *keyedRateLimiter) allow(key string) bool {
	l.mu.Lock()
	e, ok := l.entries[key]
	if !ok {
		e = &rateLimitEntry{tokens: l.burst, lastSeen: time.Now()}
		l.entries[key] = e
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
