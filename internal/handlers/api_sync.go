package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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

// TombstonePurgeSafetyMargin è il margine aggiuntivo prima dell'hard-delete:
// una sync già in volo con un cursore appena dentro la retention deve poter
// ancora vedere il tombstone. Senza margine, la purge può cancellarlo mentre
// la richiesta è in coda, e il client non riceverebbe mai la cancellazione
// (il suo cursore non è abbastanza vecchio per un full_resync).
const TombstonePurgeSafetyMargin = 5 * time.Minute

// MaxClockSkew è la tolleranza massima accettata tra l'orologio del client e
// quello del server: un updated_at/deleted_at che supera "adesso + 5 minuti"
// non è una modifica reale ma un orologio sballato (o un tentativo di
// "bloccare" una riga per sempre: con LWW un timestamp nel futuro vincerebbe
// contro qualunque modifica legittima successiva). Il server lo CORREGGE
// riportandolo all'istante corrente del server.
const MaxClockSkew = 5 * time.Minute

// stampLease è di quanto l'high-water mark persistente supera lo stamp
// corrente (Y-04). Più è largo, più raramente si scrive su disco (al massimo
// una volta per lease, non a ogni sync); più è stretto, meno lo stamp «salta
// avanti» rispetto all'orologio dopo un riavvio. Un minuto è il valore
// proposto dal report.
const stampLease = 60 * time.Second

// Limiti di sanità sui campi ricevuti dal client.
const (
	maxIDLen     = 64
	maxNameLen   = 255
	maxTitleLen  = 1024
	maxBatchSize = 100000
)

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

	// stampMu serializza le sync e garantisce che gli stamp (synced_at)
	// assegnati siano strettamente crescenti nell'ordine di commit: vedi
	// nextStamp. Il server è un singolo processo con un singolo writer
	// SQLite, quindi la serializzazione non costa throughput reale.
	stampMu   sync.Mutex
	lastStamp int64

	// Y-04: high-water mark persistente degli stamp. Invariante: ogni stamp
	// consegnato a un client è <= dell'hwm GIÀ scritto su disco. Dopo un
	// riavvio lastStamp riparte dall'hwm, quindi i nuovi stamp superano ogni
	// cursore mai consegnato, anche se l'orologio di sistema è tornato
	// indietro. Tutti e tre i campi sono protetti da stampMu.
	SyncState   *db.SyncStateRepo // bound a SQLDB (creato al bisogno)
	stampLoaded bool
	stampHWM    int64

	// clock è un gancio per i test (orologio finto). Nil = time.Now.
	clock func() time.Time
}

func NewSyncHandler(sqlDB *sql.DB) *SyncHandler {
	return &SyncHandler{
		SQLDB:        sqlDB,
		Folders:      db.NewFoldersRepo(sqlDB),
		Notes:        db.NewNotesRepo(sqlDB),
		SyncState:    db.NewSyncStateRepo(sqlDB),
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

// nextStamp restituisce lo stamp SERVER per la sync corrente: l'orologio
// del server, ma mai <= dello stamp precedente (monotono anche se l'orologio
// di sistema torna indietro). DEVE essere chiamata con stampMu acquisito,
// DENTRO la sezione critica che comprende anche il commit.
func (h *SyncHandler) nextStamp() int64 {
	now := h.nowMillis()
	if now <= h.lastStamp {
		now = h.lastStamp + 1
	}
	h.lastStamp = now
	return now
}

// nowMillis è l'orologio di sistema in millisecondi (sostituibile nei test).
func (h *SyncHandler) nowMillis() int64 {
	if h.clock != nil {
		return h.clock().UnixMilli()
	}
	return time.Now().UnixMilli()
}

// reserveStamp assegna lo stamp della sync corrente (Y-04). DEVE essere
// chiamata con stampMu acquisito e PRIMA di aprire la transazione di sync.
//
// Perché serve: lastStamp solo in RAM ripartiva da zero a ogni riavvio. Con
// l'orologio di sistema tornato indietro (step NTP, snapshot di VM/container)
// i nuovi stamp risultavano minori dei cursori già consegnati e le righe
// scritte da quel momento non arrivavano mai ai dispositivi, senza errori.
// Persistere MAX(synced_at) non basta: lo stamp di una sync a vuoto non è
// scritto in nessuna riga. Si persiste quindi il massimo stamp EMESSO, con un
// margine («lease»): si scrive su disco solo quando lo stamp raggiunge l'hwm
// già scritto, cioè al massimo una volta per stampLease.
//
// Se la scrittura dell'hwm fallisce NON si consegna nessuno stamp (si
// risponde con un errore e il client riprova): consegnare uno stamp oltre
// l'hwm persistente romperebbe l'invariante. Non cambia il significato del
// cursore, né LWW, né i tombstone: è solo più restrittivo di prima.
func (h *SyncHandler) reserveStamp(ctx context.Context) (int64, error) {
	if h.SyncState == nil {
		h.SyncState = db.NewSyncStateRepo(h.SQLDB)
	}
	if !h.stampLoaded {
		hwm, err := h.SyncState.GetInt(ctx, db.StampHWMKey)
		if err != nil {
			return 0, err
		}
		h.stampHWM = hwm
		if hwm > h.lastStamp {
			h.lastStamp = hwm
		}
		h.stampLoaded = true
	}
	stamp := h.nextStamp()
	if stamp >= h.stampHWM {
		newHWM := stamp + stampLease.Milliseconds()
		if err := h.SyncState.SetMaxInt(ctx, db.StampHWMKey, newHWM); err != nil {
			return 0, err
		}
		h.stampHWM = newHWM
	}
	return stamp, nil
}

// clampClientTime corregge un timestamp del client: mai nel futuro oltre
// MaxClockSkew, mai <= 0. Restituisce anche se è stato modificato.
func clampClientTime(ts, serverNow int64) (int64, bool) {
	if ts <= 0 {
		return serverNow, true
	}
	if ts > serverNow+MaxClockSkew.Milliseconds() {
		return serverNow, true
	}
	return ts, false
}

func validID(id string) bool {
	if id == "" || len(id) > maxIDLen {
		return false
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// isOwnershipViolation riconosce il rifiuto, da parte dei trigger SQLite
// definiti in internal/db/db.go (addOwnershipTriggers), di un INSERT/UPDATE
// che assegnerebbe a una nota/cartella un parent_id/folder_id inesistente o
// appartenente a un altro utente.
//
// Riconosce il messaggio tramite db.OwnershipViolationMarker (letteralmente
// "OWNERSHIP_VIOLATION", lo stesso testo che i trigger passano a
// RAISE(ABORT, ...)): un marcatore scelto da noi, non un pattern-matching
// sul testo generico di un errore SQLite, quindi non fragile rispetto a
// come il driver libSQL formatta i propri messaggi di errore.
//
// A differenza di una busy-error, questa non è transitoria: significa che
// il singolo elemento del batch inviato dal client è, di per sé, invalido.
// Un RAISE(ABORT, ...) in un trigger BEFORE annulla solo l'INSERT/UPDATE che
// lo ha innescato: la transazione in corso resta pienamente utilizzabile per
// gli statement successivi — per questo il chiamante può semplicemente
// scartare l'elemento incriminato e proseguire il resto del batch, invece di
// far fallire l'intera sync.
func isOwnershipViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), db.OwnershipViolationMarker)
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
//     synced_at (stamp SERVER monotono, NON l'updated_at del client) nell'
//     intervallo (last_synced_at, server_time]. Poiché questa query viene
//     eseguita DOPO aver applicato push e cascata, include sia le modifiche
//     remote di altri dispositivi sia l'esito (accettato o "server wins") di
//     ciò che il client ha appena inviato.
//  4. TIMESTAMP: updated_at/deleted_at del client oltre "adesso + 5 min"
//     (MaxClockSkew) vengono riportati all'ora del server. updated_at serve
//     SOLO alla risoluzione LWW, mai come cursore.
//  5. RIFIUTI: un record con parent_id/folder_id non valido causa un 422
//     esplicito con l'elenco dei record rifiutati e il ROLLBACK dell'intero
//     batch: il client non riceve server_time e non avanza il cursore.
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
	req, ok := decodeSyncRequest(w, r, maxBody)
	if !ok {
		return
	}
	if len(req.Folders)+len(req.Notes) > maxBatchSize {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "troppi elementi nel batch")
		return
	}

	// La sezione critica (lock + stamp + transazione + commit + pull) è
	// in process(); la scrittura della risposta HTTP avviene FUORI dal lock,
	// così un client lento a leggere non blocca le altre sync.
	res := h.process(ctx, userID, req)
	switch {
	case res.aborted:
		return // client disconnesso
	case res.rejected != nil:
		// Una riga di riepilogo per ogni 422, con la versione dell'app (solo
		// per capire chi sta inviando cosa; nessun titolo né contenuto).
		log.Printf("sync: 422, %d elementi rifiutati (client=%s)", len(res.rejected), clientTag(r))
		writeJSON(w, http.StatusUnprocessableEntity, models.SyncRejectedResponse{
			Error:    "alcuni elementi sono stati rifiutati: riferimento a cartella non valido",
			Rejected: res.rejected,
		})
	case res.status != 0:
		if res.retryAfter {
			w.Header().Set("Retry-After", "1")
		}
		writeJSONError(w, res.status, res.message)
	default:
		setProtoHeader(w)
		writeJSON(w, http.StatusOK, res.resp)
	}
}

// decodeSyncRequest legge il body di sync in due fasi: prima conserva gli
// array come JSON grezzo, ne conta gli elementi e solo se il batch rientra
// nel limite li decodifica nei DTO. Così un array enorme viene rifiutato con
// 413 senza allocare prima tutti i modelli (il solo limite di byte non
// impedisce a un body da 64 MiB di contenere centinaia di migliaia di
// oggetti con molti campi).
func decodeSyncRequest(w http.ResponseWriter, r *http.Request, maxBytes int64) (*models.SyncRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)

	var env struct {
		LastSyncedAt int64           `json:"last_synced_at"`
		Folders      json.RawMessage `json:"folders"`
		Notes        json.RawMessage `json:"notes"`
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&env); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "payload troppo grande")
		} else {
			writeJSONError(w, http.StatusBadRequest, "body JSON non valido")
		}
		return nil, false
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "body JSON non valido")
		return nil, false
	}

	nFolders, err := countJSONArray(env.Folders)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "body JSON non valido")
		return nil, false
	}
	nNotes, err := countJSONArray(env.Notes)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "body JSON non valido")
		return nil, false
	}
	if nFolders+nNotes > maxBatchSize {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "troppi elementi nel batch")
		return nil, false
	}

	req := &models.SyncRequest{LastSyncedAt: env.LastSyncedAt}
	if len(env.Folders) > 0 {
		if err := json.Unmarshal(env.Folders, &req.Folders); err != nil {
			writeJSONError(w, http.StatusBadRequest, "body JSON non valido")
			return nil, false
		}
	}
	if len(env.Notes) > 0 {
		if err := json.Unmarshal(env.Notes, &req.Notes); err != nil {
			writeJSONError(w, http.StatusBadRequest, "body JSON non valido")
			return nil, false
		}
	}
	return req, true
}

// countJSONArray conta gli elementi di un array JSON senza costruire i
// valori intermedi. Si ferma appena supera maxBatchSize: al chiamante basta
// sapere che il limite è stato superato.
func countJSONArray(raw json.RawMessage) (int, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return 0, err
	}
	d, ok := tok.(json.Delim)
	if !ok || d != '[' {
		return 0, errors.New("atteso un array JSON")
	}
	count := 0
	for dec.More() {
		count++
		if count > maxBatchSize {
			return count, nil
		}
		if err := skipJSONValue(dec); err != nil {
			return 0, err
		}
	}
	tok, err = dec.Token()
	if err != nil {
		return 0, err
	}
	if d, ok := tok.(json.Delim); !ok || d != ']' {
		return 0, errors.New("array JSON non chiuso")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return 0, errors.New("contenuto extra dopo l'array")
	}
	return count, nil
}

// skipJSONValue consuma un valore JSON arbitrario (oggetto, array o
// scalare) usando solo i token del decoder, senza materializzarlo.
func skipJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch d {
	case '{':
		for dec.More() {
			if _, err := dec.Token(); err != nil { // chiave
				return err
			}
			if err := skipJSONValue(dec); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil { // '}'
			return err
		}
	case '[':
		for dec.More() {
			if err := skipJSONValue(dec); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil { // ']'
			return err
		}
	default:
		return errors.New("delimitatore JSON inatteso")
	}
	return nil
}

// syncResult è l'esito di process: esattamente uno tra resp (successo),
// rejected (422), status/message (altro errore) o aborted.
type syncResult struct {
	resp       *models.SyncResponse
	rejected   []models.RejectedItem
	status     int
	message    string
	retryAfter bool
	aborted    bool
}

func failure(status int, msg string) syncResult { return syncResult{status: status, message: msg} }

func (h *SyncHandler) process(ctx context.Context, userID string, req *models.SyncRequest) syncResult {
	// Lock + stamp: lo stamp è assegnato DOPO aver ottenuto il lock, quindi
	// gli stamp risultano crescenti nell'ordine di commit. È ciò che rende
	// sicuro usare "server_time" come cursore: ogni riga committata da una
	// sync successiva ha uno stamp maggiore di quello restituito qui.
	h.stampMu.Lock()
	defer h.stampMu.Unlock()
	now, err := h.reserveStamp(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return syncResult{aborted: true} // client disconnesso
		}
		if isTransientBusyErr(err) {
			return syncResult{status: http.StatusServiceUnavailable, message: "database temporaneamente occupato, riprovare", retryAfter: true}
		}
		log.Printf("sync: errore stato stamp: %v", err)
		return failure(http.StatusInternalServerError, "errore stato sync")
	}

	tx, err := h.beginTxWithRetry(ctx)
	if err != nil {
		if isTransientBusyErr(err) {
			return syncResult{status: http.StatusServiceUnavailable, message: "database temporaneamente occupato, riprovare", retryAfter: true}
		}
		log.Printf("sync: errore avvio transazione: %v", err)
		return failure(http.StatusInternalServerError, "errore avvio transazione")
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	foldersTx := db.NewFoldersRepo(tx)
	notesTx := db.NewNotesRepo(tx)

	// Cartelle (Y-01): vedi applyFolders. Il trigger di ownership richiede che
	// il genitore esista già quando si scrive il figlio, ma l'ordine in cui il
	// client elenca le cartelle sporche non è garantito (listDirty non ha ORDER
	// BY e l'upsert locale cambia rowid): un figlio inviato PRIMA di un
	// genitore nuovo veniva rifiutato con 422 e il client, per «ripararlo», lo
	// spostava in radice, appiattendo la gerarchia.
	rejected, stop := applyFolders(ctx, foldersTx, notesTx, userID, req.Folders, now)
	if stop != nil {
		return *stop
	}

	for _, nc := range req.Notes {
		if ctx.Err() != nil {
			return syncResult{aborted: true}
		}
		if !validID(nc.ID) {
			rejected = append(rejected, models.RejectedItem{Kind: "note", ID: truncateForLog(nc.ID), Reason: "invalid_id"})
			continue
		}
		if nc.FolderID != nil && !validID(*nc.FolderID) {
			rejected = append(rejected, models.RejectedItem{Kind: "note", ID: nc.ID, Reason: "invalid_folder_id"})
			continue
		}
		title := nc.Title
		if utf8.RuneCountInString(title) > maxTitleLen {
			title = string([]rune(title)[:maxTitleLen])
		}
		updatedAt, _ := clampClientTime(nc.UpdatedAt, now)
		var deletedAt *int64
		if nc.DeletedAt != nil {
			d, _ := clampClientTime(*nc.DeletedAt, now)
			deletedAt = &d
		}
		note := &models.Note{
			ID:         nc.ID,
			UserID:     userID,
			Title:      title,
			Content:    nc.Content,
			FolderID:   nc.FolderID,
			IsFavorite: nc.IsFavorite,
			IsPinned:   nc.IsPinned,
			OrderIndex: nc.OrderIndex,
			UpdatedAt:  updatedAt,
			SyncedAt:   now,
			DeletedAt:  deletedAt,
		}
		if err := notesTx.UpsertLWW(ctx, note); err != nil {
			if isOwnershipViolation(err) {
				log.Printf("sync: nota %s rifiutata, folder_id non valido o non dell'utente", nc.ID)
				rejected = append(rejected, models.RejectedItem{Kind: "note", ID: nc.ID, Reason: "invalid_folder_id"})
				continue
			}
			log.Printf("sync: errore upsert nota %s: %v", nc.ID, err)
			return failure(http.StatusInternalServerError, "errore elaborazione nota")
		}
		// Se il server aveva già una versione più recente (LWW perso dal
		// client), la si ri-consegna nella pull di questa stessa richiesta.
		if cur, err := notesTx.Get(ctx, userID, nc.ID); err != nil {
			log.Printf("sync: errore lettura nota %s: %v", nc.ID, err)
			return failure(http.StatusInternalServerError, "errore lettura nota")
		} else if cur != nil && cur.UpdatedAt > updatedAt {
			if err := notesTx.TouchSynced(ctx, userID, nc.ID, now); err != nil {
				log.Printf("sync: errore touch nota %s: %v", nc.ID, err)
				return failure(http.StatusInternalServerError, "errore elaborazione nota")
			}
		}
	}

	// Almeno un record rifiutato: errore ESPLICITO 422, nessuna modifica
	// applicata (il defer fa il rollback dell'intero batch) e nessun
	// server_time nella risposta: il client non ha modo di avanzare il
	// cursore e riproverà dopo aver corretto i record indicati.
	if len(rejected) > 0 {
		return syncResult{rejected: rejected}
	}

	if err := tx.Commit(); err != nil {
		if isTransientBusyErr(err) {
			// NB: qui NON si ritenta tx.Commit() sullo stesso *sql.Tx (una
			// volta fallito è considerato concluso). Il retry corretto è
			// l'intero batch da capo, delegato al client: la sync è
			// idempotente rispetto a un rinvio completo (LWW per
			// id/updated_at dà lo stesso esito a ogni reinvio).
			return syncResult{status: http.StatusServiceUnavailable, message: "database temporaneamente occupato, riprovare", retryAfter: true}
		}
		log.Printf("sync: errore commit: %v", err)
		return failure(http.StatusInternalServerError, "errore salvataggio sync")
	}
	committed = true

	// Cursore del client: non plausibile (negativo / nel futuro) oppure più
	// vecchio della retention dei tombstone => il client potrebbe non aver
	// mai visto tombstone già purgati. Si risponde con lo stato COMPLETO
	// (solo righe attive) e FullResync=true: il client elimina i residui
	// locali che non compaiono nella risposta.
	since := req.LastSyncedAt
	fullResync := false
	if since < 0 || since > now {
		since, fullResync = 0, true
	} else if since > 0 && since < now-TombstoneRetention.Milliseconds() {
		since, fullResync = 0, true
	}
	// Tombstone utili solo per una pull incrementale: a una prima sync o a
	// un full resync basta lo stato attivo.
	includeTombstones := since > 0

	folders, err := h.Folders.ListSyncedBetween(ctx, userID, since, now, includeTombstones)
	if err != nil {
		log.Printf("sync: errore lettura cartelle: %v", err)
		return failure(http.StatusInternalServerError, "errore lettura cartelle aggiornate")
	}
	notes, err := h.Notes.ListSyncedBetween(ctx, userID, since, now, includeTombstones)
	if err != nil {
		log.Printf("sync: errore lettura note: %v", err)
		return failure(http.StatusInternalServerError, "errore lettura note aggiornate")
	}

	resp := &models.SyncResponse{
		ServerTime: now,
		FullResync: fullResync,
		Folders:    make([]models.FolderDTO, 0, len(folders)),
		Notes:      make([]models.NoteDTO, 0, len(notes)),
		Caps:       models.ServerCaps,
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
			ID:         n.ID,
			Title:      n.Title,
			Content:    n.Content,
			FolderID:   n.FolderID,
			IsFavorite: n.IsFavorite,
			IsPinned:   n.IsPinned,
			OrderIndex: n.OrderIndex,
			UpdatedAt:  n.UpdatedAt,
			DeletedAt:  n.DeletedAt,
		})
	}
	return syncResult{resp: resp}
}

// folderOutcome è l'esito dell'applicazione di una singola cartella del batch.
type folderOutcome int

const (
	folderApplied       folderOutcome = iota // upsert (ed eventuale cascata) eseguiti
	folderRejected                           // rifiutata: il motivo è nel RejectedItem restituito
	folderParentMissing                      // il trigger di ownership non trova (ancora) il genitore
)

// applyFolders applica le cartelle del batch dentro la transazione `foldersTx`
// e restituisce i rifiuti. Un valore non nil come secondo risultato significa
// «interrompi e rispondi così» (errore interno o client disconnesso).
//
// Strategia (Y-01), pensata per NON cambiare l'esito di nessun batch che oggi
// riesce:
//
//  1. Prima passata nell'ordine ricevuto, identica a prima. Una cartella il cui
//     genitore non c'è ancora ma È NEL BATCH non viene rifiutata: viene
//     differita.
//  2. Le differite sono ordinate con i genitori prima dei figli
//     (orderDeferredFolders, O(n)) e applicate. Se il genitore, in realtà,
//     manca (non è nel DB e non è stato accettato), il rifiuto resta
//     invalid_parent_id: stesso 422 di oggi.
//  3. Un ciclo fra cartelle nuove (A→B→A) non si può risolvere: le cartelle
//     coinvolte sono rifiutate con invalid_parent_id e il batch intero va in
//     rollback (422), come per ogni altro rifiuto.
//
// Non si riordina l'intero batch di proposito: la cascata di cancellazione
// (cascadeSoftDeleteFolder) dipende dall'ordine, e cambiarlo per batch che già
// funzionano potrebbe cambiare il loro esito (regola 2 del PM: nessuna
// modifica alla semantica di LWW e tombstone).
func applyFolders(ctx context.Context, foldersTx *db.FoldersRepo, notesTx *db.NotesRepo, userID string, folders []models.FolderDTO, now int64) ([]models.RejectedItem, *syncResult) {
	var rejected []models.RejectedItem

	inBatch := make(map[string]struct{}, len(folders))
	for _, fc := range folders {
		inBatch[fc.ID] = struct{}{}
	}

	var deferred []models.FolderDTO
	for _, fc := range folders {
		if ctx.Err() != nil {
			return nil, &syncResult{aborted: true}
		}
		outcome, item, fail := applyFolder(ctx, foldersTx, notesTx, userID, fc, now)
		if fail != nil {
			return nil, fail
		}
		switch outcome {
		case folderRejected:
			rejected = append(rejected, item)
		case folderParentMissing:
			if fc.ParentID != nil {
				if _, ok := inBatch[*fc.ParentID]; ok {
					deferred = append(deferred, fc)
					continue
				}
			}
			// parent_id inesistente o di un altro utente: rifiuto ESPLICITO (non
			// più scarto silenzioso). Il batch verrà annullato per intero e il
			// client NON avanzerà il cursore.
			log.Printf("sync: cartella %s rifiutata, parent_id non valido o non dell'utente", fc.ID)
			rejected = append(rejected, item)
		}
	}

	ordered, cyclic := orderDeferredFolders(deferred)
	for _, fc := range ordered {
		if ctx.Err() != nil {
			return nil, &syncResult{aborted: true}
		}
		outcome, item, fail := applyFolder(ctx, foldersTx, notesTx, userID, fc, now)
		if fail != nil {
			return nil, fail
		}
		switch outcome {
		case folderRejected:
			rejected = append(rejected, item)
		case folderParentMissing:
			log.Printf("sync: cartella %s rifiutata, il genitore indicato nel batch non è valido", fc.ID)
			rejected = append(rejected, item)
		}
	}
	for _, fc := range cyclic {
		log.Printf("sync: cartella %s rifiutata, ciclo o dipendenza irrisolvibile fra cartelle del batch", fc.ID)
		rejected = append(rejected, models.RejectedItem{Kind: "folder", ID: fc.ID, Reason: "invalid_parent_id"})
	}
	return rejected, nil
}

// applyFolder valida e applica UNA cartella (upsert LWW, eventuale
// ri-consegna se il push ha perso, eventuale cascata di cancellazione). Il
// corpo è quello che prima stava inline nel ciclo di process: stesse
// validazioni, stessi messaggi, stesso ordine delle operazioni.
//
// Se il trigger di ownership respinge la riga restituisce folderParentMissing
// con il RejectedItem già pronto: decide il chiamante se differirla o
// rifiutarla. Il terzo risultato è non nil solo per errori interni.
func applyFolder(ctx context.Context, foldersTx *db.FoldersRepo, notesTx *db.NotesRepo, userID string, fc models.FolderDTO, now int64) (folderOutcome, models.RejectedItem, *syncResult) {
	if !validID(fc.ID) {
		return folderRejected, models.RejectedItem{Kind: "folder", ID: truncateForLog(fc.ID), Reason: "invalid_id"}, nil
	}
	if fc.ParentID != nil && (!validID(*fc.ParentID) || *fc.ParentID == fc.ID) {
		return folderRejected, models.RejectedItem{Kind: "folder", ID: fc.ID, Reason: "invalid_parent_id"}, nil
	}
	name := strings.TrimSpace(fc.Name)
	if name == "" {
		name = "Senza nome"
	}
	if utf8.RuneCountInString(name) > maxNameLen {
		name = string([]rune(name)[:maxNameLen])
	}
	updatedAt, _ := clampClientTime(fc.UpdatedAt, now)
	var deletedAt *int64
	if fc.DeletedAt != nil {
		d, _ := clampClientTime(*fc.DeletedAt, now)
		deletedAt = &d
	}
	folder := &models.Folder{
		ID:        fc.ID,
		UserID:    userID,
		Name:      name,
		ParentID:  fc.ParentID,
		UpdatedAt: updatedAt,
		SyncedAt:  now,
		DeletedAt: deletedAt,
	}
	if err := foldersTx.UpsertLWW(ctx, folder); err != nil {
		if isOwnershipViolation(err) {
			return folderParentMissing, models.RejectedItem{Kind: "folder", ID: fc.ID, Reason: "invalid_parent_id"}, nil
		}
		log.Printf("sync: errore upsert cartella %s: %v", fc.ID, err)
		f := failure(http.StatusInternalServerError, "errore elaborazione cartella")
		return folderRejected, models.RejectedItem{}, &f
	}

	// Rileggiamo lo stato risultante (non ciò che il client ha inviato):
	// se l'upsert è stato respinto dalla condizione LWW perché il server
	// aveva già una versione più recente E ATTIVA, la cartella non va
	// messa in cascata solo perché il client la voleva cancellare.
	current, err := foldersTx.Get(ctx, userID, fc.ID)
	if err != nil {
		log.Printf("sync: errore lettura cartella %s: %v", fc.ID, err)
		f := failure(http.StatusInternalServerError, "errore lettura cartella")
		return folderRejected, models.RejectedItem{}, &f
	}
	if current != nil && current.UpdatedAt > updatedAt {
		// Il push ha perso il LWW: ri-consegna la versione vincente.
		if err := foldersTx.TouchSynced(ctx, userID, fc.ID, now); err != nil {
			log.Printf("sync: errore touch cartella %s: %v", fc.ID, err)
			f := failure(http.StatusInternalServerError, "errore elaborazione cartella")
			return folderRejected, models.RejectedItem{}, &f
		}
	}
	if current != nil && current.DeletedAt != nil {
		if err := cascadeSoftDeleteFolder(ctx, foldersTx, notesTx, userID, fc.ID, now); err != nil {
			log.Printf("sync: errore cascata cartella %s: %v", fc.ID, err)
			f := failure(http.StatusInternalServerError, "errore cascata cancellazione cartella")
			return folderRejected, models.RejectedItem{}, &f
		}
	}
	return folderApplied, models.RejectedItem{}, nil
}

// orderDeferredFolders ordina le cartelle differite con i genitori prima dei
// figli (ordinamento topologico di Kahn, O(n), stabile rispetto all'ordine di
// arrivo). Una cartella dipende da un'altra solo se il suo genitore è fra le
// differite: se il genitore è stato applicato più tardi nella prima passata,
// o è già nel DB, non c'è dipendenza e la cartella passa subito.
//
// `cyclic` raccoglie ciò che non si può ordinare: i cicli (A→B→A) e chi
// dipende da un ciclo. Non servono limiti di profondità né ricorsione: lo
// stack non cresce con la profondità dell'albero.
func orderDeferredFolders(deferred []models.FolderDTO) (ordered, cyclic []models.FolderDTO) {
	n := len(deferred)
	if n == 0 {
		return nil, nil
	}
	byID := make(map[string]int, n)
	for i, fc := range deferred {
		byID[fc.ID] = i
	}
	indeg := make([]int, n)
	children := make(map[int][]int, n)
	for i, fc := range deferred {
		if fc.ParentID == nil {
			continue
		}
		if pj, ok := byID[*fc.ParentID]; ok && pj != i {
			indeg[i]++
			children[pj] = append(children[pj], i)
		}
	}
	queue := make([]int, 0, n)
	for i := 0; i < n; i++ {
		if indeg[i] == 0 {
			queue = append(queue, i)
		}
	}
	done := make([]bool, n)
	for head := 0; head < len(queue); head++ {
		i := queue[head]
		done[i] = true
		ordered = append(ordered, deferred[i])
		for _, c := range children[i] {
			indeg[c]--
			if indeg[c] == 0 {
				queue = append(queue, c)
			}
		}
	}
	for i := 0; i < n; i++ {
		if !done[i] {
			cyclic = append(cyclic, deferred[i])
		}
	}
	return ordered, cyclic
}

func truncateForLog(s string) string {
	if len(s) > 32 {
		return s[:32]
	}
	return s
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
			if err := notesTx.ForceSet(ctx, userID, id, now, now, &deletedAt); err != nil {
				return err
			}
		}

		childIDs, err := foldersTx.ListActiveChildIDs(ctx, userID, current)
		if err != nil {
			return err
		}
		for _, id := range childIDs {
			if err := foldersTx.ForceSet(ctx, userID, id, now, now, &deletedAt); err != nil {
				return err
			}
			queue = append(queue, id)
		}
	}
	return nil
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
