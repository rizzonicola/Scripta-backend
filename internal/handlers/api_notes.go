package handlers

import (
	"fmt"
	"net/http"
	"strings"

	"notes-server/internal/db"
	"notes-server/internal/middleware"
)

// NotesHandler.DownloadMarkdown è instradata su un unico pattern piatto
// ("/api/v1/notes/download", registrato in main.go) perché lo stdlib
// http.ServeMux di questo progetto non usa i pattern con wildcard "{id}"
// introdotti in Go 1.22: l'ID della nota arriva quindi come query string
// (?id=...), non come segmento di path.

// NotesHandler espone operazioni di sola lettura/utilità sulle note che non
// fanno parte del protocollo di sync vero e proprio (quello è interamente
// gestito da SyncHandler). Oggi l'unica di queste è l'export di una singola
// nota come file Markdown grezzo, utile per il download diretto dal browser
// o da un client che non vuole implementare la logica di sync solo per
// leggere una nota.
//
// A differenza della generazione precedente, qui non c'è alcun filesystem da
// interrogare: il contenuto vive nella colonna "content" della tabella
// "notes", quindi non esiste più alcuna possibilità di path traversal né
// necessità di validare/risolvere percorsi.
type NotesHandler struct {
	Notes *db.NotesRepo
}

func NewNotesHandler(notes *db.NotesRepo) *NotesHandler {
	return &NotesHandler{Notes: notes}
}

// DownloadMarkdown gestisce GET /api/v1/notes/download?id=<uuid>, restituendo
// il contenuto della nota come allegato text/markdown scaricabile. Risponde
// 404 sia se la nota non esiste sia se è di un altro utente (Get è già
// filtrato per user_id) sia se è un tombstone (deleted_at valorizzato): in
// tutti e tre i casi non c'è nulla di legittimo da scaricare.
func (h *NotesHandler) DownloadMarkdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "metodo non consentito")
		return
	}

	ctx := r.Context()
	userID, ok := middleware.UserIDFromContext(ctx)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "utente non autenticato")
		return
	}

	noteID := r.URL.Query().Get("id")
	if noteID == "" {
		writeJSONError(w, http.StatusBadRequest, "parametro 'id' mancante")
		return
	}

	note, err := h.Notes.Get(ctx, userID, noteID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "errore lettura nota")
		return
	}
	if note == nil || note.DeletedAt != nil {
		writeJSONError(w, http.StatusNotFound, "nota non trovata")
		return
	}

	filename := sanitizeFilename(note.Title)
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.md"`, filename))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(note.Content))
}

// sanitizeFilename produce un nome file sicuro a partire dal titolo di una
// nota: rimuove i separatori di percorso e i caratteri tipicamente non validi
// nei nomi file su Windows/macOS/Linux, e ricade su "nota" se il titolo,
// dopo la pulizia, risultasse vuoto.
func sanitizeFilename(title string) string {
	title = strings.TrimSpace(title)
	replacer := strings.NewReplacer(
		"/", "-", "\\", "-", ":", "-", "*", "-", "?", "-",
		"\"", "-", "<", "-", ">", "-", "|", "-", "\x00", "",
	)
	title = replacer.Replace(title)
	if title == "" {
		title = "nota"
	}
	return title
}
