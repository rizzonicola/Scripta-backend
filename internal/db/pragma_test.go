package db

import (
	"path/filepath"
	"strings"
	"testing"
)

// Fissa la configurazione di durabilità/concorrenza che il server dichiara
// (db.go: pragmaStatements) e verifica che valga su OGNI connessione fisica,
// non solo sulla prima: i PRAGMA di SQLite sono per-connessione, e il pool
// può chiuderne e riaprirne (inattività, errori). Una regressione qui
// riaprirebbe il bug storico delle FK perse dopo la chiusura per inattività
// (commento in OpenWithConfig) e cambierebbe in silenzio la durabilità.
func TestPragma_ValgonoSuOgniConnessioneFisica(t *testing.T) {
	conn, err := Open(filepath.Join(t.TempDir(), "pragma.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	check := func(fase string) {
		t.Helper()
		var journal string
		if err := conn.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil || !strings.EqualFold(journal, "wal") {
			t.Errorf("%s: journal_mode=%q err=%v, atteso wal", fase, journal, err)
		}
		// 1 = NORMAL. FULL (2) costerebbe un fsync per commit: cambiare la
		// durabilità è una decisione del PM, non un effetto collaterale.
		if got := pragmaInt(t, conn, "synchronous"); got != 1 {
			t.Errorf("%s: synchronous=%d, atteso 1 (NORMAL)", fase, got)
		}
		if got := pragmaInt(t, conn, "busy_timeout"); got != 5000 {
			t.Errorf("%s: busy_timeout=%d, atteso 5000", fase, got)
		}
		if got := pragmaInt(t, conn, "foreign_keys"); got != 1 {
			t.Errorf("%s: foreign_keys=%d, atteso 1", fase, got)
		}
	}

	check("prima connessione")

	// Chiude le connessioni inattive: la query successiva ne apre una nuova.
	conn.SetMaxIdleConns(0)
	if st := conn.Stats(); st.OpenConnections != 0 {
		t.Fatalf("precondizione: connessioni aperte=%d, attese 0", st.OpenConnections)
	}
	check("connessione riaperta dal pool")
}
