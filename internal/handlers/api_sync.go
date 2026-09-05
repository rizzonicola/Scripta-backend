package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"notes-server/internal/db"
	"notes-server/internal/middleware"
	"notes-server/internal/models"
	"notes-server/internal/storage"
)

type SyncHandler struct {
	Notes *db.NotesRepo
	Store *storage.Store
}

func NewSyncHandler(notes *db.NotesRepo, store *storage.Store) *SyncHandler {
	return &SyncHandler{Notes: notes, Store: store}
}

// Sync gestisce POST /api/v1/sync.
//
// Per ogni nota inviata dal client mobile:
//   - se non esiste sul server, oppure il client è più recente o uguale
//     (updated_at client >= updated_at server) -> vince il client:
//     il file viene scritto (o cancellato) in modo atomico e i metadati aggiornati.
//     La nota entra nella lista "accepted".
//   - se il server ha una versione più recente -> vince il server:
//     la nota (con contenuto letto da disco) entra nella lista "server_wins"
//     così il client può aggiornare la propria copia locale.
//
// L'intero batch di aggiornamenti ai metadati viene eseguito dentro un'unica
// transazione esplicita: meno round-trip al DB, un solo fsync a fine batch
// (con synchronous=NORMAL + WAL) e atomicità sui metadati dell'intera
// richiesta di sync. Per ogni nota, la sequenza "leggi stato attuale -> decidi
// il vincitore -> scrivi file -> aggiorna metadati" è racchiusa in un lock
// per-path, per evitare race condition quando lo stesso utente sincronizza da
// più dispositivi in parallelo sullo stesso file.
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

	var req models.SyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "body JSON non valido")
		return
	}

	resp := models.SyncResponse{
		Accepted:   []models.NoteResult{},
		ServerWins: []models.NoteResult{},
	}

	tx, notesTx, err := h.Notes.BeginTx(ctx)
	if err != nil {
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
		writeJSONError(w, http.StatusInternalServerError, "errore salvataggio sync")
		return
	}
	committed = true

	writeJSON(w, http.StatusOK, resp)
}

// syncOne applica la risoluzione dei conflitti last-write-wins per una singola
// nota, sotto il lock per-path dello Store: la sequenza lettura-stato ->
// scrittura-file -> upsert-metadati è così atomica rispetto ad altre sync
// concorrenti sullo stesso file.
func (h *SyncHandler) syncOne(ctx context.Context, notesTx *db.NotesRepo, userID, fullPath string, change models.NoteChange, resp *models.SyncResponse) error {
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

func sha256Hex(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}
