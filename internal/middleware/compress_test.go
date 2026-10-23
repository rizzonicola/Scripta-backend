package middleware

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAcceptsGzip(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		{"", false},
		{"gzip", true},
		{"gzip, deflate, br", true},
		{"GZip", true},
		{"gzip;q=0", false},
		{"gzip; q=0.000", false},
		{"gzip;q=0.5", true},
		{"xgzip", false},
		{"gzip2", false},
		{"*", true},
		{"*;q=0", false},
		{"deflate, *;q=0.5", true},
		{"gzip;q=0, *;q=1", false},
		{"gzip;q=0.5, *;q=0", true},
		{"gzip;q=abc", false},
		{"gzip;q=2", false},
	}
	for _, tc := range cases {
		if got := acceptsGzip([]string{tc.header}); got != tc.want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
	if !acceptsGzip([]string{"br", "gzip"}) {
		t.Error("acceptsGzip deve considerare tutte le righe dell'header")
	}
}

func TestGzipCompressesWhenAccepted(t *testing.T) {
	handler := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello")
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer zr.Close()
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(body) != "hello" {
		t.Fatalf("body = %q, want hello", body)
	}
}

func TestGzipSkipsWhenQZeroOrNotAccepted(t *testing.T) {
	for _, header := range []string{"gzip;q=0", "xgzip", "br", ""} {
		handler := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "hello")
		}))
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if header != "" {
			req.Header.Set("Accept-Encoding", header)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("Accept-Encoding %q: Content-Encoding = %q, want empty", header, got)
		}
		if got := rec.Body.String(); got != "hello" {
			t.Errorf("Accept-Encoding %q: body = %q, want hello", header, got)
		}
	}
}
