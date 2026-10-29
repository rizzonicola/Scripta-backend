package handlers

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"notes-server/internal/db"
	"notes-server/internal/models"
)

func benchmarkSync(b *testing.B, notesPerBatch int) {
	b.Helper()
	sqlDB, err := db.Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = sqlDB.Close() })
	user, err := db.NewUsersRepo(sqlDB).Create(context.Background(), "bench", "hash")
	if err != nil {
		b.Fatal(err)
	}

	h := NewSyncHandler(sqlDB)
	ctx := context.Background()
	last := int64(0)
	base := time.Now().Add(-time.Hour).UnixMilli()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := &models.SyncRequest{
			LastSyncedAt: last,
			Notes:        make([]models.NoteDTO, notesPerBatch),
		}
		for j := range req.Notes {
			ts := base + int64(i*notesPerBatch+j)
			req.Notes[j] = models.NoteDTO{
				ID:        fmt.Sprintf("note-%d-%d", i, j),
				Title:     "nota",
				Content:   "contenuto della nota",
				CreatedAt: ts,
				UpdatedAt: ts,
			}
		}
		res := h.process(ctx, user.ID, req)
		if res.resp == nil {
			b.Fatalf("sync fallita: status=%d message=%q rejected=%v", res.status, res.message, res.rejected)
		}
		last = res.resp.ServerTime
	}
}

func BenchmarkSyncProcess100Notes(b *testing.B) {
	benchmarkSync(b, 100)
}

func BenchmarkSyncProcess1000Notes(b *testing.B) {
	benchmarkSync(b, 1000)
}
