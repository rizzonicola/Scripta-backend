package middleware

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// slowRequestThreshold: oltre questa durata totale una richiesta /api/* viene
// annotata nel log. È volutamente alta: l'obiettivo è trovare le sync che
// avvicinano il timeout del client (30 s), non fare un access log.
const slowRequestThreshold = 2 * time.Second

// Timing separa il tempo speso dal server dal tempo speso ad aspettare la
// rete. Senza questa distinzione una push grande su un uplink lento sembra
// una sync lenta lato server, e ogni ottimizzazione successiva (S-02, S-03,
// S-07) verrebbe misurata contro un numero che include l'upload del client.
//
// Su /api/* aggiunge alla risposta l'header standard Server-Timing con due
// metriche, in millisecondi:
//
//   - total: dall'ingresso della richiesta al momento in cui l'handler
//     comincia a rispondere (scrive lo status). Comprende la lettura del
//     body, la decodifica, la transazione e la serializzazione JSON, ma NON
//     l'invio dei byte al client: quello è rete.
//   - recv: la parte di total passata bloccati dentro Read() del body, cioè
//     in attesa che il client finisca di caricarlo. total - recv è il lavoro
//     del server.
//
// Non misura nulla dentro api_sync.go (lock, decodifica, transazione, pull):
// per quel dettaglio serve toccare l'handler, che non è di questo livello.
//
// Per le richieste che superano slowRequestThreshold scrive una riga di log
// con metodo, percorso (senza query string), status, tempi e byte ricevuti.
// Non registra mai titoli, contenuti, token, utente o IP.
//
// L'header NON viene aggiunto a /api/v1/auth/*: la durata precisa lato server
// del login toglierebbe il rumore di rete a chi cerca differenze di tempo tra
// utente esistente e inesistente, e il pavimento di 100 ms sui tentativi
// falliti (api_auth.go) esiste proprio per impedirlo. La riga di log per le
// richieste lente vale comunque anche lì.
//
// Le richieste fuori da /api/ passano senza alcuna modifica né costo.
func Timing(next http.Handler) http.Handler {
	return timing(next, slowRequestThreshold, log.Printf)
}

// timing è Timing con soglia e logger iniettabili, per i test.
func timing(next http.Handler, slow time.Duration, logf func(format string, args ...any)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()

		// Il body va avvolto per sapere quanto tempo il server passa in
		// attesa del client. http.NoBody (GET e simili) resta com'è: alcuni
		// handler lo confrontano per identità.
		var body *timingBody
		if r.Body != nil && r.Body != http.NoBody {
			body = &timingBody{ReadCloser: r.Body}
			r.Body = body
		}

		tw := &timingWriter{
			ResponseWriter: w,
			start:          start,
			body:           body,
			emit:           !strings.HasPrefix(r.URL.Path, "/api/v1/auth/"),
			status:         http.StatusOK,
		}
		next.ServeHTTP(tw, r)

		// Un handler che non scrive nulla lascia l'header ancora modificabile:
		// lo aggiungiamo qui, altrimenti quelle risposte sarebbero invisibili.
		tw.stamp()

		if total := time.Since(start); total >= slow {
			logf("richiesta lenta: %s %s status=%d totale=%dms recv=%dms bytes_in=%d",
				r.Method, r.URL.Path, tw.status, total.Milliseconds(), tw.recvDur().Milliseconds(), tw.bytesIn())
		}
	})
}

// timingBody accumula il tempo passato dentro Read() e i byte letti. Viene
// usato dalla sola goroutine dell'handler, quindi non serve alcun lock.
type timingBody struct {
	io.ReadCloser
	blocked time.Duration
	bytes   int64
}

func (b *timingBody) Read(p []byte) (int, error) {
	t := time.Now()
	n, err := b.ReadCloser.Read(p)
	b.blocked += time.Since(t)
	b.bytes += int64(n)
	return n, err
}

// timingWriter fissa l'header Server-Timing nell'istante in cui la risposta
// viene "committata" (primo WriteHeader o primo Write): dopo, gli header non
// si possono più cambiare.
type timingWriter struct {
	http.ResponseWriter
	start  time.Time
	body   *timingBody
	emit   bool
	status int
	done   bool
}

func (t *timingWriter) recvDur() time.Duration {
	if t.body == nil {
		return 0
	}
	return t.body.blocked
}

func (t *timingWriter) bytesIn() int64 {
	if t.body == nil {
		return 0
	}
	return t.body.bytes
}

// stamp scrive l'header una sola volta.
func (t *timingWriter) stamp() {
	if t.done {
		return
	}
	t.done = true
	if !t.emit {
		return
	}
	t.Header().Set("Server-Timing", fmt.Sprintf("total;dur=%.1f, recv;dur=%.1f",
		ms(time.Since(t.start)), ms(t.recvDur())))
}

func (t *timingWriter) WriteHeader(code int) {
	// Le risposte informative (1xx) non chiudono gli header: si passano oltre.
	if code >= 100 && code < 200 {
		t.ResponseWriter.WriteHeader(code)
		return
	}
	if !t.done {
		t.status = code
		t.stamp()
	}
	t.ResponseWriter.WriteHeader(code)
}

func (t *timingWriter) Write(b []byte) (int, error) {
	if !t.done {
		t.WriteHeader(http.StatusOK)
	}
	return t.ResponseWriter.Write(b)
}

// Flush mantiene l'interfaccia http.Flusher visibile agli handler, come fa
// già il writer di Gzip: avvolgere il writer non deve togliere capacità.
func (t *timingWriter) Flush() {
	// Un Flush senza scritture precedenti committa comunque lo status 200:
	// l'header va fissato prima, come per Write.
	if !t.done {
		t.WriteHeader(http.StatusOK)
	}
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap permette a http.ResponseController di raggiungere il writer vero
// (deadline, hijack) attraverso questo strato.
func (t *timingWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
