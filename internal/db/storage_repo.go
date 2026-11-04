package db

import "context"

// StorageRepo calcola lo spazio di sincronizzazione occupato da un account.
// Conta solo le righe ATTIVE: cancellare note o cartelle libera spazio
// immediatamente, anche prima della purge dei tombstone.
type StorageRepo struct {
	db execer
}

func NewStorageRepo(d execer) *StorageRepo {
	return &StorageRepo{db: d}
}

// Usage restituisce i byte occupati da titoli/contenuti delle note e nomi
// delle cartelle attive. LENGTH(CAST(... AS BLOB)) conta i byte UTF-8, non i
// caratteri.
func (r *StorageRepo) Usage(ctx context.Context, userID string) (int64, error) {
	var usage int64
	err := r.db.QueryRowContext(ctx, `
		SELECT
			COALESCE((SELECT SUM(
				LENGTH(CAST(COALESCE(title, '') AS BLOB)) +
				LENGTH(CAST(COALESCE(content, '') AS BLOB))
			) FROM notes WHERE user_id = ? AND deleted_at IS NULL), 0) +
			COALESCE((SELECT SUM(LENGTH(CAST(name AS BLOB)))
				FROM folders WHERE user_id = ? AND deleted_at IS NULL), 0)`,
		userID, userID,
	).Scan(&usage)
	return usage, err
}
