package middleware

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var serverTimingRe = regexp.MustCompile(`^total;dur=([0-9]+\.[0-9]), recv;dur=([0-9]+\.[0-9])$`)

// parseServerTiming estrae (total, recv) in ms e fallisce se il formato cambia:
// il formato è consumato da strumenti esterni (harness, curl), non è libero.
func parseServerTiming(t *testing.T, h http.Header) (total, recv float64) {
	t.Helper()
	v := h.Get("Server-Timing")
	m := serverTimingRe.FindStringSubmatch(v)
	if m == nil {
		t.Fatalf("Server-Timing assente o con formato inatteso: %q", v)
	}
	total, _ = strconv.ParseFloat(m[1], 64)
	recv, _ = strconv.ParseFloat(m[2], 64)
	return total, recv
}

// slowReader simula un uplink lento: ogni Read attende d prima di restituire.
type slowReader struct {
	r io.Reader
	d time.Duration
}

func (s *slowReader) Read(p []byte) (int, error) {
	time.Sleep(s.d)
	if len(p) > 4 {
		p = p[:4]
	}
	return s.r.Read(p)
}

func noLog(string, ...any) {}

func TestTiming_HeaderSuApi(t *testing.T) {
	h := timing(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(25 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}), time.Hour, noLog)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/user/settings", nil))

	total, recv := parseServerTiming(t, rec.Header())
	if total < 25 {
		t.Errorf("total=%.1f ms, atteso >= 25 (l'handler dorme 25 ms)", total)
	}
	if recv != 0 {
		t.Errorf("recv=%.1f ms, atteso 0: la richiesta non ha body", recv)
	}
}

func TestTiming_RecvSeparaAttesaDelClient(t *testing.T) {
	// Body da 12 byte letto 4 byte per volta con 20 ms di attesa per Read:
	// il server non lavora, aspetta. recv deve dominare total.
	body := &slowReader{r: strings.NewReader("abcdefghijkl"), d: 20 * time.Millisecond}
	h := timing(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			t.Errorf("lettura body: %v", err)
		}
		w.Write([]byte("ok"))
	}), time.Hour, noLog)

	req := httptest.NewRequest("POST", "/api/v1/sync", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	total, recv := parseServerTiming(t, rec.Header())
	if recv < 60 { // 3 Read utili + 1 finale (EOF): almeno 60 ms
		t.Errorf("recv=%.1f ms, atteso >= 60", recv)
	}
	if recv > total {
		t.Errorf("recv=%.1f > total=%.1f: impossibile", recv, total)
	}
}

func TestTiming_BodyIntegro(t *testing.T) {
	const payload = `{"last_synced_at":0,"folders":[],"notes":[]}`
	var got string
	h := timing(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
	}), time.Hour, noLog)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/v1/sync", strings.NewReader(payload)))
	if got != payload {
		t.Fatalf("il middleware ha alterato il body: %q", got)
	}
}

func TestTiming_NonToccaFuoriDaApi(t *testing.T) {
	for _, path := range []string{"/healthz", "/health", "/admin", "/admin/login", "/apix"} {
		var sawWrapper bool
		h := timing(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, sawWrapper = w.(*timingWriter)
			w.WriteHeader(http.StatusOK)
		}), time.Hour, noLog)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Header().Get("Server-Timing") != "" || sawWrapper {
			t.Errorf("%s: il middleware non deve intervenire fuori da /api/", path)
		}
	}
}

func TestTiming_LoginSenzaHeaderMaConLog(t *testing.T) {
	var logged []string
	h := timing(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Millisecond)
		w.WriteHeader(http.StatusUnauthorized)
	}), time.Millisecond, func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) })

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader("{}")))

	if v := rec.Header().Get("Server-Timing"); v != "" {
		t.Errorf("il login non deve esporre Server-Timing (timing side-channel), trovato %q", v)
	}
	if len(logged) != 1 {
		t.Fatalf("attesa 1 riga di log per il login lento, trovate %d", len(logged))
	}
}

func TestTiming_LogSoloSeLentaESenzaDatiSensibili(t *testing.T) {
	var logged []string
	logf := func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }

	h := timing(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		time.Sleep(5 * time.Millisecond)
		w.WriteHeader(http.StatusUnprocessableEntity)
	}), 2*time.Millisecond, logf)

	const payload = `{"title":"SEGRETO-TITOLO","content":"SEGRETO-CONTENUTO"}`
	req := httptest.NewRequest("POST", "/api/v1/sync?id=SEGRETO-ID&token=SEGRETO-TOKEN",
		strings.NewReader(payload))
	req.Header.Set("Authorization", "Bearer SEGRETO-JWT")
	req.RemoteAddr = "203.0.113.7:4321"
	h.ServeHTTP(httptest.NewRecorder(), req)

	if len(logged) != 1 {
		t.Fatalf("attesa 1 riga di log, trovate %d: %v", len(logged), logged)
	}
	line := logged[0]
	for _, want := range []string{"POST", "/api/v1/sync", "status=422", fmt.Sprintf("bytes_in=%d", len(payload))} {
		if !strings.Contains(line, want) {
			t.Errorf("la riga di log non contiene %q: %s", want, line)
		}
	}
	for _, leak := range []string{"SEGRETO", "203.0.113.7", "id=", "token="} {
		if strings.Contains(line, leak) {
			t.Errorf("la riga di log espone %q: %s", leak, line)
		}
	}

	// Sotto soglia: nessun log.
	logged = nil
	fast := timing(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }), time.Hour, logf)
	fast.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/user/settings", nil))
	if len(logged) != 0 {
		t.Errorf("una richiesta veloce non deve produrre log: %v", logged)
	}
}

func TestTiming_HandlerCheNonScriveNulla(t *testing.T) {
	h := timing(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), time.Hour, noLog)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/user/settings", nil))
	parseServerTiming(t, rec.Header())
}

func TestTiming_StatusEFlushPreservati(t *testing.T) {
	h := timing(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("il writer avvolto deve restare un http.Flusher")
			return
		}
		w.WriteHeader(http.StatusTeapot)
		w.Write([]byte("x"))
		f.Flush()
	}), time.Hour, noLog)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/user/settings", nil))
	if rec.Code != http.StatusTeapot || !rec.Flushed || rec.Body.String() != "x" {
		t.Fatalf("status=%d flushed=%v body=%q", rec.Code, rec.Flushed, rec.Body.String())
	}
	parseServerTiming(t, rec.Header())
}

func TestTiming_ConGzipEsterno(t *testing.T) {
	// Stessa catena di produzione: Timing fuori da Gzip. L'header deve
	// arrivare anche quando il corpo è compresso.
	h := timing(Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})), time.Hour, noLog)
	req := httptest.NewRequest("GET", "/api/v1/user/settings", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	parseServerTiming(t, rec.Header())
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("Gzip non applicato")
	}
}
