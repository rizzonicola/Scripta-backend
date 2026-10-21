package db

import (
	"context"
	"database/sql"
)

// StampHWMKey è la chiave di sync_state che conserva l'high-water mark degli
// stamp (server_time) che il server può aver consegnato ai client. Vedi
// SyncHandler.reserveStamp per il ragionamento.
const StampHWMKey = "stamp_hwm"

// SyncStateRepo è un minuscolo archivio chiave→intero per lo stato del
// protocollo di sync che deve sopravvivere al riavvio del processo. Oggi
// contiene solo StampHWMKey.
//
// La tabella sync_state è nuova ed è creata con CREATE TABLE IF NOT EXISTS: un
// binario più vecchio non la conosce e la ignora, quindi può aprire un DB già
// usato dal server nuovo (E.5 del report).
type SyncStateRepo struct {
	db execer
}

func NewSyncStateRepo(d execer) *SyncStateRepo {
	return &SyncStateRepo{db: d}
}

// GetInt restituisce il valore di una chiave, o 0 se la chiave non esiste.
func (r *SyncStateRepo) GetInt(ctx context.Context, key string) (int64, error) {
	var v int64
	err := r.db.QueryRowContext(ctx, `SELECT value FROM sync_state WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return v, nil
}

// SetMaxInt scrive il valore solo se è maggiore di quello già presente (o se
// la chiave non esiste): un high-water mark non può mai scendere, nemmeno se
// due scritture arrivano fuori ordine.
func (r *SyncStateRepo) SetMaxInt(ctx context.Context, key string, value int64) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO sync_state (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
		WHERE excluded.value > sync_state.value
	`, key, value)
	return err
}
