package handlers

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeJSONBodyAcceptsSingleValueAndWhitespace(t *testing.T) {
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"username":"mario"}  `+"\n"))
	w := httptest.NewRecorder()
	var dst struct {
		Username string `json:"username"`
	}
	if !decodeJSONBody(w, r, &dst, 1024) {
		t.Fatalf("body valido rifiutato: %d %s", w.Code, w.Body)
	}
	if dst.Username != "mario" {
		t.Fatalf("username = %q", dst.Username)
	}
}

func TestDecodeJSONBodyRejectsTrailingValue(t *testing.T) {
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"username":"a"}{"password":"b"}`))
	w := httptest.NewRecorder()
	var dst map[string]string
	if decodeJSONBody(w, r, &dst, 1024) {
		t.Fatal("secondo valore JSON accettato")
	}
	if w.Code != 400 {
		t.Fatalf("status = %d, atteso 400", w.Code)
	}
}
