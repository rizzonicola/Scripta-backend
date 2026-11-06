package db

import (
	"context"
	"fmt"
)

// storageRowOverheadBytes è una stima fissa del costo di una riga (record
// SQLite, indici, id e timestamp). Serve a impedire che la quota venga aggirata
// creando un numero illimitato di note/cartelle vuote o di tombstone: senza
// questa voce solo il testo sarebbe contato.
const storageRowOverheadBytes int64 = 256

// StorageRepo calcola lo spazio di sincronizzazione occupato da un account.
// Conta tutte le righe, anche i tombstone. Il testo delle note cancellate
// viene azzerato quando vengono scritte (vedi NotesRepo.ForceSet e api_sync),
// quindi eliminare una nota libera subito il contenuto; restano l'overhead
// della riga e l'eventuale nome di una cartella cancellata fino alla purge.
type StorageRepo struct {
	db execer
}

func NewStorageRepo(d execer) *StorageRepo {
	return &StorageRepo{db: d}
}

// storageUsageSQL restituisce l'espressione SQL che calcola lo spazio occupato
// dall'account identificato da userIDExpr. Un solo posto in cui vive la
// formula: la usano sia Usage (singolo utente) sia UsersRepo.List (dashboard).
// LENGTH(CAST(... AS BLOB)) conta i byte UTF-8, non i caratteri.
func storageUsageSQL(userIDExpr string) string {
	return fmt.Sprintf(`
		COALESCE((SELECT SUM(
			%d +
			LENGTH(CAST(COALESCE(title, '') AS BLOB)) +
			LENGTH(CAST(COALESCE(content, '') AS BLOB))
		) FROM notes WHERE notes.user_id = %s), 0) +
		COALESCE((SELECT SUM(
			%d + LENGTH(CAST(COALESCE(name, '') AS BLOB))
		) FROM folders WHERE folders.user_id = %s), 0) +
		COALESCE((SELECT LENGTH(CAST(ordering_json AS BLOB))
			FROM user_ordering WHERE user_ordering.user_id = %s), 0)`,
		storageRowOverheadBytes, userIDExpr,
		storageRowOverheadBytes, userIDExpr,
		userIDExpr)
}

// Usage restituisce i byte occupati da titoli/contenuti delle note, nomi delle
// cartelle e blob di posizionamento, più un overhead fisso per riga.
func (r *StorageRepo) Usage(ctx context.Context, userID string) (int64, error) {
	var usage int64
	err := r.db.QueryRowContext(ctx, `SELECT `+storageUsageSQL("?"), userID, userID, userID).Scan(&usage)
	return usage, err
}
