package handlers

import "testing"

func TestNormalizeNoteTimestamps(t *testing.T) {
	const now int64 = 10_000_000
	skew := MaxClockSkew.Milliseconds()

	cases := []struct {
		name          string
		createdAt     int64
		updatedAt     int64
		wantCreatedAt int64
		wantUpdatedAt int64
	}{
		{
			name:          "created_at valido precedente a updated_at",
			createdAt:     4_000,
			updatedAt:     5_000,
			wantCreatedAt: 4_000,
			wantUpdatedAt: 5_000,
		},
		{
			name:          "created_at successivo a updated_at",
			createdAt:     6_000,
			updatedAt:     5_000,
			wantCreatedAt: 5_000,
			wantUpdatedAt: 5_000,
		},
		{
			name:          "created_at nel futuro",
			createdAt:     now + skew + 1,
			updatedAt:     5_000,
			wantCreatedAt: 5_000,
			wantUpdatedAt: 5_000,
		},
		{
			name:          "entrambi nel futuro",
			createdAt:     now + skew + 1,
			updatedAt:     now + skew + 2,
			wantCreatedAt: now,
			wantUpdatedAt: now,
		},
		{
			name:          "updated_at assente",
			createdAt:     4_000,
			wantCreatedAt: 4_000,
			wantUpdatedAt: now,
		},
		{
			name:          "entrambi assenti",
			wantCreatedAt: now,
			wantUpdatedAt: now,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotCreatedAt, gotUpdatedAt := normalizeNoteTimestamps(c.createdAt, c.updatedAt, now)
			if gotCreatedAt != c.wantCreatedAt || gotUpdatedAt != c.wantUpdatedAt {
				t.Fatalf("ottenuti (%d, %d), attesi (%d, %d)",
					gotCreatedAt, gotUpdatedAt, c.wantCreatedAt, c.wantUpdatedAt)
			}
			if gotCreatedAt > gotUpdatedAt {
				t.Fatalf("invariante violata: created_at %d > updated_at %d", gotCreatedAt, gotUpdatedAt)
			}
		})
	}
}
