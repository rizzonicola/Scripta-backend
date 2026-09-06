package handlers

import (
	"context"
	"database/sql"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"notes-server/internal/db"
	"notes-server/internal/middleware"
	"notes-server/internal/models"
)

// defaultMaxSyncBodyBytes è il tetto di default sulla dimensione del body di
// una richiesta di sync: un batch può legittimamente contenere molte note
// (il cui contenuto ora viaggia per intero nel payload, non più solo i
// metadati), quindi il limite resta ben più alto di login/settings, ma
// finito per evitare che un singolo payload possa esaurire la RAM del
// processo. Sovrascrivibile per-istanza tramite SYNC_MAX_BODY_BYTES.
const defaultMaxSyncBodyBytes = 64 << 20 // 64 MiB

// maxBeginTxRetries e beginTxBackoff regolano il retry di BeginTx quando il
// database segnala una contesa transitoria (SQLITE_BUSY / "database is
// locked"). È sicuro ritentare BeginTx perché, a differenza di un Commit,
// non ha ancora effettuato alcuna scrittura.
const maxBeginTxRetries = 3

var beginTxBackoff = [maxBeginTxRetries]time.Duration{20 * time.Millisecond, 60 * time.Millisecond, 150 * time.Millisecond}

// tombstoneRetention è per quanto tempo un tombstone (deleted_at valorizzato)
// resta visibile tramite la pull della sync prima di poter essere rimosso
// definitivamente (vedi PurgeExpiredTombstones, invocata periodicamente da
// main.go). Deve essere più larga del più lungo intervallo plausibile tra
// due sync consecutive di un dispositivo che l'utente comunque continua ad
// usare, così ogni device ha il tempo di ricevere ogni cancellazione prima
// che il server la dimentichi.
const TombstoneRetention = 30 * 24 * time.Hour

// SyncHandler gestisce POST /api/v1/sync: l'unico endpoint necessario per
// tenere sincronizzati local-first client e server, con risoluzione dei
// conflitti Last-Write-Wins interamente ID-based (nessun percorso testuale,
// nessun filesystem).
type SyncHandler struct {
	SQLDB   *sql.DB
	Folders *db.FoldersRepo // bound a SQLDB, usato per la query di pull dopo il commit
	Notes   *db.NotesRepo   // bound a SQLDB, usato per la query di pull dopo il commit

	// MaxBodyBytes limita la dimensione del body accettato da Sync tramite
	// http.MaxBytesReader. Se zero, NewSyncHandler applica il default.
	MaxBodyBytes int64
}

func NewSyncHandler(sqlDB *sql.DB) *SyncHandler {
	return &SyncHandler{
		SQLDB:        sqlDB,
		Folders:      db.NewFoldersRepo(sqlDB),
		Notes:        db.NewNotesRepo(sqlDB),
		MaxBodyBytes: defaultMaxSyncBodyBytes,
	}
}

// isTransientBusyErr riconosce gli errori di contesa transitoria di SQLite/
// libSQL (SQLITE_BUSY, "database is locked", "database table is locked").
func isTransientBusyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "sqlite_busy")
}

// Sync gestisce POST /api/v1/sync applicando il protocollo Delta Sync
// Push/Pull con risoluzione dei conflitti Last-Write-Wins:
//
//  1. PUSH: ogni cartella/nota inviata dal client viene applicata con un
//     UPSERT SQL condizionato (vedi FoldersRepo.UpsertLWW / NotesRepo.
//     UpsertLWW): l'aggiornamento ha effetto solo se updated_at del client è
//     >= di quello già memorizzato, altrimenti la riga resta quella (più
//     recente) del server. Nessun lock applicativo è necessario: la
//     condizione è valutata atomicamente dal motore SQL stesso.
//  2. CASCATA: per ogni cartella il cui stato risultante (dopo l'upsert) è
//     "cancellata", il server propaga ricorsivamente la cancellazione a
//     tutte le sottocartelle e note ancora attive al suo interno. Questo è
//     indipendente da cosa abbia effettivamente inviato il client: anche un
//     client "storico" che cancellasse solo la cartella radice otterrebbe
//     comunque una cascata coerente lato server.
//  3. PULL: il server restituisce tutte le cartelle e note dell'utente con
//     updated_at maggiore del cursore last_synced_at inviato dal client.
//     Poiché questa query viene eseguita DOPO aver applicato push e cascata,
//     include automaticamente sia le modifiche remote di altri dispositivi
//     sia l'esito (accettato o "server wins") di ciò che il client ha appena
//     inviato: non serve alcuna lista "accepted/server_wins" separata.
//
// L'intero push (incluse le cascate) avviene dentro un'unica transazione
// esplicita: un solo fsync a fine batch (con synchronous=NORMAL + WAL) e
// atomicità sui metadati dell'intera richiesta.
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

	// now è il "tempo server" di questa sync: diventa sia il timestamp usato
	// per le cancellazioni propagate in cascata, sia il nuovo cursore
	// last_synced_at che il client salverà per la prossima chiamata (evita
	// problemi di clock skew tra dispositivi diversi).
	now := time.Now().UnixMilli()

	tx, err := h.beginTxWithRetry(ctx)
	if err != nil {
		if isTransientBusyErr(err) {
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

	foldersTx := db.NewFoldersRepo(tx)
	notesTx := db.NewNotesRepo(tx)

	for _, fc := range req.Folders {
		if err := ctx.Err(); err != nil {
			return // client disconnesso: il defer sopra fa rollback
		}
		if fc.ID == "" {
			continue
		}
		name := strings.TrimSpace(fc.Name)
		if name == "" {
			name = "Senza nome"
		}
		folder := &models.Folder{
			ID:        fc.ID,
			UserID:    userID,
			Name:      name,
			ParentID:  fc.ParentID,
			UpdatedAt: fc.UpdatedAt,
			DeletedAt: fc.DeletedAt,
		}
		if err := foldersTx.UpsertLWW(ctx, folder); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "errore elaborazione cartella")
			return
		}

		// Rileggiamo lo stato risultante (non ciò che il client ha inviato):
		// se l'upsert è stato respinto dalla condizione LWW perché il server
		// aveva già una versione più recente E ATTIVA, la cartella non va
		// messa in cascata solo perché il client la voleva cancellare.
		current, err := foldersTx.Get(ctx, userID, fc.ID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "errore lettura cartella")
			return
		}
		if current != nil && current.DeletedAt != nil {
			if err := cascadeSoftDeleteFolder(ctx, foldersTx, notesTx, userID, fc.ID, now); err != nil {
				writeJSONError(w, http.StatusInternalServerError, "errore cascata cancellazione cartella")
				return
			}
		}
	}

	for _, nc := range req.Notes {
		if err := ctx.Err(); err != nil {
			return
		}
		if nc.ID == "" {
			continue
		}
		note := &models.Note{
			ID:        nc.ID,
			UserID:    userID,
			Title:     nc.Title,
			Content:   nc.Content,
			FolderID:  nc.FolderID,
			UpdatedAt: nc.UpdatedAt,
			DeletedAt: nc.DeletedAt,
		}
		if err := notesTx.UpsertLWW(ctx, note); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "errore elaborazione nota")
			return
		}
	}

	if err := tx.Commit(); err != nil {
		if isTransientBusyErr(err) {
			// NB: qui NON si ritenta tx.Commit() sullo stesso *sql.Tx (una
			// volta fallito è considerato concluso). Il retry corretto è
			// l'intero batch da capo, delegato al client: la sync è
			// idempotente rispetto a un rinvio completo, dato che la
			// risoluzione LWW per id/updated_at dà lo stesso esito qualunque
			// sia il numero di volte in cui lo stesso batch viene reinviato.
			w.Header().Set("Retry-After", "1")
			writeJSONError(w, http.StatusServiceUnavailable, "database temporaneamente occupato, riprovare")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "errore salvataggio sync")
		return
	}
	committed = true

	folders, err := h.Folders.ListUpdatedSince(ctx, userID, req.LastSyncedAt)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "errore lettura cartelle aggiornate")
		return
	}
	notes, err := h.Notes.ListUpdatedSince(ctx, userID, req.LastSyncedAt)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "errore lettura note aggiornate")
		return
	}

	resp := models.SyncResponse{
		ServerTime: now,
		Folders:    make([]models.FolderDTO, 0, len(folders)),
		Notes:      make([]models.NoteDTO, 0, len(notes)),
	}
	for _, f := range folders {
		resp.Folders = append(resp.Folders, models.FolderDTO{
			ID:        f.ID,
			Name:      f.Name,
			ParentID:  f.ParentID,
			UpdatedAt: f.UpdatedAt,
			DeletedAt: f.DeletedAt,
		})
	}
	for _, n := range notes {
		resp.Notes = append(resp.Notes, models.NoteDTO{
			ID:        n.ID,
			Title:     n.Title,
			Content:   n.Content,
			FolderID:  n.FolderID,
			UpdatedAt: n.UpdatedAt,
			DeletedAt: n.DeletedAt,
		})
	}

	writeJSON(w, http.StatusOK, resp)
}

// cascadeSoftDeleteFolder propaga ricorsivamente (in ampiezza, senza
// ricorsione nativa per evitare stack overflow su alberi patologicamente
// profondi) la cancellazione di una cartella a tutte le sottocartelle e note
// ancora attive al suo interno, usando lo stesso timestamp "now" per tutte le
// entità toccate. Usa ForceSet (non UpsertLWW) perché la cancellazione del
// genitore deve avere sempre la precedenza sullo stato precedente dei figli,
// a prescindere dal loro updated_at: è un evento strutturale, non una
// modifica di contenuto in competizione con altre.
func cascadeSoftDeleteFolder(ctx context.Context, foldersTx *db.FoldersRepo, notesTx *db.NotesRepo, userID, rootFolderID string, now int64) error {
	deletedAt := now
	queue := []string{rootFolderID}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		noteIDs, err := notesTx.ListActiveIDsByFolder(ctx, userID, current)
		if err != nil {
			return err
		}
		for _, id := range noteIDs {
			if err := notesTx.ForceSet(ctx, userID, id, now, &deletedAt); err != nil {
				return err
			}
		}

		childIDs, err := foldersTx.ListActiveChildIDs(ctx, userID, current)
		if err != nil {
			return err
		}
		for _, id := range childIDs {
			if err := foldersTx.ForceSet(ctx, userID, id, now, &deletedAt); err != nil {
				return err
			}
			queue = append(queue, id)
		}
	}
	return nil
}

// sqlTx è il sottoinsieme di *sql.Tx usato da beginTxWithRetry.
type sqlTx interface {
	Commit() error
	Rollback() error
}

// beginTxWithRetry avvia una transazione ritentando, con backoff crescente,
// solo in caso di contesa transitoria del database. È sicuro ritentare qui
// perché nessuna scrittura è ancora avvenuta.
func (h *SyncHandler) beginTxWithRetry(ctx context.Context) (*sql.Tx, error) {
	var lastErr error
	for attempt := 0; attempt <= maxBeginTxRetries; attempt++ {
		tx, err := h.SQLDB.BeginTx(ctx, nil)
		if err == nil {
			return tx, nil
		}
		lastErr = err
		if !isTransientBusyErr(err) || attempt == maxBeginTxRetries {
			break
		}
		backoff := beginTxBackoff[attempt]
		backoff += time.Duration(rand.Int63n(int64(backoff) / 2))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
	}
	return nil, lastErr
}

// ensure sqlTx stays referenced for interface documentation purposes even
// though *sql.Tx already satisfies it structurally and no variable of type
// sqlTx is otherwise declared.
var _ sqlTx = (*sql.Tx)(nil)
