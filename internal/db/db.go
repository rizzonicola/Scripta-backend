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
		// Modalità locale pura: connessione singola serializzata, MA con i
		// PRAGMA applicati a OGNI nuova connessione fisica (vedi
		// newLocalConnector). Prima venivano applicati una sola volta
		// dopo l'apertura: con SetConnMaxIdleTime la connessione veniva
		// chiusa dopo l'inattività e quella riaperta dal pool partiva con
		// foreign_keys=OFF (default di SQLite), disattivando in silenzio
		// le ON DELETE CASCADE (cancellazione utente) e i vincoli FK.
		//
		// NB: il driver libSQL NON supporta parametri DSN tipo
		// "_fk=1&_busy_timeout=5000" (sono specifici di mattn/go-sqlite3 e
		// modernc): verrebbero ignorati. L'equivalente corretto per questo
		// driver è un init-hook per-connessione, che è ciò che fa il
		// pragmaConnector già usato per la modalità embedded replica.
		localConnector, cerr := newLocalConnector("file:" + cfg.Path)
		if cerr != nil {
			return nil, fmt.Errorf("open libsql locale: %w", cerr)
		}
		conn = sql.OpenDB(wrapWithPragmas(localConnector, pragmaStatements))
		maxOpen = 1
	}

	conn.SetMaxOpenConns(maxOpen)
	conn.SetMaxIdleConns(maxOpen)
	conn.SetConnMaxIdleTime(5 * time.Minute)
	conn.SetConnMaxLifetime(0) // nessun limite: file locale/replica, non connessione di rete

	if err := conn.Ping(); err != nil {
		return nil, fmt.Errorf("ping libsql: %w", err)
	}

	// Verifica esplicita: se foreign_keys non risulta attivo, meglio non
	// avviare il server che perdere in silenzio l'integrità referenziale.
	if err := verifyForeignKeys(conn); err != nil {
		return nil, err
	}

	if err := migrate(conn); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}

	if rawConnector == nil {
		// Solo in modalità locale pura: abilita auto_vacuum=INCREMENTAL (con
		// un VACUUM di conversione una-tantum se necessario) così che la
		// manutenzione periodica in background (vedi RunMaintenance in
		// maintenance.go, avviata da main.go) possa recuperare lo spazio
		// liberato dagli hard-delete tramite PRAGMA incremental_vacuum,
		// un'operazione economica ripetibile spesso senza bloccare il
		// server. In modalità embedded replica questo passo viene saltato
		// di proposito: vedi il commento su RunMaintenance per il motivo.
		if err := bootstrapIncrementalVacuum(conn); err != nil {
			log.Printf("attenzione: impossibile abilitare auto_vacuum incrementale (%v); il recupero automatico dello spazio su disco resterà disattivato finché il problema persiste, ma la cancellazione dei dati (hard-delete dei tombstone) non è comunque affetta", err)
		}
	}

	log.Printf("database aperto via libSQL in modalità %s (WAL, synchronous=NORMAL, max_open_conns=%d): %s", mode, maxOpen, cfg.Path)
	return conn, nil
}

// newLibsqlConnector costruisce il driver.Connector giusto in base alla
// configurazione: locale puro oppure embedded replica sincronizzata con un
// primario remoto (libsql.NewEmbeddedReplicaConnector).
//
// Nota implementativa: il driver go-libsql espone un *libsql.Connector
// "pubblico" solo per la modalità embedded replica. Per il file locale il
// driver è registrato in database/sql sotto il nome "libsql": newLocalConnector
// ne ricava un driver.Connector, così ENTRAMBE le modalità passano da
// wrapWithPragmas e i PRAGMA (foreign_keys incluso) sono applicati a ogni
// connessione fisica, anche dopo che il pool chiude quelle inattive.
func newLibsqlConnector(cfg Config) (driver.Connector, string, error) {
	if cfg.PrimaryURL == "" {
		return nil, "locale", nil // nil = connector locale costruito da newLocalConnector in OpenWithConfig
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

// verifyForeignKeys controlla che PRAGMA foreign_keys sia effettivamente
// attivo sulla connessione del pool.
func verifyForeignKeys(conn *sql.DB) error {
	var on int
	if err := conn.QueryRow("PRAGMA foreign_keys").Scan(&on); err != nil {
		return fmt.Errorf("lettura PRAGMA foreign_keys: %w", err)
	}
	if on != 1 {
		return fmt.Errorf("PRAGMA foreign_keys non attivo: le ON DELETE CASCADE non sarebbero applicate")
	}
	return nil
}

// dsnConnector adatta un driver.Driver "classico" (solo Open(dsn)) a
// driver.Connector, così da poterlo decorare con pragmaConnector.
type dsnConnector struct {
	drv driver.Driver
	dsn string
}

func (c *dsnConnector) Connect(context.Context) (driver.Conn, error) { return c.drv.Open(c.dsn) }
func (c *dsnConnector) Driver() driver.Driver                        { return c.drv }

// newLocalConnector costruisce un driver.Connector per il file locale a
// partire dal driver "libsql" registrato in database/sql, senza dipendere da
// API non pubbliche del binding.
func newLocalConnector(dsn string) (driver.Connector, error) {
	probe, err := sql.Open("libsql", dsn)
	if err != nil {
		return nil, err
	}
	drv := probe.Driver()
	_ = probe.Close() // sql.Open non apre connessioni: chiude solo il pool "sonda"
	if dc, ok := drv.(driver.DriverContext); ok {
		return dc.OpenConnector(dsn)
	}
	return &dsnConnector{drv: drv, dsn: dsn}, nil
}

// migrationStatements sono le singole DDL dello schema attuale, ID-based e
// senza alcuna dipendenza da percorsi testuali o dal filesystem:
//
//   - folders: id (UUID) / parent_id (UUID nullable, NON un path) / name /
//     updated_at / deleted_at. Spostare una cartella è un singolo
//     UPDATE ... SET parent_id = ? WHERE id = ?: nessun rename, nessuna
//     riscrittura di sottoalberi.
//   - notes: id (UUID) / folder_id (UUID nullable) / title / content
//     (il Markdown vive QUI, come colonna di testo: non più su disco) /
//     updated_at / deleted_at.
//
// deleted_at NULL = riga attiva; deleted_at valorizzato = tombstone (soft
// delete), propagato tra i dispositivi tramite la sync e infine rimosso
// fisicamente (hard delete) da purgeExpiredTombstones oltre la finestra di
// retention (vedi PurgeExpiredTombstones in notes_repo.go/folders_repo.go).
//
// A differenza dei driver SQLite "classici" (mattn/go-sqlite3, modernc.org/
// sqlite), il driver libSQL esegue una sola statement per chiamata
// Exec/Query: una singola stringa con più CREATE TABLE separati da ";"
// esegue silenziosamente solo la prima e ignora le altre, senza errore. Per
// questo lo schema è diviso in statement singoli, eseguiti in sequenza.
var migrationStatements = []string{
	`CREATE TABLE IF NOT EXISTS users (
		id            TEXT PRIMARY KEY,
		username      TEXT NOT NULL UNIQUE,
		password_hash TEXT NOT NULL,
		created_at    INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS folders (
		id         TEXT PRIMARY KEY,
		user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		name       TEXT NOT NULL,
		parent_id  TEXT,
		updated_at INTEGER NOT NULL,
		synced_at  INTEGER NOT NULL DEFAULT 0,
		deleted_at INTEGER
	)`,
	`CREATE INDEX IF NOT EXISTS idx_folders_user ON folders(user_id)`,
	`CREATE INDEX IF NOT EXISTS idx_folders_parent ON folders(user_id, parent_id)`,
	`CREATE INDEX IF NOT EXISTS idx_folders_updated ON folders(user_id, updated_at)`,
	// Indice parziale: PurgeExpiredTombstones (folders_repo.go) filtra
	// "WHERE deleted_at IS NOT NULL AND deleted_at < ?" senza user_id (è un
	// job di manutenzione globale, non per-utente). Senza questo indice
	// quella DELETE fa uno scan completo della tabella folders ad ogni
	// ciclo di purge (main.go, ogni 24h); essendo parziale (solo le righe
	// con deleted_at valorizzato, cioè i soli tombstone) resta piccolo e
	// economico da mantenere anche sulle scritture normali di righe attive.
	`CREATE INDEX IF NOT EXISTS idx_folders_deleted_at ON folders(deleted_at) WHERE deleted_at IS NOT NULL`,
	`CREATE TABLE IF NOT EXISTS notes (
		id          TEXT PRIMARY KEY,
		user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		title       TEXT NOT NULL DEFAULT '',
		content     TEXT NOT NULL DEFAULT '',
		folder_id   TEXT,
		is_favorite INTEGER NOT NULL DEFAULT 0,
		is_pinned   INTEGER NOT NULL DEFAULT 0,
		order_index INTEGER NOT NULL DEFAULT 0,
		updated_at  INTEGER NOT NULL,
		synced_at   INTEGER NOT NULL DEFAULT 0,
		deleted_at  INTEGER
	)`,
	`CREATE INDEX IF NOT EXISTS idx_notes_user ON notes(user_id)`,
	`CREATE INDEX IF NOT EXISTS idx_notes_folder ON notes(user_id, folder_id)`,
	`CREATE INDEX IF NOT EXISTS idx_notes_updated ON notes(user_id, updated_at)`,
	// Stessa motivazione di idx_folders_deleted_at, per NotesRepo.PurgeExpiredTombstones.
	`CREATE INDEX IF NOT EXISTS idx_notes_deleted_at ON notes(deleted_at) WHERE deleted_at IS NOT NULL`,
	`CREATE TABLE IF NOT EXISTS user_settings (
		user_id       TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
		settings_json TEXT NOT NULL,
		updated_at    INTEGER NOT NULL
	)`,
	// Revoca persistente dei token (sopravvive al riavvio del server).
	// Volutamente SENZA foreign key verso users: la revoca di un utente
	// cancellato deve restare valida anche dopo la DELETE della sua riga.
	//
	//  - user_token_revocations: "tutti i token emessi prima di revoked_at
	//    per questo utente sono invalidi" (reset password, cancellazione).
	//  - revoked_tokens: revoca del singolo token (logout esplicito),
	//    identificato dal claim jti, conservata fino alla scadenza naturale.
	`CREATE TABLE IF NOT EXISTS user_token_revocations (
		user_id    TEXT PRIMARY KEY,
		revoked_at INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS revoked_tokens (
		jti        TEXT PRIMARY KEY,
		user_id    TEXT NOT NULL,
		expires_at INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_revoked_tokens_exp ON revoked_tokens(expires_at)`,
}

func migrate(conn *sql.DB) error {
	// Un'installazione preesistente (prima di questa migrazione) ha una
	// tabella "notes" con lo schema legacy path-based (relative_path,
	// checksum, is_folder, senza folder_id/content/title). CREATE TABLE IF
	// NOT EXISTS non altera in alcun modo quella tabella già esistente: se la
	// rinominassimo dopo aver già creato la nuova "notes" fallirebbe, quindi
	// il controllo/rename va fatto PRIMA di eseguire migrationStatements.
	if err := quarantineLegacyNotesTable(conn); err != nil {
		return fmt.Errorf("quarantena schema legacy: %w", err)
	}

	for _, stmt := range migrationStatements {
		if _, err := conn.Exec(stmt); err != nil {
			return fmt.Errorf("statement %q: %w", stmt, err)
		}
	}

	// CREATE TABLE IF NOT EXISTS sopra non altera una tabella "notes" già
	// esistente nel nuovo schema ID-based ma creata PRIMA dell'introduzione
	// di is_favorite/is_pinned/order_index: su un'installazione del genere
	// queste colonne mancherebbero silenziosamente, e ogni UPSERT/SELECT che
	// le referenzia fallirebbe. addNotesPinningColumns le aggiunge in modo
	// additivo (ALTER TABLE ADD COLUMN, idempotente) quando risultano assenti.
	if err := addNotesPinningColumns(conn); err != nil {
		return fmt.Errorf("migrazione colonne pin/favorite: %w", err)
	}

	// synced_at: timestamp SERVER monotono, unico cursore di pull (vedi
	// addSyncedAtColumns).
	if err := addSyncedAtColumns(conn); err != nil {
		return fmt.Errorf("migrazione colonne synced_at: %w", err)
	}

	// Unicità username case-insensitive (best effort, vedi funzione).
	ensureUsernameUniqueNoCase(conn)

	// Vincola a livello di schema che folders.parent_id e notes.folder_id
	// possano riferire SOLO una cartella dello stesso user_id (mai di un
	// altro utente): vedi addOwnershipTriggers per i dettagli e per il
	// motivo per cui NON usiamo una vera FOREIGN KEY per questo.
	if err := addOwnershipTriggers(conn); err != nil {
		return fmt.Errorf("migrazione trigger di ownership: %w", err)
	}
	return nil
}

// OwnershipViolationMarker è il prefisso del messaggio che i trigger sotto
// sollevano con RAISE(ABORT, ...) quando parent_id/folder_id non appartiene
// allo user_id della riga. Esportata perché internal/handlers/api_sync.go
// la riconosce per scartare il singolo elemento del batch invece di far
// fallire l'intera sync (vedi isOwnershipViolation).
const OwnershipViolationMarker = "OWNERSHIP_VIOLATION"

// addOwnershipTriggers impone, con quattro trigger SQLite, che
// folders.parent_id e notes.folder_id possano riferire SOLO una cartella
// dello STESSO user_id della riga che li imposta: è la difesa DB-level
// contro un client che tenti di "agganciare" una propria nota/cartella a
// quella di un altro utente (IDOR), richiesta esplicitamente al posto di una
// SELECT applicativa preventiva in UpsertLWW — stesso identico costo (un
// solo round-trip: il controllo avviene DENTRO lo statement INSERT/UPDATE
// già in corso, valutato dal motore SQLite stesso).
//
// Deliberatamente TRIGGER e non una vera FOREIGN KEY composita (approccio
// tentato in un primo momento, poi scartato): una FK avrebbe richiesto di
// ricreare interamente le tabelle folders/notes (SQLite non supporta ALTER
// TABLE ADD CONSTRAINT), con due conseguenze inaccettabili su
// un'installazione già in produzione con dati reali:
//
//  1. PRAGMA foreign_key_check, eseguito sui dati esistenti prima del
//     commit, avrebbe bloccato l'AVVIO DELL'INTERO SERVER se anche una sola
//     riga storica avesse un folder_id/parent_id ormai "orfano" — scenario
//     tutt'altro che raro: PurgeExpiredTombstones cancella fisicamente le
//     cartelle tombstoned da più tempo della retention SENZA propagare la
//     cancellazione a eventuali riferimenti rimasti (dispositivi offline che
//     risincronizzano tardi una nota creata prima della cancellazione della
//     sua cartella, per esempio). Con una FK, un singolo caso del genere
//     avrebbe reso l'intero backend inutilizzabile fino a un intervento
//     manuale sul database.
//  2. "ON DELETE CASCADE"/"ON DELETE SET NULL" su una FK composita che
//     include user_id nella chiave figlia (folder_id, user_id) si applica a
//     TUTTE le colonne della chiave: CASCADE avrebbe fatto sì che il purge
//     automatico di una vecchia cartella tombstoned cancellasse a sua volta,
//     in modo silenzioso, ogni nota ancora "agganciata" a quell'id — anche
//     note ATTIVE, mai cancellate dall'utente. SET NULL avrebbe invece
//     tentato di azzerare anche user_id (parte della stessa chiave
//     composita), violando il suo vincolo NOT NULL. Entrambe le opzioni
//     rischiavano perdita di dati reali dell'utente.
//
// I trigger evitano interamente questi due problemi: si attivano SOLO sulle
// scritture che avvengono da questo momento in poi (mai sui dati storici già
// presenti, quindi nessun rischio per l'avvio), e non definiscono alcun
// comportamento ON DELETE: la cancellazione/il purge di una cartella
// continuano a funzionare esattamente come prima, senza alcuna propagazione
// automatica.
func addOwnershipTriggers(conn *sql.DB) error {
	statements := []string{
		`CREATE TRIGGER IF NOT EXISTS trg_folders_parent_ownership_ins
		 BEFORE INSERT ON folders
		 WHEN NEW.parent_id IS NOT NULL
		 BEGIN
		   SELECT CASE WHEN NOT EXISTS (
		     SELECT 1 FROM folders WHERE id = NEW.parent_id AND user_id = NEW.user_id
		   ) THEN RAISE(ABORT, 'OWNERSHIP_VIOLATION: parent_id non valido o non appartenente allo stesso utente') END;
		 END`,
		`CREATE TRIGGER IF NOT EXISTS trg_folders_parent_ownership_upd
		 BEFORE UPDATE OF parent_id ON folders
		 WHEN NEW.parent_id IS NOT NULL
		 BEGIN
		   SELECT CASE WHEN NOT EXISTS (
		     SELECT 1 FROM folders WHERE id = NEW.parent_id AND user_id = NEW.user_id
		   ) THEN RAISE(ABORT, 'OWNERSHIP_VIOLATION: parent_id non valido o non appartenente allo stesso utente') END;
		 END`,
		`CREATE TRIGGER IF NOT EXISTS trg_notes_folder_ownership_ins
		 BEFORE INSERT ON notes
		 WHEN NEW.folder_id IS NOT NULL
		 BEGIN
		   SELECT CASE WHEN NOT EXISTS (
		     SELECT 1 FROM folders WHERE id = NEW.folder_id AND user_id = NEW.user_id
		   ) THEN RAISE(ABORT, 'OWNERSHIP_VIOLATION: folder_id non valido o non appartenente allo stesso utente') END;
		 END`,
		`CREATE TRIGGER IF NOT EXISTS trg_notes_folder_ownership_upd
		 BEFORE UPDATE OF folder_id ON notes
		 WHEN NEW.folder_id IS NOT NULL
		 BEGIN
		   SELECT CASE WHEN NOT EXISTS (
		     SELECT 1 FROM folders WHERE id = NEW.folder_id AND user_id = NEW.user_id
		   ) THEN RAISE(ABORT, 'OWNERSHIP_VIOLATION: folder_id non valido o non appartenente allo stesso utente') END;
		 END`,
	}
	for _, stmt := range statements {
		if _, err := conn.Exec(stmt); err != nil {
			return fmt.Errorf("statement %q: %w", stmt, err)
		}
	}
	return nil
}

// addNotesPinningColumns aggiunge is_favorite, is_pinned e order_index alla
// tabella "notes" se non sono già presenti. È sicuro chiamarla ad ogni avvio
// (idempotente): interroga PRAGMA table_info prima di ogni ALTER TABLE e
// salta le colonne già esistenti, esattamente come quarantineLegacyNotesTable
// fa per il rilevamento dello schema legacy.
func addNotesPinningColumns(conn *sql.DB) error {
	rows, err := conn.Query(`PRAGMA table_info(notes)`)
	if err != nil {
		return fmt.Errorf("pragma table_info(notes): %w", err)
	}

	existing := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return fmt.Errorf("scan table_info(notes): %w", err)
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterazione table_info(notes): %w", err)
	}
	rows.Close()

	wanted := []struct {
		column string
		ddl    string
	}{
		{"is_favorite", `ALTER TABLE notes ADD COLUMN is_favorite INTEGER NOT NULL DEFAULT 0`},
		{"is_pinned", `ALTER TABLE notes ADD COLUMN is_pinned INTEGER NOT NULL DEFAULT 0`},
		{"order_index", `ALTER TABLE notes ADD COLUMN order_index INTEGER NOT NULL DEFAULT 0`},
	}
	for _, w := range wanted {
		if existing[w.column] {
			continue
		}
		if _, err := conn.Exec(w.ddl); err != nil {
			return fmt.Errorf("alter table %q: %w", w.column, err)
		}
		log.Printf("migrazione: aggiunta colonna notes.%s mancante (default 0)", w.column)
	}
	return nil
}

// addSyncedAtColumns aggiunge folders.synced_at / notes.synced_at.
//
// PERCHÉ: il cursore di pull era confrontato con updated_at, cioè con
// l'orologio del CLIENT che ha scritto la riga. Con clock skew (o con un
// dispositivo rimasto offline) una modifica pushata DOPO l'ultima pull di un
// altro dispositivo poteva avere updated_at < cursore di quest'ultimo e non
// venirgli mai consegnata. synced_at è invece assegnato dal SERVER (monotono,
// vedi handlers.SyncHandler) al momento dell'accettazione: il pull filtra
// solo su di esso, mentre updated_at resta usato esclusivamente per la
// risoluzione LWW.
//
// Le righe preesistenti ricevono synced_at = adesso: ogni dispositivo le
// riscaricherà una volta sola (idempotente) e ne risulta riparato anche
// qualsiasi aggiornamento perso in passato per skew.
func addSyncedAtColumns(conn *sql.DB) error {
	for _, table := range []string{"folders", "notes"} {
		cols, err := tableColumns(conn, table)
		if err != nil {
			return err
		}
		if !cols["synced_at"] {
			if _, err := conn.Exec(`ALTER TABLE ` + table + ` ADD COLUMN synced_at INTEGER NOT NULL DEFAULT 0`); err != nil {
				return fmt.Errorf("alter table %s: %w", table, err)
			}
			if _, err := conn.Exec(`UPDATE `+table+` SET synced_at = ? WHERE synced_at = 0`, time.Now().UnixMilli()); err != nil {
				return fmt.Errorf("backfill %s.synced_at: %w", table, err)
			}
			log.Printf("migrazione: aggiunta colonna %s.synced_at (backfill = ora corrente del server)", table)
		}
		if _, err := conn.Exec(`CREATE INDEX IF NOT EXISTS idx_` + table + `_synced ON ` + table + `(user_id, synced_at)`); err != nil {
			return fmt.Errorf("index %s.synced_at: %w", table, err)
		}
	}
	return nil
}

// tableColumns restituisce l'insieme dei nomi colonna di una tabella.
func tableColumns(conn *sql.DB, table string) (map[string]bool, error) {
	rows, err := conn.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return nil, fmt.Errorf("pragma table_info(%s): %w", table, err)
	}
	defer rows.Close()
	cols := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return nil, fmt.Errorf("scan table_info(%s): %w", table, err)
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

// ensureUsernameUniqueNoCase impone l'unicità degli username ignorando
// maiuscole/minuscole ("Mario" e "mario" sono lo stesso utente). Best
// effort: se nel DB esistono già username che collidono case-insensitive
// l'indice non può essere creato; il server NON si blocca (i login
// continuano a funzionare, la creazione applicativa controlla comunque i
// duplicati) ma viene loggato un avviso per la correzione manuale.
func ensureUsernameUniqueNoCase(conn *sql.DB) {
	if _, err := conn.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_username_nocase ON users(username COLLATE NOCASE)`); err != nil {
		log.Printf("ATTENZIONE: impossibile creare l'indice univoco case-insensitive sugli username (%v). Esistono probabilmente utenti con username uguali a meno delle maiuscole: risolverli manualmente.", err)
	}
}

// quarantineLegacyNotesTable rileva lo schema "notes" della generazione
// precedente (path-based: colonna relative_path, senza folder_id) e lo mette
// da parte rinominandolo in "notes_legacy_backup" invece di cancellarlo: i
// dati storici restano ispezionabili/esportabili manualmente da un
// amministratore, ma non interferiscono con il nuovo schema ID-based (che
// userebbe altrimenti lo stesso nome "notes" con colonne incompatibili).
// Su un'installazione nuova (colonna relative_path assente o tabella
// inesistente) questa funzione è un no-op.
func quarantineLegacyNotesTable(conn *sql.DB) error {
	rows, err := conn.Query(`PRAGMA table_info(notes)`)
	if err != nil {
		return fmt.Errorf("pragma table_info(notes): %w", err)
	}

	hasRelativePath := false
	hasFolderID := false
	tableExists := false
	for rows.Next() {
		tableExists = true
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return fmt.Errorf("scan table_info(notes): %w", err)
		}
		if name == "relative_path" {
			hasRelativePath = true
		}
		if name == "folder_id" {
			hasFolderID = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterazione table_info(notes): %w", err)
	}
	rows.Close()

	if !tableExists || !hasRelativePath || hasFolderID {
		// Tabella assente (installazione nuova), oppure già nel nuovo
		// schema (hasFolderID): nulla da fare.
		return nil
	}

	if _, err := conn.Exec(`ALTER TABLE notes RENAME TO notes_legacy_backup`); err != nil {
		return fmt.Errorf("rename notes -> notes_legacy_backup: %w", err)
	}
	log.Println("migrazione: rilevato schema 'notes' legacy path-based, rinominato in 'notes_legacy_backup' (dati preservati, non più in uso). Il nuovo schema ID-based (folders/notes) viene creato da zero.")
	return nil
}
