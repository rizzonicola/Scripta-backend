package db

import (
	"context"
	"database/sql"
)

// StampHWMKey è la chiave di sync_state che conserva l'high-water mark degli
// stamp (server_time) che il server può aver consegnato ai client. Vedi
// SyncHandler.reserveStampTx per il ragionamento.
const StampHWMKey = "stamp_hwm"

// StampLastKey è la chiave di sync_state che conserva l'ULTIMO stamp
// effettivamente allocato. L'allocazione avviene con AllocateStamp come prima
// scrittura della transazione di sync: è il lock di scrittura SQLite a
// serializzare i processi, quindi l'ordine degli stamp coincide con l'ordine
// di commit anche con più istanze sullo stesso database.
const StampLastKey = "stamp_last"

// SyncStateRepo è un minuscolo archivio chiave→intero per lo stato del
// protocollo di sync che deve sopravvivere al riavvio del processo.
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

// AllocateStamp assegna il prossimo stamp: max(candidate, ultimo+1). La
// INSERT ... ON CONFLICT è atomica e, se eseguita su un *sql.Tx, è la prima
// scrittura della transazione: due processi concorrenti si serializzano sul
// lock di scrittura SQLite e il secondo vede l'aggiornamento del primo.
//
// DEVE essere chiamata dentro la transazione che scrive le righe con lo stamp
// restituito: così l'ordine di commit è anche l'ordine degli stamp e un
// cursore consegnato non può «scavalcare» una sync che committa dopo.
func (r *SyncStateRepo) AllocateStamp(ctx context.Context, candidate int64) (int64, error) {
	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO sync_state (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = max(sync_state.value + 1, excluded.value)
	`, StampLastKey, candidate); err != nil {
		return 0, err
	}
	return r.GetInt(ctx, StampLastKey)
}
