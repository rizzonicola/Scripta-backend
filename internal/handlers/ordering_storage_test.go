package handlers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"notes-server/internal/db"
	"notes-server/internal/middleware"
	"notes-server/internal/models"
)

// pushOrdering esegue una sync vuota con un push di posizionamento e
// restituisce l'esito. Il clock avanza a ogni chiamata, così le due sync non
// condividono lo stesso server_time.
func pushOrdering(t *testing.T, h *SyncHandler, clock *time.Time, userID string, baseVersion int64, data string) syncResult {
	t.Helper()
	*clock = clock.Add(time.Second)
	return h.process(context.Background(), userID, &models.SyncRequest{
		Folders: []models.FolderDTO{},
		Notes:   []models.NoteDTO{},
		Ordering: &models.OrderingPush{
			BaseVersion: baseVersion,
			Data:        json.RawMessage(data),
		},
	})
}

func TestOrderingPushUsesOptimisticVersionAndResetWins(t *testing.T) {
	sqlDB, userID := newStampTestDB(t)
	clock := time.UnixMilli(1_800_000_000_000)
	h := handlerWithClock(sqlDB, &clock)

	first := pushOrdering(t, h, &clock, userID, 0, `{"folders":{"a":1}}`)
	if first.resp == nil || first.resp.Ordering == nil {
		t.Fatalf("prima sync fallita: %+v", first)
	}
	if first.resp.Ordering.Version != 1 || string(first.resp.Ordering.Data) != `{"folders":{"a":1}}` {
		t.Fatalf("stato dopo il primo push: v=%d data=%s", first.resp.Ordering.Version, first.resp.Ordering.Data)
	}

	// Base superata (0 contro la versione 1): il server ignora il push e
	// restituisce lo stato autoritativo.
	stale := pushOrdering(t, h, &clock, userID, 0, `{"folders":{"a":9}}`)
	if stale.resp == nil || stale.resp.Ordering == nil {
		t.Fatalf("sync con base superata fallita: %+v", stale)
	}
	if stale.resp.Ordering.Version != 1 || string(stale.resp.Ordering.Data) != `{"folders":{"a":1}}` {
		t.Fatalf("un push superato ha sovrascritto lo stato: v=%d data=%s", stale.resp.Ordering.Version, stale.resp.Ordering.Data)
	}

	current := pushOrdering(t, h, &clock, userID, 1, `{"folders":{"b":2}}`)
	if current.resp == nil || current.resp.Ordering == nil || current.resp.Ordering.Version != 2 {
		t.Fatalf("push sulla versione corrente fallito: %+v", current)
	}

	// Il reset azzera il blob e avanza la versione: un client rimasto alla
	// versione precedente non può ripristinare la sua copia.
	resetVersion, err := db.NewOrderingRepo(sqlDB).Reset(context.Background(), userID)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if resetVersion != 3 {
		t.Fatalf("versione dopo reset = %d, attesa 3", resetVersion)
	}

	afterReset := pushOrdering(t, h, &clock, userID, 2, `{"folders":{"c":3}}`)
	if afterReset.resp == nil || afterReset.resp.Ordering == nil {
		t.Fatalf("sync dopo reset fallita: %+v", afterReset)
	}
	if afterReset.resp.Ordering.Version != 3 || string(afterReset.resp.Ordering.Data) != `{}` {
		t.Fatalf("reset sovrascritto da un client vecchio: v=%d data=%s", afterReset.resp.Ordering.Version, afterReset.resp.Ordering.Data)
	}
}

func TestQuotaRejectsIncreasesButAllowsCleanup(t *testing.T) {
	sqlDB, userID := newStampTestDB(t)
	clock := time.UnixMilli(1_800_000_000_000)
	h := handlerWithClock(sqlDB, &clock)

	const quota = 20
	if err := db.NewUsersRepo(sqlDB).SetStorageQuota(context.Background(), userID, quota); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	ctx := context.WithValue(context.Background(), middleware.CtxAccountStatus, db.AccountStatus{
		StorageQuotaBytes: quota,
	})

	syncNote := func(n models.NoteDTO) syncResult {
		clock = clock.Add(time.Second)
		return h.process(ctx, userID, &models.SyncRequest{
			Folders: []models.FolderDTO{},
			Notes:   []models.NoteDTO{n},
		})
	}

	// Una nota più grande della quota: il batch viene rifiutato per intero.
	big := syncNote(models.NoteDTO{
		ID: "big", Title: "titolo lungo", Content: "contenuto che supera la quota",
		CreatedAt: 1, UpdatedAt: 1,
	})
	if big.status != 409 || big.code != "quota_exceeded" {
		t.Fatalf("push oltre quota: status=%d code=%q resp=%v", big.status, big.code, big.resp)
	}
	if got, err := db.NewNotesRepo(sqlDB).Get(context.Background(), userID, "big"); err != nil || got != nil {
		t.Fatalf("la nota oltre quota è stata salvata nonostante il 409: %v %v", got, err)
	}

	// Una nota che rientra nella quota passa.
	small := syncNote(models.NoteDTO{
		ID: "small", Title: "ok", Content: "",
		CreatedAt: 1, UpdatedAt: 2,
	})
	if small.resp == nil {
		t.Fatalf("push entro quota fallito: status=%d msg=%q", small.status, small.message)
	}
	if small.resp.Storage.UsageBytes != 2 || small.resp.Storage.QuotaBytes != quota {
		t.Fatalf("storage in risposta = %+v", small.resp.Storage)
	}

	// Ingrandire la nota esistente supera la quota: rifiutato e la versione
	// precedente resta intatta (rollback dell'intero batch).
	grow := syncNote(models.NoteDTO{
		ID: "small", Title: "ok", Content: "0123456789012345678901234567890123456789",
		CreatedAt: 1, UpdatedAt: 3,
	})
	if grow.status != 409 || grow.code != "quota_exceeded" {
		t.Fatalf("crescita oltre quota: status=%d code=%q", grow.status, grow.code)
	}
	got, err := db.NewNotesRepo(sqlDB).Get(context.Background(), userID, "small")
	if err != nil || got == nil || got.Content != "" {
		t.Fatalf("la crescita rifiutata non è stata annullata: %+v %v", got, err)
	}

	// La cancellazione riduce lo spazio e deve passare anche a quota raggiunta.
	deletedAt := int64(4)
	del := syncNote(models.NoteDTO{
		ID: "small", Title: "ok", Content: "", CreatedAt: 1, UpdatedAt: 4,
		DeletedAt: &deletedAt,
	})
	if del.resp == nil {
		t.Fatalf("cancellazione rifiutata: status=%d msg=%q", del.status, del.message)
	}
	if del.resp.Storage.UsageBytes != 0 {
		t.Fatalf("spazio dopo la cancellazione = %d", del.resp.Storage.UsageBytes)
	}
}
