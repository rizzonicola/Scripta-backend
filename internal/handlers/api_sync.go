package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"time"

	"notes-server/internal/db"
	"notes-server/internal/middleware"
	"notes-server/internal/models"
	"notes-server/internal/storage"
)

// defaultMaxSyncBodyBytes è il tetto di default sulla dimensione del body di
// una richiesta di sync: un batch può legittimamente contenere molte note,
// quindi il limite è ben più alto di login/settings, ma resta comunque
// finito per evitare che un singolo payload possa esaurire la RAM del
// processo. Sovrascrivibile per-istanza (vedi NewSyncHandler) tramite la
// variabile d'ambiente SYNC_MAX_BODY_BYTES, senza dover ricompilare.
const defaultMaxSyncBodyBytes = 64 << 20 // 64 MiB

// maxBeginTxRetries e beginTxBackoff regolano il retry di BeginTx quando il
// database segnala una contesa transitoria (SQLITE_BUSY / "database is
// locked"). È sicuro ritentare BeginTx perché, a differenza di un Commit,
// non ha ancora effettuato alcuna scrittura: un retry non rischia mai di
// duplicare né di corrompere alcuno stato.
const maxBeginTxRetries = 3

var beginTxBackoff = [maxBeginTxRetries]time.Duration{20 * time.Millisecond, 60 * time.Millisecond, 150 * time.Millisecond}

type SyncHandler struct {
	Notes *db.NotesRepo
	Store *storage.Store

	// MaxBodyBytes limita la dimensione del body accettato da Sync tramite
	// http.MaxBytesReader. Se zero, NewSyncHandler applica il default.
	MaxBodyBytes int64
}

func NewSyncHandler(notes *db.NotesRepo, store *storage.Store) *SyncHandler {
	return &SyncHandler{Notes: notes, Store: store, MaxBodyBytes: defaultMaxSyncBodyBytes}
}

// isTransientBusyErr riconosce gli errori di contesa transitoria di SQLite/
// libSQL (SQLITE_BUSY, "database is locked", "database table is locked").
// In modalità locale pura (MaxOpenConns=1, vedi internal/db/db.go) questo
// caso è di fatto irraggiungibile: il pool ha una sola connessione fisica,
// quindi database/sql serializza già le richieste concorrenti mettendole in
// coda per quella connessione, senza mai generare SQLITE_BUSY. Diventa
// invece un caso reale in modalità embedded replica con più connessioni
// verso lo stesso primario remoto (es. più istanze del server dietro un load
// balancer che scrivono sullo stesso database Turso): è lì che questo retry
// ha effetto.
func isTransientBusyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "sqlite_busy")
}

// Sync gestisce POST /api/v1/sync.
//
// Per ogni voce inviata dal client mobile (nota o cartella):
//   - se OldRelativePath è valorizzato e diverso da RelativePath, si tratta
//     di uno spostamento/rinomina: il server sposta fisicamente il file o la
//     cartella sul disco (rename) e ne aggiorna il path nei metadati, invece
//     di ricrearlo da zero al nuovo path (che perderebbe il contenuto se il
//     client non lo rinvia per intero durante un semplice move).
//   - altrimenti, se non esiste sul server, oppure il client è più recente o
//     uguale (updated_at client >= updated_at server) -> vince il client:
//     il file/cartella viene scritto/creato (o cancellato) in modo atomico e
//     i metadati aggiornati. La voce entra nella lista "accepted".
//   - se il server ha una versione più recente -> vince il server: la voce
//     (con contenuto letto da disco, se non è una cartella) entra nella
//     lista "server_wins" così il client può aggiornare la propria copia locale.
//
// L'intero batch di aggiornamenti ai metadati viene eseguito dentro un'unica
// transazione esplicita: meno round-trip al DB, un solo fsync a fine batch
// (con synchronous=NORMAL + WAL) e atomicità sui metadati dell'intera
// richiesta di sync. Per ogni voce, la sequenza "leggi stato attuale -> decidi
// il vincitore -> scrivi file -> aggiorna metadati" è racchiusa in un lock
// per-path (o per coppia di path, per gli spostamenti), per evitare race
// condition quando lo stesso utente sincronizza da più dispositivi in
// parallelo sullo stesso file/cartella.
func (h *SyncHandler) Sync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "metodo non consentito")
		return
	}

	ctx := r.Context()

	userID, ok := middleware.UserIDFromContext(ctx)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "utente non autenticato")
		return
	}

	maxBody := h.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = defaultMaxSyncBodyBytes
	}
	var req models.SyncRequest
	if !decodeJSONBody(w, r, &req, maxBody) {
		return
	}

	resp := models.SyncResponse{
		Accepted:   []models.NoteResult{},
		ServerWins: []models.NoteResult{},
	}

	tx, notesTx, err := h.beginTxWithRetry(ctx)
	if err != nil {
		if isTransientBusyErr(err) {
			// Il database resta momentaneamente occupato nonostante i
			// tentativi: chiediamo al client di ritentare l'intero batch più
			// tardi, invece di far fallire la richiesta con un generico 500.
			// È sicuro perché non è stata effettuata alcuna scrittura: la
			// sync è idempotente rispetto a un retry completo (la
			// risoluzione LWW per relative_path/updated_at dà lo stesso
			// esito qualunque sia il numero di volte in cui lo stesso batch
			// viene rinviato).
			w.Header().Set("Retry-After", "1")
			writeJSONError(w, http.StatusServiceUnavailable, "database temporaneamente occupato, riprovare")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "errore avvio transazione")
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	for _, change := range req.Notes {
		if err := ctx.Err(); err != nil {
			// client disconnesso o richiesta scaduta: interrompiamo senza
			// scrivere risposta (la connessione non è più utile) e il defer
			// sopra farà rollback della transazione.
			return
		}

		if change.RelativePath == "" {
			continue
		}

		fullPath, err := h.Store.ResolvePath(userID, change.RelativePath)
		if err != nil {
			// path non valido/traversal: salta la nota, non interrompe l'intera sync
			continue
		}

		if err := h.syncOne(ctx, notesTx, userID, fullPath, change, &resp); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "errore elaborazione nota")
			return
		}
	}

	if err := tx.Commit(); err != nil {
		if isTransientBusyErr(err) {
			// NB: qui NON ritentiamo tx.Commit() sullo stesso *sql.Tx. Con
			// database/sql, una volta che Commit ha restituito un errore
			// l'oggetto Tx è considerato "concluso" a livello applicativo
			// (ExecContext/QueryContext successivi restituirebbero
			// sql.ErrTxDone): un secondo tentativo sullo stesso Tx non è
			// un vero retry lato SQLite, è solo un errore diverso. L'unico
			// retry corretto sarebbe ripetere l'intera transazione da capo
			// (BeginTx + tutte le syncOne), che qui scarteremmo perché
			// aumenterebbe la latenza percepita in modo imprevedibile sotto
			// contesa. Deleghiamo quindi il retry al client, che è già
			// pensato per rimandare periodicamente lo stesso batch (sync
			// idempotente), con lo stesso status/headers usati sopra per
			// BeginTx.
			w.Header().Set("Retry-After", "1")
			writeJSONError(w, http.StatusServiceUnavailable, "database temporaneamente occupato, riprovare")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "errore salvataggio sync")
		return
	}
	committed = true

	writeJSON(w, http.StatusOK, resp)
}

// sqlTx è il sottoinsieme di *sql.Tx che ci serve qui: isolarlo in
// un'interfaccia evita di esporre l'intero *sql.Tx nella firma di
// beginTxWithRetry e rende esplicito che, dopo il BeginTx, la trattiamo solo
// come "qualcosa che si può commitare o annullare".
type sqlTx interface {
	Commit() error
	Rollback() error
}

// beginTxWithRetry avvia una transazione ritentando, con backoff crescente,
// solo in caso di contesa transitoria del database (vedi isTransientBusyErr).
// È sicuro ritentare qui perché nessuna scrittura è ancora avvenuta: al
// contrario di un Commit fallito, un BeginTx fallito non lascia alcuno stato
// a metà da riconciliare.
func (h *SyncHandler) beginTxWithRetry(ctx context.Context) (sqlTx, *db.NotesRepo, error) {
	var lastErr error
	for attempt := 0; attempt <= maxBeginTxRetries; attempt++ {
		tx, notesTx, err := h.Notes.BeginTx(ctx)
		if err == nil {
			return tx, notesTx, nil
		}
		lastErr = err
		if !isTransientBusyErr(err) || attempt == maxBeginTxRetries {
			break
		}
		backoff := beginTxBackoff[attempt]
		// Piccolo jitter per evitare che più richieste in contesa si
		// risveglino tutte nello stesso istante e si ripresentino di nuovo
		// in blocco ("thundering herd").
		backoff += time.Duration(rand.Int63n(int64(backoff) / 2))
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(backoff):
		}
	}
	return nil, nil, lastErr
}

// syncOne smista ciascuna voce del batch di sync verso il gestore corretto:
// spostamento (se OldRelativePath è presente e diverso da RelativePath),
// cartella, o file .md ordinario. fullPath è il percorso su disco già
// risolto/validato per change.RelativePath (la destinazione, nel caso di uno
// spostamento).
func (h *SyncHandler) syncOne(ctx context.Context, notesTx *db.NotesRepo, userID, fullPath string, change models.NoteChange, resp *models.SyncResponse) error {
	isMove := change.OldRelativePath != "" && change.OldRelativePath != change.RelativePath

	switch {
	case isMove:
		return h.syncMove(ctx, notesTx, userID, fullPath, change, resp)
	case change.IsFolder:
		return h.syncFolder(ctx, notesTx, userID, fullPath, change, resp)
	default:
		return h.syncFile(ctx, notesTx, userID, fullPath, change, resp)
	}
}

// syncFile applica la risoluzione dei conflitti last-write-wins per un
// singolo file .md, sotto il lock per-path dello Store: la sequenza lettura-
// stato -> scrittura-file -> upsert-metadati è così atomica rispetto ad altre
// sync concorrenti sullo stesso file. Comportamento invariato rispetto alla
// logica storica del server (prima dell'introduzione di cartelle e move
// espliciti): un client che invia solo relative_path/content/updated_at/
// deleted, senza mai valorizzare old_relative_path o is_folder, passa sempre
// da qui.
func (h *SyncHandler) syncFile(ctx context.Context, notesTx *db.NotesRepo, userID, fullPath string, change models.NoteChange, resp *models.SyncResponse) error {
	// Lock condiviso a livello di utente: esclude solo un eventuale
	// spostamento/cancellazione utente in corso (vedi Store.RLockUser),
	// permettendo comunque piena concorrenza con altre operazioni puntuali
	// sullo stesso utente.
	unlockUser := h.Store.RLockUser(userID)
	defer unlockUser()

	unlock := h.Store.LockPath(fullPath)
	defer unlock()

	existing, err := notesTx.Get(ctx, userID, change.RelativePath)
	if err != nil {
		return err
	}

	clientWins := existing == nil || change.UpdatedAt >= existing.UpdatedAt

	if clientWins {
		var checksum string
		if change.Deleted {
			if err := h.Store.Delete(fullPath); err != nil {
				return err
			}
		} else {
			if err := h.Store.AtomicWrite(fullPath, []byte(change.Content)); err != nil {
				return err
			}
			checksum = sha256Hex(change.Content)
		}

		note := &models.Note{
			UserID:       userID,
			RelativePath: change.RelativePath,
			UpdatedAt:    change.UpdatedAt,
			Deleted:      change.Deleted,
			Checksum:     checksum,
		}
		if existing != nil {
			note.ID = existing.ID
		}
		if err := notesTx.Upsert(ctx, note); err != nil {
			return err
		}

		resp.Accepted = append(resp.Accepted, models.NoteResult{
			RelativePath: change.RelativePath,
			UpdatedAt:    change.UpdatedAt,
			Deleted:      change.Deleted,
		})
		return nil
	}

	// Il server ha una versione più recente: la restituiamo al client.
	result := models.NoteResult{
		RelativePath: existing.RelativePath,
		UpdatedAt:    existing.UpdatedAt,
		Deleted:      existing.Deleted,
	}
	if !existing.Deleted {
		content, err := h.Store.ReadFile(fullPath)
		if err == nil {
			result.Content = string(content)
		}
	}
	resp.ServerWins = append(resp.ServerWins, result)
	return nil
}

// syncFolder gestisce la creazione/cancellazione di una cartella (anche
// vuota, cioè senza alcun file .md al suo interno). A differenza di un file,
// una cartella vuota non lascia alcuna traccia sul filesystem finché non
// viene creata esplicitamente con EnsureFolder: è questo il bug che
// syncFolder risolve (prima, una cartella senza note non veniva mai
// persistita su disco). La stessa risoluzione dei conflitti LWW usata per i
// file si applica anche qui, così due device che creano/cancellano la stessa
// cartella non vanno in conflitto in modo incoerente.
func (h *SyncHandler) syncFolder(ctx context.Context, notesTx *db.NotesRepo, userID, fullPath string, change models.NoteChange, resp *models.SyncResponse) error {
	unlockUser := h.Store.RLockUser(userID)
	defer unlockUser()

	unlock := h.Store.LockPath(fullPath)
	defer unlock()

	existing, err := notesTx.Get(ctx, userID, change.RelativePath)
	if err != nil {
		return err
	}

	clientWins := existing == nil || change.UpdatedAt >= existing.UpdatedAt
	if !clientWins {
		resp.ServerWins = append(resp.ServerWins, models.NoteResult{
			RelativePath: existing.RelativePath,
			UpdatedAt:    existing.UpdatedAt,
			Deleted:      existing.Deleted,
			IsFolder:     true,
		})
		return nil
	}

	if change.Deleted {
		if err := h.Store.DeleteFolder(fullPath); err != nil {
			return err
		}
	} else {
		if err := h.Store.EnsureFolder(fullPath); err != nil {
			return err
		}
	}

	note := &models.Note{
		UserID:       userID,
		RelativePath: change.RelativePath,
		UpdatedAt:    change.UpdatedAt,
		Deleted:      change.Deleted,
		IsFolder:     true,
	}
	if existing != nil {
		note.ID = existing.ID
	}
	if err := notesTx.Upsert(ctx, note); err != nil {
		return err
	}

	resp.Accepted = append(resp.Accepted, models.NoteResult{
		RelativePath: change.RelativePath,
		UpdatedAt:    change.UpdatedAt,
		Deleted:      change.Deleted,
		IsFolder:     true,
	})
	return nil
}

// syncMove gestisce lo spostamento/rinomina di una nota o di una cartella da
// change.OldRelativePath a change.RelativePath (fullPath è già il percorso
// risolto per quest'ultimo). Questo è il fix del bug di "nota non spostata
// correttamente tra cartelle": invece di affidarsi al client per cancellare
// il vecchio path e ricreare da zero il contenuto al nuovo path (fragile, e
// vulnerabile a perdita di contenuto se il campo "content" non viene rinviato
// per intero in un move puro), il server sposta fisicamente il file/cartella
// con una rename sul filesystem, che è atomica e preserva esattamente i byte
// esistenti, poi allinea i metadati nel DB.
func (h *SyncHandler) syncMove(ctx context.Context, notesTx *db.NotesRepo, userID, fullPath string, change models.NoteChange, resp *models.SyncResponse) error {
	oldFullPath, err := h.Store.ResolvePath(userID, change.OldRelativePath)
	if err != nil {
		// Percorso di origine non valido/traversal: non possiamo spostare
		// nulla, ma trattiamo comunque la voce come una creazione ordinaria
		// alla destinazione, così la sync non perde silenziosamente il dato.
		if change.IsFolder {
			return h.syncFolder(ctx, notesTx, userID, fullPath, change, resp)
		}
		return h.syncFile(ctx, notesTx, userID, fullPath, change, resp)
	}

	newFullPath := fullPath

	// Lock ESCLUSIVO a livello di utente: uno spostamento tocca due path
	// contemporaneamente (origine e destinazione, potenzialmente un intero
	// sottoalbero se è una cartella) e deve quindi escludere qualunque altra
	// operazione sullo stesso utente finché non è terminato, non solo quelle
	// sui due path espliciti — altrimenti un file "figlio" della cartella
	// spostata, sincronizzato individualmente in parallelo con un lock solo
	// sul proprio path, potrebbe correre con l'os.Rename della cartella
	// padre (vedi Store.LockUser per il dettaglio del problema).
	unlockUser := h.Store.LockUser(userID)
	defer unlockUser()

	// Lock sui due path espliciti, in ordine deterministico: ridondante con
	// il lock utente esclusivo qui sopra ai fini della sicurezza (che già
	// esclude tutto), ma lo manteniamo per coerenza con syncFile/syncFolder
	// e per restare corretti anche se in futuro LockUser venisse allentato a
	// un lock più granulare.
	unlock := h.Store.LockPaths(oldFullPath, newFullPath)
	defer unlock()

	oldNote, err := notesTx.Get(ctx, userID, change.OldRelativePath)
	if err != nil {
		return err
	}
	targetNote, err := notesTx.Get(ctx, userID, change.RelativePath)
	if err != nil {
		return err
	}

	// LWW sul path di destinazione: se il server ha già lì una versione più
	// recente di quella che il client sta spostando, vince il server. Il
	// vecchio path NON viene toccato in questo caso: è compito di un
	// successivo giro di sync (con updated_at coerente) riconciliare la
	// situazione, evitando di cancellare dati sulla sola base di un conflitto
	// rilevato a metà spostamento.
	if targetNote != nil && targetNote.UpdatedAt > change.UpdatedAt {
		result := models.NoteResult{
			RelativePath: targetNote.RelativePath,
			UpdatedAt:    targetNote.UpdatedAt,
			Deleted:      targetNote.Deleted,
			IsFolder:     targetNote.IsFolder,
		}
		if !targetNote.IsFolder && !targetNote.Deleted {
			if content, err := h.Store.ReadFile(newFullPath); err == nil {
				result.Content = string(content)
			}
		}
		resp.ServerWins = append(resp.ServerWins, result)
		return nil
	}

	if err := h.Store.MovePath(oldFullPath, newFullPath); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		// La sorgente non esiste più su disco (già spostata in precedenza, o
		// mai creata prima del fix del bug delle cartelle vuote): ripieghiamo
		// su una creazione ordinaria alla destinazione, usando i dati forniti
		// dal client, invece di far fallire l'intera sync.
		if !change.Deleted {
			if change.IsFolder {
				if err := h.Store.EnsureFolder(newFullPath); err != nil {
					return err
				}
			} else {
				if err := h.Store.AtomicWrite(newFullPath, []byte(change.Content)); err != nil {
					return err
				}
			}
		}
	}

	if change.Deleted {
		if change.IsFolder {
			if err := h.Store.DeleteFolder(newFullPath); err != nil {
				return err
			}
		} else {
			if err := h.Store.Delete(newFullPath); err != nil {
				return err
			}
		}
	}

	checksum := ""
	if !change.IsFolder && !change.Deleted {
		// HashFile legge il file in streaming e calcola l'hash senza
		// caricarne l'intero contenuto in una slice separata: qui serve
		// solo il checksum, non il contenuto, quindi ReadFile+sha256Hex
		// sprecherebbe una copia completa del file (ReadFile alloca il
		// buffer, poi la conversione a string in sha256Hex ne alloca
		// un'altra) per un dato che scartiamo subito dopo.
		if sum, err := h.Store.HashFile(newFullPath); err == nil {
			checksum = sum
		}
	}

	// Rimuoviamo PRIMA la vecchia riga di metadati (se esisteva): il file/
	// cartella è già stato fisicamente spostato, quindi quella riga
	// punterebbe ormai a un path inesistente (hard delete, non tombstone: non
	// è una cancellazione logica della nota, solo la ripulitura del vecchio
	// indirizzo). Va fatto PRIMA dell'Upsert sulla nuova riga perché, quando
	// riutilizziamo lo stesso ID per preservare l'identità della nota
	// attraverso lo spostamento, inserire la nuova riga mentre la vecchia
	// (con lo stesso ID, ma relative_path diverso) esiste ancora violerebbe
	// il vincolo di chiave primaria su id.
	if oldNote != nil {
		if err := notesTx.Delete(ctx, userID, change.OldRelativePath); err != nil {
			return err
		}
	}

	note := &models.Note{
		UserID:       userID,
		RelativePath: change.RelativePath,
		UpdatedAt:    change.UpdatedAt,
		Deleted:      change.Deleted,
		Checksum:     checksum,
		IsFolder:     change.IsFolder,
	}
	switch {
	case targetNote != nil:
		note.ID = targetNote.ID
	case oldNote != nil:
		note.ID = oldNote.ID // preserva l'identità della nota/cartella attraverso lo spostamento
	}
	if err := notesTx.Upsert(ctx, note); err != nil {
		return err
	}

	resp.Accepted = append(resp.Accepted, models.NoteResult{
		RelativePath: change.RelativePath,
		UpdatedAt:    change.UpdatedAt,
		Deleted:      change.Deleted,
		IsFolder:     change.IsFolder,
	})
	return nil
}

func sha256Hex(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}
