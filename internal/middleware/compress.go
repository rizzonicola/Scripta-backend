package middleware

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
)

// gzipWriterPool riusa i *gzip.Writer tra una richiesta e l'altra: allocare
// un nuovo writer (e i relativi buffer interni, ~32-64KB) ad ogni risposta
// sarebbe pressione GC inutile su un server che comprime ogni singola
// risposta JSON/HTML/Markdown. Reset(w) sull'istanza presa dal pool sposta
// solo il destinatario dei byte, senza ri-allocare nulla.
var gzipWriterPool = sync.Pool{
	New: func() any {
		gw, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
		return gw
	},
}

// gzipResponseWriter inoltra i byte scritti dagli handler al *gzip.Writer
// invece che direttamente al client, e rimuove Content-Length (che dopo la
// compressione non è più noto in anticipo: gli handler di questo progetto
// non lo impostano comunque esplicitamente, ma la rimozione è difensiva).
type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
}

func (g *gzipResponseWriter) WriteHeader(status int) {
	g.Header().Del("Content-Length")
	g.wroteHeader = true
	g.ResponseWriter.WriteHeader(status)
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	return g.gz.Write(b)
}

// Flush inoltra il flush sia al gzip.Writer (svuota i byte già compressi nel
// buffer) sia, se supportato, al ResponseWriter sottostante. Non
// strettamente necessario per gli handler attuali (nessuno di essi fa
// streaming incrementale), ma evita comportamenti sorprendenti se in futuro
// un handler chiamasse Flush esplicitamente.
func (g *gzipResponseWriter) Flush() {
	_ = g.gz.Flush()
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Gzip comprime il corpo delle risposte quando il client dichiara supporto
// per gzip (header Accept-Encoding), senza alcun impatto sui contratti API:
// stessi status code, stesso JSON/HTML/Markdown nel body, semplicemente
// trasferito compresso. I client HTTP moderni (inclusi tutti i client
// mobile/browser tipici di Scripta) decodificano gzip in modo trasparente.
//
// Non comprime se il client non dichiara Accept-Encoding: gzip (es. la
// HEALTHCHECK di Docker via wget senza opzioni), lasciando la risposta
// invariata in quel caso.
func Gzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}

		gz := gzipWriterPool.Get().(*gzip.Writer)
		gz.Reset(w)
		defer func() {
			_ = gz.Close()
			gzipWriterPool.Put(gz)
		}()

		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")

		next.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, gz: gz}, r)
	})
}
