package db

import (
	"context"
	"database/sql"
	"fmt"
	"log"
)

// ---------------------------------------------------------------------------
// Recupero spazio su disco.
//
// PurgeExpiredTombstones (vedi notes_repo.go/folders_repo.go) esegue un
// hard-delete SQL sui tombstone scaduti: le righe spariscono dalle tabelle,
// ma in SQLite/libSQL un DELETE marca semplicemente le pagine occupate come
// "libere" nel file .db, senza restituirle al filesystem né riscrivere il
// file più piccolo. Allo stesso modo, journal_mode=WAL scrive ogni modifica
// in app.db-wal: un checkpoint automatico (PASSIVE) sposta i frame nel file
// principale quando il WAL supera ~1000 pagine, ma NON tronca il file
// app.db-wal alla dimensione minima se è rimasto aperto un qualsiasi lettore.
// Risultato: senza un intervento esplicito, sia app.db sia app.db-wal
// possono crescere indefinitamente anche se il contenuto "vivo" resta
// piccolo — esattamente il comportamento osservato in produzione.
//
// Questo file implementa il recupero automatico, in due fasi indipendenti:
//
//  1. incrementalVacuum: PRAGMA incremental_vacuum, che restituisce al
//     filesystem le pagine libere e tronca app.db di conseguenza. Richiede
//     che il DB sia stato aperto con auto_vacuum=INCREMENTAL (vedi
//     bootstrapIncrementalVacuum in db.go): a differenza di un VACUUM
//     completo, che riscrive l'intero file e richiede un lock esclusivo
//     anche per DB da pochi MB, incremental_vacuum sposta solo le pagine
//     effettivamente libere ed è quindi un'operazione economica, sicura da
//     eseguire periodicamente anche su un'istanza in produzione a basso
//     traffico senza percepibili blocchi.
//  2. walCheckpointTruncate: PRAGMA wal_checkpoint(TRUNCATE), che forza un
//     checkpoint completo del WAL nel file principale e tronca app.db-wal.
//
// Entrambe le operazioni sono NO-OP sicuri se non c'è nulla da recuperare.
// ---------------------------------------------------------------------------

// RunMaintenance esegue un ciclo di manutenzione del file .db locale.
//
// isLocal deve essere true SOLO quando il DB è aperto in modalità locale
// pura (Config.PrimaryURL vuoto in db.go/OpenWithConfig). In modalità
// embedded replica il file locale è una copia sincronizzata via frame WAL
// da un server primario remoto (Turso/libSQL): un incremental_vacuum o un
// wal_checkpoint(TRUNCATE) eseguiti localmente riorganizzerebbero le pagine
// della replica in un modo che il protocollo di sync non si aspetta,
// rischiando un disallineamento (o un rollback forzato della replica) al
// giro di sync successivo. In quella modalità la gestione dello spazio su
// disco compete al primario remoto: qui ci si limita quindi a saltare
// l'operazione (la funzione ritorna nil senza fare nulla).
//
// reclaimPages controla se eseguire anche l'incremental_vacuum (fase 1,
// leggermente più costosa): va passato a true solo quando il chiamante sa
// che sono state effettivamente cancellate righe di recente (es. subito
// dopo un giro di PurgeExpiredTombstones con risultato > 0), per evitare di
// lavorare a vuoto. Il wal_checkpoint(TRUNCATE) (fase 2) viene invece
// sempre tentato, perché il WAL cresce anche per il normale traffico di
// scrittura (note salvate, sync) indipendentemente dai tombstone.
func RunMaintenance(ctx context.Context, conn *sql.DB, isLocal bool, reclaimPages bool) error {
	if !isLocal {
		// Embedded replica: nessuna riscrittura locale delle pagine, vedi
		// commento sopra. Non è un errore, è la modalità di funzionamento
		// attesa: il chiamante non deve loggare questo caso come fallimento.
		return nil
	}

	if reclaimPages {
		if err := incrementalVacuum(ctx, conn); err != nil {
			return fmt.Errorf("incremental_vacuum: %w", err)
		}
	}

	if err := walCheckpointTruncate(ctx, conn); err != nil {
		return fmt.Errorf("wal_checkpoint(truncate): %w", err)
	}
	return nil
}

// bootstrapIncrementalVacuum garantisce che il DB locale sia in modalità
// auto_vacuum=INCREMENTAL, condizione necessaria perché PRAGMA
// incremental_vacuum abbia effetto (vedi
// https://sqlite.org/pragma.html#pragma_auto_vacuum: cambiare auto_vacuum
// su un DB che contiene già pagine richiede un VACUUM completo per
// riorganizzare il file secondo il nuovo schema; su un DB nuovo/vuoto il
// PRAGMA da solo è sufficiente).
//
// Viene chiamata una sola volta, subito dopo l'apertura, e solo in modalità
// locale pura (mai su un'embedded replica, per lo stesso motivo spiegato in
// RunMaintenance). È idempotente: se auto_vacuum è già INCREMENTAL (es. ai
// riavvii successivi al primo) non fa nulla, quindi il costo del VACUUM
// completo una-tantum viene pagato al massimo una volta nella vita del file
// .db, non ad ogni avvio del server.
func bootstrapIncrementalVacuum(conn *sql.DB) error {
	const incrementalMode = 2 // valore restituito da "PRAGMA auto_vacuum" quando è INCREMENTAL

	var mode int
	if err := conn.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		return fmt.Errorf("lettura auto_vacuum: %w", err)
	}
	if mode == incrementalMode {
		return nil
	}

	log.Println("database: auto_vacuum non incrementale, abilitazione in corso (richiede un VACUUM completo una-tantum: può richiedere qualche secondo su database di grandi dimensioni, i futuri hard-delete verranno invece recuperati in modo incrementale ed economico)...")

	if err := queryDiscard(conn, `PRAGMA auto_vacuum=INCREMENTAL;`); err != nil {
		return fmt.Errorf("impostazione auto_vacuum=incremental: %w", err)
	}
	if _, err := conn.Exec(`VACUUM;`); err != nil {
		return fmt.Errorf("vacuum one-shot di conversione: %w", err)
	}

	log.Println("database: auto_vacuum incrementale abilitato con successo")
	return nil
}

func incrementalVacuum(ctx context.Context, conn *sql.DB) error {
	// PRAGMA incremental_vacuum (senza argomento) restituisce tutte le
	// pagine attualmente libere, non solo un numero limitato: va bene per un
	// job periodico in background che non ha fretta e preferisce lasciare il
	// file il più compatto possibile tra un giro e l'altro.
	rows, err := conn.QueryContext(ctx, `PRAGMA incremental_vacuum;`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() { // nolint:revive // scarico eventuale result set, vedi drainRows in db.go
	}
	return rows.Err()
}

func walCheckpointTruncate(ctx context.Context, conn *sql.DB) error {
	// PRAGMA wal_checkpoint(TRUNCATE) restituisce una riga
	// (busy, log_frames, checkpointed): busy != 0 significa che un lettore o
	// scrittore concorrente ha impedito un checkpoint/truncate completo (non
	// è un errore, capita normalmente sotto traffico concorrente) — in quel
	// caso il WAL viene comunque riportato al minimo possibile e il resto
	// verrà recuperato al giro successivo.
	rows, err := conn.QueryContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE);`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var busy, logFrames, checkpointed int
	for rows.Next() {
		if err := rows.Scan(&busy, &logFrames, &checkpointed); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if busy != 0 {
		log.Printf("wal_checkpoint(TRUNCATE): checkpoint parziale (busy=%d, log_frames=%d, checkpointed=%d), un lettore/scrittore attivo ha impedito il truncate completo del WAL; verrà ritentato al prossimo giro", busy, logFrames, checkpointed)
	}
	return nil
}
