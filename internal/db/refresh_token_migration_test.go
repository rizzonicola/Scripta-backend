package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// Verifica che la migrazione non "legalizzi" i refresh token legacy: le
// righe con epoch 0 vengono eliminate, non rilegate alla versione corrente.
// Il backfill precedente rendeva accettabile per sempre un token nato dalla
// race reset/login (e la rotazione ne copiava l'epoch nel successore).
func TestRefreshTokenMigrationDropsLegacyEpochRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-refresh.db")
	conn, err := sql.Open("libsql", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Schema pre-migrazione: refresh_tokens senza password_version.
	if _, err := conn.Exec(`CREATE TABLE users (
		id TEXT PRIMARY KEY,
		password_version INTEGER NOT NULL DEFAULT 1
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`CREATE TABLE refresh_tokens (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		family_id TEXT NOT NULL,
		token_hash TEXT NOT NULL UNIQUE,
		issued_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		revoked_at INTEGER,
		replaced_by TEXT
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO users (id, password_version) VALUES ('u1', 4)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO refresh_tokens
		(id, user_id, family_id, token_hash, issued_at, expires_at)
		VALUES ('legacy', 'u1', 'fam', 'hash', 1, 9999999999999)`); err != nil {
		t.Fatal(err)
	}

	if err := addRefreshTokenPasswordVersion(conn); err != nil {
		t.Fatal(err)
	}

	var count int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM refresh_tokens`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("riga legacy ancora presente: %d", count)
	}

	// La colonna esiste e un token con epoch corrente sopravvive (idempotenza
	// su un secondo avvio).
	if _, err := conn.Exec(`INSERT INTO refresh_tokens
		(id, user_id, family_id, token_hash, password_version, issued_at, expires_at)
		VALUES ('fresh', 'u1', 'fam2', 'hash2', 4, 2, 9999999999999)`); err != nil {
		t.Fatal(err)
	}
	if err := addRefreshTokenPasswordVersion(conn); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(`SELECT COUNT(*) FROM refresh_tokens WHERE id = 'fresh'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("la migrazione ha eliminato un token con epoch valido")
	}
}
