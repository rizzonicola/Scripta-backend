package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestOpenWithConfigReturnsNilAndClosesOnMigrationFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "malformed.db")

	connector, err := newLocalConnector("file:" + path)
	if err != nil {
		t.Fatalf("newLocalConnector: %v", err)
	}
	raw := sql.OpenDB(wrapWithPragmas(connector, pragmaStatements))
	if _, err := raw.ExecContext(context.Background(), `CREATE TABLE folders (id TEXT)`); err != nil {
		_ = raw.Close()
		t.Fatalf("creazione schema malformato: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("chiusura DB preparazione: %v", err)
	}

	db, err := OpenWithConfig(Config{Path: path})
	if err == nil {
		if db != nil {
			_ = db.Close()
		}
		t.Fatal("OpenWithConfig ha accettato uno schema incompatibile")
	}
	if db != nil {
		t.Fatal("OpenWithConfig ha restituito un DB non nil insieme all'errore")
	}
}
