package db

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSecureDBFileRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.db")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.db")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink non supportato: %v", err)
	}
	if err := secureDBFile(link); err == nil {
		t.Fatal("symlink accettato")
	}
}

func TestSecureDBFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.db")
	if err := secureDBFile(path); err != nil {
		t.Fatalf("secureDBFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("permessi = %o, attesi 600", got)
	}
}

func TestValidatePrimaryURL(t *testing.T) {
	ok := []string{
		"libsql://example.turso.io",
		"https://example.turso.io",
		"wss://example.turso.io",
		"http://localhost:8080",
		"http://127.0.0.1:8080",
		"http://[::1]:8080",
	}
	for _, raw := range ok {
		if err := validatePrimaryURL(raw); err != nil {
			t.Errorf("%s rifiutato: %v", raw, err)
		}
	}
	bad := []string{
		"http://example.com",
		"ws://example.com",
		"ftp://example.com",
		"libsql://",
		"https://",
		"http://",
		"://",
		"https://user:pass@example.turso.io",
		"libsql://user@example.turso.io",
		"https://example.turso.io?token=secret",
		"https://example.turso.io#fragment",
	}
	for _, raw := range bad {
		if err := validatePrimaryURL(raw); err == nil {
			t.Errorf("%s accettato", raw)
		}
	}
}
