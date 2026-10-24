package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"notes-server/internal/db"
	"notes-server/internal/middleware"
)

type fakeUserLimiter struct {
	allow bool
	calls int
}

func (f *fakeUserLimiter) Allow(string) bool {
	f.calls++
	return f.allow
}

func TestSyncRateLimitHonorsGlobalToggleAndExemption(t *testing.T) {
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "sync_limit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()

	settings := db.NewServerSettingsRepo(sqlDB)
	h := NewSyncHandler(sqlDB)
	limiter := &fakeUserLimiter{}
	h.UserLimiter = limiter

	call := func(status db.AccountStatus) (int, string) {
		ctx := context.WithValue(context.Background(), middleware.CtxUserID, "u1")
		ctx = context.WithValue(ctx, middleware.CtxAccountStatus, status)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/sync",
			strings.NewReader(`{"last_synced_at":0,"folders":[],"notes":[]}`)).WithContext(ctx)
		rec := httptest.NewRecorder()
		h.Sync(rec, req)
		return rec.Code, rec.Body.String()
	}

	limiter.allow = false
	code, body := call(db.AccountStatus{})
	if code != http.StatusTooManyRequests || !strings.Contains(body, `"code":"rate_limited"`) {
		t.Fatalf("limite attivo: %d %s", code, body)
	}
	if limiter.calls != 1 {
		t.Fatalf("limiter chiamato %d volte, atteso 1", limiter.calls)
	}

	if err := settings.SetUserRateLimitEnabled(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if code, body := call(db.AccountStatus{}); code != http.StatusOK {
		t.Fatalf("toggle globale off: %d %s", code, body)
	}

	if err := settings.SetUserRateLimitEnabled(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if code, body := call(db.AccountStatus{RateLimitExempt: true}); code != http.StatusOK {
		t.Fatalf("esenzione utente: %d %s", code, body)
	}
	if limiter.calls != 1 {
		t.Fatalf("il limiter è stato consultato per un utente esente o con limite off: %d chiamate", limiter.calls)
	}
}
