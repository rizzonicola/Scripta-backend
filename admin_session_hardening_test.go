package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Il logout admin deve invalidare il cookie anche lato server: riusare il
// vecchio cookie non deve più dare accesso alla dashboard.
func TestAdminLogoutRevokesSessionServerSide(t *testing.T) {
	e := newEnv(t)
	cookie := e.adminCookie()

	if code, _ := e.doAdmin("POST", "/admin/logout", cookie, nil); code != http.StatusSeeOther {
		t.Fatalf("logout admin: %d, atteso 303", code)
	}
	if code, _ := e.doAdmin("GET", "/admin", cookie, nil); code != http.StatusSeeOther {
		t.Fatalf("vecchio cookie ancora accettato dopo il logout: %d", code)
	}
}

// Un POST admin senza né Sec-Fetch-Site né Origin non è verificabile e deve
// essere rifiutato (prima passava).
func TestAdminPostWithoutOriginHeadersIsRejected(t *testing.T) {
	e := newEnv(t)
	form := url.Values{"username": {"root"}, "password": {"a-very-long-admin-pass"}}
	req, err := http.NewRequest("POST", e.srv.URL+"/admin/login", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST senza header di origine: %d, atteso 403", resp.StatusCode)
	}
}
