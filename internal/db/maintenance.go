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

	// PRAGMA auto_vacuum è per-connessione finché non viene eseguito un
	// VACUUM: entrambi gli statement devono quindi girare sulla STESSA
	// connessione fisica del pool, non su due connessioni diverse.
	ctx := context.Background()
	pinned, err := conn.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquisizione connessione dedicata: %w", err)
	}
	defer pinned.Close()

	if err := queryDiscard(ctx, pinned, `PRAGMA auto_vacuum=INCREMENTAL;`); err != nil {
		return fmt.Errorf("impostazione auto_vacuum=incremental: %w", err)
	}
	if _, err := pinned.ExecContext(ctx, `VACUUM;`); err != nil {
		return fmt.Errorf("vacuum one-shot di conversione: %w", err)
	}

	log.Println("database: auto_vacuum incrementale abilitato con successo")
	return nil
}

// queryDiscard esegue uno statement (tipicamente un PRAGMA) usando
// l'interfaccia Query anziché Exec e scarta le righe restituite: i PRAGMA di
// libSQL possono restituire il valore impostato, e con Exec il driver può
// lasciare il cursore aperto o rifiutare lo statement.
func queryDiscard(ctx context.Context, conn *sql.Conn, query string) error {
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
	}
	return rows.Err()
}

// vacuumChunkPages è quante pagine libere vengono restituite al filesystem
// per ogni passo di incrementalVacuum (con pagine da 4 KiB sono ≈ 2 MiB).
// Un passo è piccolo abbastanza da durare millisecondi, e abbastanza grande
// da non far diventare i round trip il costo dominante.
const vacuumChunkPages = 512

// incrementalVacuum restituisce al filesystem le pagine libere a piccoli
// passi invece che in un colpo solo.
//
// Perché a passi: in modalità locale il pool ha UNA sola connessione
// (OpenWithConfig). Un PRAGMA incremental_vacuum senza argomento libera tutte
// le pagine in un unico statement e per tutta la sua durata nessun'altra
// richiesta (login, settings, sync) può usare il database; dopo un purge
// grande la durata cresce con le pagine da liberare. Eseguendo un passo alla
// volta la connessione torna al pool tra un passo e l'altro, e le richieste
// in attesa vengono servite prima del passo successivo.
//
// Non cambia cosa viene liberato (solo pagine già libere, mai righe vive) né
// il risultato finale: cambia soltanto per quanto tempo alla volta la
// connessione resta occupata. Se ctx scade a metà, i passi già eseguiti
// restano validi (prima un unico statement interrotto veniva annullato per
// intero e non liberava nulla).
func incrementalVacuum(ctx context.Context, conn *sql.DB) error {
	return incrementalVacuumChunked(ctx, conn, vacuumChunkPages, nil)
}

// incrementalVacuumChunked è incrementalVacuum con dimensione del passo e un
// hook tra i passi, per poter provare nei test che la connessione viene
// davvero rilasciata (con un pool da 1 un hook che usa il DB si bloccherebbe
// per sempre se la connessione fosse ancora tenuta).
func incrementalVacuumChunked(ctx context.Context, conn *sql.DB, chunkPages int, between func()) error {
	prev := int64(-1)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var free int64
		if err := conn.QueryRowContext(ctx, `PRAGMA freelist_count;`).Scan(&free); err != nil {
			return fmt.Errorf("lettura freelist_count: %w", err)
		}
		if free == 0 {
			return nil
		}
		// Difesa contro un ciclo infinito: se un passo non ha liberato nulla
		// il PRAGMA non sta facendo il suo lavoro (es. auto_vacuum non
		// incrementale) e insistere sarebbe solo occupare la connessione.
		if prev >= 0 && free >= prev {
			return fmt.Errorf("incremental_vacuum non libera pagine (freelist_count fermo a %d)", free)
		}
		prev = free

		// chunkPages è un int interno, non input esterno: nessuna iniezione.
		rows, err := conn.QueryContext(ctx, fmt.Sprintf(`PRAGMA incremental_vacuum(%d);`, chunkPages))
		if err != nil {
			return err
		}
		for rows.Next() { // nolint:revive // scarico eventuale result set, vedi drainRows in db.go
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		if between != nil {
			between()
		}
	}
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
