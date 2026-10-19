package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// openScratch apre un DB locale vero (stesso pool da 1 connessione di
// produzione) e ci crea una tabella di appoggio da riempire e svuotare per
// produrre pagine libere, senza passare dai repository del sync.
func openScratch(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := Open(filepath.Join(t.TempDir(), "maint.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.Exec(`CREATE TABLE scratch (grp INTEGER NOT NULL, b BLOB NOT NULL)`); err != nil {
		t.Fatalf("create scratch: %v", err)
	}
	return conn
}

// fill inserisce n righe da ~2 KiB nel gruppo grp.
func fill(t *testing.T, conn *sql.DB, grp, n int) {
	t.Helper()
	tx, err := conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	blob := []byte(strings.Repeat("x", 2048))
	for i := 0; i < n; i++ {
		if _, err := tx.Exec(`INSERT INTO scratch (grp, b) VALUES (?, ?)`, grp, blob); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func pragmaInt(t *testing.T, conn *sql.DB, name string) int64 {
	t.Helper()
	var v int64
	if err := conn.QueryRow(`PRAGMA ` + name).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return v
}

func TestIncrementalVacuumChunked_LiberaTuttoERestringeIlDb(t *testing.T) {
	conn := openScratch(t)
	fill(t, conn, 1, 3000)
	if _, err := conn.Exec(`DELETE FROM scratch`); err != nil {
		t.Fatal(err)
	}
	freeBefore, pagesBefore := pragmaInt(t, conn, "freelist_count"), pragmaInt(t, conn, "page_count")
	if freeBefore < 1000 {
		t.Fatalf("precondizione: servono molte pagine libere, trovate %d", freeBefore)
	}

	if err := incrementalVacuum(context.Background(), conn); err != nil {
		t.Fatalf("incrementalVacuum: %v", err)
	}

	if free := pragmaInt(t, conn, "freelist_count"); free != 0 {
		t.Errorf("freelist_count=%d dopo il vacuum, atteso 0", free)
	}
	if pagesAfter := pragmaInt(t, conn, "page_count"); pagesAfter >= pagesBefore {
		t.Errorf("page_count %d -> %d: il DB non si è ristretto", pagesBefore, pagesAfter)
	}
}

// Il test che distingue il passo-a-passo da "tutto in un colpo": con un pool da
// 1 connessione, un hook tra i passi che usa il DB si blocca per sempre se la
// connessione non è stata restituita al pool. I passi, inoltre, devono essere
// davvero più d'uno e ciascuno limitato alla dimensione richiesta.
func TestIncrementalVacuumChunked_RilasciaLaConnessioneTraIPassi(t *testing.T) {
	conn := openScratch(t)
	fill(t, conn, 1, 3000)
	if _, err := conn.Exec(`DELETE FROM scratch`); err != nil {
		t.Fatal(err)
	}
	free := pragmaInt(t, conn, "freelist_count")

	const chunk = 64
	steps := 0
	prevFree := free
	failed := false
	runCtx, stopRun := context.WithCancel(context.Background())
	defer stopRun()
	between := func() {
		steps++
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var now int64
		if err := conn.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&now); err != nil {
			// Basta un solo fallimento per concludere: si ferma il ciclo invece
			// di aspettare un timeout per ognuno dei passi restanti.
			t.Errorf("la connessione non è stata rilasciata tra i passi: %v", err)
			failed = true
			stopRun()
			return
		}
		if prevFree-now > chunk {
			t.Errorf("un passo ha liberato %d pagine, più del limite %d", prevFree-now, chunk)
		}
		prevFree = now
	}

	err := incrementalVacuumChunked(runCtx, conn, chunk, between)
	if failed {
		return
	}
	if err != nil {
		t.Fatalf("incrementalVacuumChunked: %v", err)
	}
	if min := int(free/chunk) - 1; steps < min {
		t.Errorf("passi eseguiti=%d, attesi almeno %d (free=%d, chunk=%d)", steps, min, free, chunk)
	}
	if got := pragmaInt(t, conn, "freelist_count"); got != 0 {
		t.Errorf("freelist_count=%d, atteso 0", got)
	}
}

// Nessuna perdita di dati: le righe vive prima del vacuum sono identiche dopo.
func TestIncrementalVacuumChunked_NonToccaLeRigheVive(t *testing.T) {
	conn := openScratch(t)
	fill(t, conn, 1, 2000) // da cancellare
	fill(t, conn, 2, 500)  // da conservare
	if _, err := conn.Exec(`DELETE FROM scratch WHERE grp = 1`); err != nil {
		t.Fatal(err)
	}
	var nBefore, bytesBefore int64
	if err := conn.QueryRow(`SELECT COUNT(*), SUM(LENGTH(b)) FROM scratch`).Scan(&nBefore, &bytesBefore); err != nil {
		t.Fatal(err)
	}

	if err := incrementalVacuumChunked(context.Background(), conn, 32, nil); err != nil {
		t.Fatal(err)
	}

	var nAfter, bytesAfter, grpsAfter int64
	if err := conn.QueryRow(`SELECT COUNT(*), SUM(LENGTH(b)), COUNT(DISTINCT grp) FROM scratch`).Scan(&nAfter, &bytesAfter, &grpsAfter); err != nil {
		t.Fatal(err)
	}
	if nAfter != nBefore || bytesAfter != bytesBefore || grpsAfter != 1 {
		t.Fatalf("righe vive alterate: n %d->%d, byte %d->%d, gruppi=%d", nBefore, nAfter, bytesBefore, bytesAfter, grpsAfter)
	}
	var ic string
	if err := conn.QueryRow(`PRAGMA integrity_check`).Scan(&ic); err != nil || ic != "ok" {
		t.Fatalf("integrity_check=%q err=%v", ic, err)
	}
}

// Se il contesto scade a metà i passi già fatti restano: prima un unico
// statement interrotto veniva annullato per intero.
func TestIncrementalVacuumChunked_ContestoAnnullatoConservaIlProgresso(t *testing.T) {
	conn := openScratch(t)
	fill(t, conn, 1, 3000)
	if _, err := conn.Exec(`DELETE FROM scratch`); err != nil {
		t.Fatal(err)
	}
	freeBefore := pragmaInt(t, conn, "freelist_count")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := incrementalVacuumChunked(ctx, conn, 64, cancel) // annulla dopo il primo passo
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("atteso context.Canceled, ottenuto %v", err)
	}
	freeAfter := pragmaInt(t, conn, "freelist_count")
	if freeAfter >= freeBefore || freeAfter == 0 {
		t.Errorf("freelist %d -> %d: atteso un progresso parziale (>0 e <%d)", freeBefore, freeAfter, freeBefore)
	}
}

func TestIncrementalVacuumChunked_SenzaPagineLibereEUnNoOp(t *testing.T) {
	conn := openScratch(t)
	if err := incrementalVacuum(context.Background(), conn); err != nil {
		t.Fatalf("atteso nil su DB senza pagine libere, ottenuto %v", err)
	}
}

// Su un DB con auto_vacuum=NONE il PRAGMA non libera nulla: il ciclo deve
// fermarsi con un errore, non girare all'infinito tenendo la connessione.
func TestIncrementalVacuumChunked_SenzaProgressoSiFerma(t *testing.T) {
	conn, err := sql.Open("libsql", "file:"+filepath.Join(t.TempDir(), "none.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	if _, err := conn.Exec(`CREATE TABLE scratch (grp INTEGER NOT NULL, b BLOB NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	fill(t, conn, 1, 500)
	if _, err := conn.Exec(`DELETE FROM scratch`); err != nil {
		t.Fatal(err)
	}
	if mode := pragmaInt(t, conn, "auto_vacuum"); mode != 0 {
		t.Skipf("precondizione non soddisfatta: auto_vacuum=%d", mode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := incrementalVacuumChunked(ctx, conn, 16, nil); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("atteso errore di mancato progresso, ottenuto %v", err)
	}
}

// Il percorso di produzione: RunMaintenance con reclaimPages=true.
func TestRunMaintenance_RecuperaSpazio(t *testing.T) {
	conn := openScratch(t)
	fill(t, conn, 1, 2000)
	if _, err := conn.Exec(`DELETE FROM scratch`); err != nil {
		t.Fatal(err)
	}
	if err := RunMaintenance(context.Background(), conn, true, true); err != nil {
		t.Fatalf("RunMaintenance: %v", err)
	}
	if free := pragmaInt(t, conn, "freelist_count"); free != 0 {
		t.Errorf("freelist_count=%d, atteso 0", free)
	}
}
