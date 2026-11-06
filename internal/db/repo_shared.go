package db

import "context"

// purgeExpiredTombstones è la parte SQL comune a NotesRepo e FoldersRepo: le
// due tabelle hanno le stesse colonne deleted_at/synced_at, quindi la regola
// di scadenza non va duplicata. Il nome tabella è sempre una costante interna
// al package, mai input dell'utente.
func purgeExpiredTombstones(ctx context.Context, d execer, table string, olderThan int64) (int64, error) {
	res, err := d.ExecContext(ctx,
		`DELETE FROM `+table+` WHERE deleted_at IS NOT NULL AND synced_at < ?`,
		olderThan,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// touchSynced è la parte SQL comune a NotesRepo e FoldersRepo: ri-consegna una
// riga che ha vinto il confronto LWW senza modificarne il contenuto.
func touchSynced(ctx context.Context, d execer, table, userID, id string, syncedAt int64) error {
	_, err := d.ExecContext(ctx,
		`UPDATE `+table+` SET synced_at = ? WHERE id = ? AND user_id = ?`,
		syncedAt, id, userID,
	)
	return err
}
