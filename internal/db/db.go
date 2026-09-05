package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	libsql "github.com/tursodatabase/go-libsql"
)

// Config descrive come aprire il database. Path è sempre richiesto (è il file
// .db locale, o la replica locale in modalità embedded replica). PrimaryURL è
// opzionale: se impostato, il file locale diventa una "embedded replica" che
// si sincronizza con un server libSQL/Turso remoto (es. libsql://xxx.turso.io).
type Config struct {
	// Path del file .db locale su disco. In modalità pura locale è l'unico
	// storage; in modalità embedded replica è la replica locale usata per
	// le letture (le scritture vengono comunque instradate al primario).
	Path string

	// PrimaryURL, se non vuoto, abilita la modalità embedded replica verso
	// un server libSQL/Turso remoto (schema libsql://, https:// o http://).
	PrimaryURL string

	// AuthToken usato per autenticarsi contro PrimaryURL.
	AuthToken string

	// SyncInterval, se > 0, abilita la sincronizzazione periodica automatica
	// in background con il primario. Se 0, la replica si sincronizza solo
	// all'apertura (nessun auto-sync in background).
	SyncInterval time.Duration

	// MaxOpenConns imposta il numero massimo di connessioni concorrenti nel
	// pool. 0 => default (vedi Open). libSQL, a differenza dei classici
	// binding SQLite "single-writer serializzato", gestisce internamente la
	// concorrenza lettori/scrittore in WAL, quindi è sicuro aprire più
	// connessioni: ogni lettura può avvenire su una connessione propria
	// mentre uno scrittore è in corso, senza serializzare tutto lato Go.
	MaxOpenConns int
}

// pragmaStatements sono i PRAGMA che vogliamo garantiti su OGNI connessione
// fisica del pool (in SQLite/libSQL molti PRAGMA sono per-connessione e non
// persistono nel file, journal_mode escluso). Per questo li applichiamo con
// un driver.Connector "decorato" (vedi pragmaConnector) invece che una volta
// sola all'apertura: con MaxOpenConns > 1, ogni nuova connessione aperta dal
// pool passerebbe altrimenti con foreign_keys/synchronous/busy_timeout ai
// valori di default di libSQL.
var pragmaStatements = []string{
	"PRAGMA busy_timeout=5000;",  // attende fino a 5s prima di "database is locked"
	"PRAGMA journal_mode=WAL;",   // lettori non bloccano lo scrittore (e viceversa)
	"PRAGMA synchronous=NORMAL;", // sicuro in WAL, fsync solo ai checkpoint
	"PRAGMA foreign_keys=ON;",    // integrità referenziale (cascade su cancellazione utente)
}

// Open apre (creando se necessario) il DB SQLite locale tramite il driver
// libSQL, con i PRAGMA ottimizzati per un server a bassa/media concorrenza.
// È la firma "semplice", equivalente a OpenWithConfig(Config{Path: path}):
// nessuna sincronizzazione remota, solo file locale.
func Open(path string) (*sql.DB, error) {
	return OpenWithConfig(Config{Path: path})
}

// OpenWithConfig apre il DB in modalità locale pura (PrimaryURL vuoto) oppure
// in modalità embedded replica (PrimaryURL valorizzato, tipicamente verso
// Turso). In entrambi i casi lo schema esposto a database/sql è identico:
// i repository in internal/db/*_repo.go non necessitano di alcuna modifica.
func OpenWithConfig(cfg Config) (*sql.DB, error) {
	if cfg.Path == "" {
		return nil, fmt.Errorf("db: Path non può essere vuoto")
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir data dir: %w", err)
	}

	rawConnector, mode, err := newLibsqlConnector(cfg)
	if err != nil {
		return nil, err
	}

	var conn *sql.DB
	maxOpen := cfg.MaxOpenConns

	if rawConnector != nil {
		// Modalità embedded replica: pieno controllo sul Connector, i PRAGMA
		// vengono garantiti su ogni connessione fisica del pool.
		conn = sql.OpenDB(wrapWithPragmas(rawConnector, pragmaStatements))
		if maxOpen <= 0 {
			// Le scritture vengono comunque instradate al primario remoto:
			// più lettori possono servire la replica locale in parallelo.
			maxOpen = 4
		}
	} else {
		// Modalità locale pura: driver registrato standard, connessione
		// singola serializzata (come da comportamento storico) con i PRAGMA
		// applicati una sola volta all'apertura.
		conn, err = sql.Open("libsql", "file:"+cfg.Path)
		if err != nil {
			return nil, fmt.Errorf("open libsql locale: %w", err)
		}
		maxOpen = 1
	}

	conn.SetMaxOpenConns(maxOpen)
	conn.SetMaxIdleConns(maxOpen)
	conn.SetConnMaxIdleTime(5 * time.Minute)
	conn.SetConnMaxLifetime(0) // nessun limite: file locale/replica, non connessione di rete

	if err := conn.Ping(); err != nil {
		return nil, fmt.Errorf("ping libsql: %w", err)
	}

	if rawConnector == nil {
		// Applicazione one-shot dei PRAGMA: con MaxOpenConns=1 la stessa
		// connessione fisica viene riutilizzata per tutta la vita del pool,
		// quindi non serve riapplicarli ad ogni nuova connessione.
		//
		// NB: usiamo Query e non Exec. A differenza di molti driver SQLite,
		// go-libsql restituisce un errore ("Execute returned rows") se un
		// PRAGMA che produce un risultato (es. journal_mode, busy_timeout
		// rispondono col valore impostato) viene lanciato con Exec invece
		// che con Query.
		for _, stmt := range pragmaStatements {
			if err := queryDiscard(conn, stmt); err != nil {
				return nil, fmt.Errorf("pragma %q: %w", stmt, err)
			}
		}
	}

	if err := migrate(conn); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}

	log.Printf("database aperto via libSQL in modalità %s (WAL, synchronous=NORMAL, max_open_conns=%d): %s", mode, maxOpen, cfg.Path)
	return conn, nil
}

// newLibsqlConnector costruisce il driver.Connector giusto in base alla
// configurazione: locale puro oppure embedded replica sincronizzata con un
// primario remoto (libsql.NewEmbeddedReplicaConnector).
//
// Nota implementativa: il driver go-libsql espone un *libsql.Connector
// "pubblico" (con Connect/Driver/Close, decorabile con pragmaConnector) solo
// per la modalità embedded replica. Per l'apertura di un semplice file locale
// il driver si registra invece come driver SQL standard sotto il nome
// "libsql" (sql.Register("libsql", ...)), senza esporre un Connector
// pubblico: per questo la modalità locale usa sql.OpenDB tramite il registry
// standard di database/sql, e i PRAGMA vengono applicati una sola volta sulla
// connessione dopo l'apertura (esattamente come faceva il driver precedente),
// mantenendo MaxOpenConns=1 per coerenza. La modalità embedded replica,
// invece, sfrutta pienamente il pragmaConnector per abilitare più connessioni
// in lettura in sicurezza.
func newLibsqlConnector(cfg Config) (driver.Connector, string, error) {
	if cfg.PrimaryURL == "" {
		return nil, "locale", nil // nil = usa il percorso sql.Open("libsql", ...) in OpenWithConfig
	}

	opts := []libsql.Option{libsql.WithAuthToken(cfg.AuthToken)}
	if cfg.SyncInterval > 0 {
		opts = append(opts, libsql.WithSyncInterval(cfg.SyncInterval))
	}

	connector, err := libsql.NewEmbeddedReplicaConnector(cfg.Path, cfg.PrimaryURL, opts...)
	if err != nil {
		return nil, "", fmt.Errorf("apertura libsql embedded replica (%s): %w", cfg.PrimaryURL, err)
	}
	mode := fmt.Sprintf("embedded-replica[primary=%s]", cfg.PrimaryURL)
	return connector, mode, nil
}

// pragmaConnector decora un driver.Connector applicando una lista di PRAGMA
// subito dopo l'apertura di ogni singola connessione fisica. È il modo
// corretto di garantire PRAGMA per-connessione (busy_timeout, synchronous,
// foreign_keys) quando il pool di database/sql può aprirne più di una.
type pragmaConnector struct {
	driver.Connector
	pragmas []string
}

func wrapWithPragmas(c driver.Connector, pragmas []string) driver.Connector {
	return &pragmaConnector{Connector: c, pragmas: pragmas}
}

func (p *pragmaConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := p.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	for _, stmt := range p.pragmas {
		if err := execOnConn(ctx, conn, stmt); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("applicazione %q: %w", stmt, err)
		}
	}
	return conn, nil
}

// Close inoltra la chiusura al connector libSQL sottostante, che libera le
// risorse native (handle CGO) aperte da NewConnector/NewEmbeddedReplicaConnector.
// database/sql chiama automaticamente questo metodo da (*sql.DB).Close() se il
// connector implementa io.Closer, quindi non serve alcuna modifica in main.go.
func (p *pragmaConnector) Close() error {
	if closer, ok := p.Connector.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func execOnConn(ctx context.Context, conn driver.Conn, query string) error {
	// Come in queryDiscard: i PRAGMA di libSQL possono restituire righe
	// (es. il valore impostato), quindi usiamo l'interfaccia Query e non Exec.
	if queryer, ok := conn.(driver.QueryerContext); ok {
		rows, err := queryer.QueryContext(ctx, query, nil)
		if err != nil {
			return err
		}
		defer rows.Close()
		return drainRows(rows)
	}
	if queryer, ok := conn.(driver.Queryer); ok { //nolint:staticcheck // fallback per driver senza supporto context
		rows, err := queryer.Query(query, nil)
		if err != nil {
			return err
		}
		defer rows.Close()
		return drainRows(rows)
	}
	return fmt.Errorf("il driver libsql non espone QueryerContext/Queryer")
}

func drainRows(rows driver.Rows) error {
	dest := make([]driver.Value, len(rows.Columns()))
	for {
		if err := rows.Next(dest); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func queryDiscard(conn *sql.DB, query string) error {
	rows, err := conn.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()
	// Scarica il result set (se presente) e propaga eventuali errori di lettura.
	for rows.Next() {
	}
	return rows.Err()
}

// migrationStatements sono le singole DDL dello schema. A differenza dei
// driver SQLite "classici" (mattn/go-sqlite3, modernc.org/sqlite), il driver
// libSQL esegue una sola statement per chiamata Exec/Query: una singola
// stringa con più CREATE TABLE separati da ";" esegue silenziosamente solo
// la prima e ignora le altre, senza errore. Per questo lo schema è diviso in
// statement singoli, eseguiti in sequenza.
var migrationStatements = []string{
	`CREATE TABLE IF NOT EXISTS users (
		id            TEXT PRIMARY KEY,
		username      TEXT NOT NULL UNIQUE,
		password_hash TEXT NOT NULL,
		created_at    INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS notes (
		id            TEXT PRIMARY KEY,
		user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		relative_path TEXT NOT NULL,
		updated_at    INTEGER NOT NULL,
		deleted       INTEGER NOT NULL DEFAULT 0,
		checksum      TEXT NOT NULL DEFAULT '',
		is_folder     INTEGER NOT NULL DEFAULT 0,
		UNIQUE(user_id, relative_path)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_notes_user ON notes(user_id)`,
	`CREATE TABLE IF NOT EXISTS user_settings (
		user_id       TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
		settings_json TEXT NOT NULL,
		updated_at    INTEGER NOT NULL
	)`,
}

func migrate(conn *sql.DB) error {
	for _, stmt := range migrationStatements {
		if _, err := conn.Exec(stmt); err != nil {
			return fmt.Errorf("statement %q: %w", stmt, err)
		}
	}
	// CREATE TABLE IF NOT EXISTS non altera una tabella "notes" già esistente
	// creata da una versione precedente del server (prima dell'introduzione
	// del supporto alle cartelle vuote): su un DB già in produzione la colonna
	// is_folder andrebbe quindi aggiunta esplicitamente con ALTER TABLE.
	return ensureNotesIsFolderColumn(conn)
}

// ensureNotesIsFolderColumn aggiunge la colonna "is_folder" alla tabella
// "notes" se manca ancora (DB creato da una versione precedente del server),
// così la migrazione è sicura sia su installazioni nuove (dove la colonna è
// già presente dalla CREATE TABLE sopra) sia su installazioni esistenti.
func ensureNotesIsFolderColumn(conn *sql.DB) error {
	rows, err := conn.Query(`PRAGMA table_info(notes)`)
	if err != nil {
		return fmt.Errorf("pragma table_info(notes): %w", err)
	}
	defer rows.Close()

	hasColumn := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return fmt.Errorf("scan table_info(notes): %w", err)
		}
		if name == "is_folder" {
			hasColumn = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterazione table_info(notes): %w", err)
	}
	if hasColumn {
		return nil
	}

	if _, err := conn.Exec(`ALTER TABLE notes ADD COLUMN is_folder INTEGER NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("alter table notes add is_folder: %w", err)
	}
	log.Println("migrazione: aggiunta colonna 'is_folder' alla tabella 'notes' (supporto cartelle vuote)")
	return nil
}
