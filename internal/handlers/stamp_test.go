package handlers

// Y-04: lo stamp del server (server_time) non deve mai tornare indietro,
// nemmeno dopo un riavvio con l'orologio di sistema indietro.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"notes-server/internal/db"
	"notes-server/internal/models"
)

func newStampTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "stamp.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	u, err := db.NewUsersRepo(sqlDB).Create(context.Background(), "mario", "hash")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return sqlDB, u.ID
}

func handlerWithClock(sqlDB *sql.DB, now *time.Time) *SyncHandler {
	h := NewSyncHandler(sqlDB)
	h.clock = func() time.Time { return *now }
	return h
}

func syncAt(t *testing.T, h *SyncHandler, userID string, last int64) *models.SyncResponse {
	t.Helper()
	res := h.process(context.Background(), userID, &models.SyncRequest{
		LastSyncedAt: last, Folders: []models.FolderDTO{}, Notes: []models.NoteDTO{},
	})
	if res.resp == nil {
		t.Fatalf("sync fallita: status=%d msg=%q rejected=%v", res.status, res.message, res.rejected)
	}
	return res.resp
}

func persistedHWM(t *testing.T, sqlDB *sql.DB) int64 {
	t.Helper()
	v, err := db.NewSyncStateRepo(sqlDB).GetInt(context.Background(), db.StampHWMKey)
	if err != nil {
		t.Fatalf("lettura hwm: %v", err)
	}
	return v
}

func TestStampStaysAheadAfterRestartWithClockGoingBackwards(t *testing.T) {
	sqlDB, userID := newStampTestDB(t)
	clock := time.UnixMilli(1_800_000_000_000)

	h1 := handlerWithClock(sqlDB, &clock)
	first := syncAt(t, h1, userID, 0)
	second := syncAt(t, h1, userID, first.ServerTime)

	// «Riavvio» con l'orologio indietro di un'ora (step NTP, snapshot VM).
	earlier := clock.Add(-time.Hour)
	h2 := handlerWithClock(sqlDB, &earlier)
	third := syncAt(t, h2, userID, second.ServerTime)

	if third.ServerTime <= second.ServerTime {
		t.Fatalf("lo stamp è tornato indietro: %d dopo %d", third.ServerTime, second.ServerTime)
	}
	// Senza lo stamp persistente, il cursore del client risultava «nel futuro»
	// e la risposta diventava un full resync spurio.
	if third.FullResync {
		t.Fatal("full_resync inatteso: il cursore consegnato prima del riavvio era valido")
	}
}

func TestStampRemainsMonotonicWithinOneProcessWhenClockGoesBack(t *testing.T) {
	sqlDB, userID := newStampTestDB(t)
	clock := time.UnixMilli(1_800_000_000_000)
	h := handlerWithClock(sqlDB, &clock)

	a := syncAt(t, h, userID, 0)
	clock = clock.Add(-time.Hour)
	b := syncAt(t, h, userID, a.ServerTime)
	if b.ServerTime <= a.ServerTime {
		t.Fatalf("stamp non monotono: %d dopo %d", b.ServerTime, a.ServerTime)
	}
}

func TestStampHighWaterMarkIsWrittenAtMostOncePerLease(t *testing.T) {
	sqlDB, userID := newStampTestDB(t)
	clock := time.UnixMilli(1_800_000_000_000)
	h := handlerWithClock(sqlDB, &clock)

	first := syncAt(t, h, userID, 0)
	hwm := persistedHWM(t, sqlDB)
	if hwm < first.ServerTime+stampLease.Milliseconds() {
		t.Fatalf("hwm %d non copre lo stamp %d più il lease", hwm, first.ServerTime)
	}

	// Molte sync dentro la stessa finestra: l'hwm non si muove (nessuna scrittura).
	last := first.ServerTime
	for i := 0; i < 25; i++ {
		clock = clock.Add(100 * time.Millisecond)
		last = syncAt(t, h, userID, last).ServerTime
	}
	if got := persistedHWM(t, sqlDB); got != hwm {
		t.Fatalf("hwm riscritto dentro il lease: %d invece di %d", got, hwm)
	}

	// Oltre la finestra: una nuova scrittura, e l'hwm resta ≥ di ogni stamp emesso.
	clock = clock.Add(stampLease + time.Second)
	later := syncAt(t, h, userID, last)
	if got := persistedHWM(t, sqlDB); got <= hwm || got < later.ServerTime {
		t.Fatalf("hwm non esteso: %d (stamp %d, vecchio hwm %d)", got, later.ServerTime, hwm)
	}
}

// Invariante che rende sicuro il riavvio: ogni stamp consegnato a un client è
// ≤ dell'hwm già scritto su disco.
func TestStampNeverExceedsPersistedHighWaterMark(t *testing.T) {
	sqlDB, userID := newStampTestDB(t)
	clock := time.UnixMilli(1_800_000_000_000)
	h := handlerWithClock(sqlDB, &clock)

	var last int64
	for i := 0; i < 40; i++ {
		clock = clock.Add(20 * time.Second)
		r := syncAt(t, h, userID, last)
		last = r.ServerTime
		if hwm := persistedHWM(t, sqlDB); r.ServerTime > hwm {
			t.Fatalf("giro %d: stamp %d oltre l'hwm persistito %d", i, r.ServerTime, hwm)
		}
	}
}
