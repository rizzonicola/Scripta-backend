package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncStateMissingKeyIsZero(t *testing.T) {
	conn, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	v, err := NewSyncStateRepo(conn).GetInt(context.Background(), "assente")
	if err != nil || v != 0 {
		t.Fatalf("attesi (0, nil), ottenuti (%d, %v)", v, err)
	}
}

func TestSyncStateSetMaxNeverLowersTheValue(t *testing.T) {
	conn, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := NewSyncStateRepo(conn)
	ctx := context.Background()

	for _, step := range []struct{ write, want int64 }{{100, 100}, {50, 100}, {100, 100}, {200, 200}} {
		if err := r.SetMaxInt(ctx, "k", step.write); err != nil {
			t.Fatal(err)
		}
		if got, _ := r.GetInt(ctx, "k"); got != step.want {
			t.Fatalf("dopo SetMaxInt(%d): %d, atteso %d", step.write, got, step.want)
		}
	}
}

// Compatibilità all'indietro (E.5): il binario 2.1.0 esegue le SUE DDL su un DB
// già usato dal server nuovo. Qui si garantisce la condizione necessaria: ogni
// DDL dello schema è idempotente (IF NOT EXISTS), quindi una tabella in più
// non può far fallire l'avvio di un binario che non la conosce. La prova
// vera (avviare il binario 2.1.0 sul DB nuovo) è nel piano di rollout.
func TestEverySchemaStatementIsIdempotent(t *testing.T) {
	for _, stmt := range migrationStatements {
		s := strings.ToUpper(strings.Join(strings.Fields(stmt), " "))
		if !strings.HasPrefix(s, "CREATE TABLE IF NOT EXISTS") && !strings.HasPrefix(s, "CREATE INDEX IF NOT EXISTS") {
			t.Fatalf("DDL non idempotente: %q", stmt)
		}
	}
}

func TestMigrateTwiceKeepsSyncStateRows(t *testing.T) {
	conn, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := NewSyncStateRepo(conn)
	ctx := context.Background()
	if err := r.SetMaxInt(ctx, StampHWMKey, 12345); err != nil {
		t.Fatal(err)
	}
	// Seconda migrazione (riavvio): non deve toccare i dati.
	if err := migrate(conn); err != nil {
		t.Fatalf("seconda migrate: %v", err)
	}
	if got, _ := r.GetInt(ctx, StampHWMKey); got != 12345 {
		t.Fatalf("hwm perso dopo la migrate: %d", got)
	}
}
