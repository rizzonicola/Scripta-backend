package handlers

import (
	"net/http"
	"os"
	"path/filepath"

	"notes-server/internal/middleware"
	"notes-server/internal/storage"
)

type NotesHandler struct {
	Store *storage.Store
}

func NewNotesHandler(store *storage.Store) *NotesHandler {
	return &NotesHandler{Store: store}
}

// Download gestisce GET /api/v1/notes/download?path=<relative_path>
func (h *NotesHandler) Download(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "metodo non consentito")
		return
	}

	userID, ok := middleware.UserIDFromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "utente non autenticato")
		return
	}

	relativePath := r.URL.Query().Get("path")
	if relativePath == "" {
		writeJSONError(w, http.StatusBadRequest, "parametro 'path' obbligatorio")
		return
	}

	fullPath, err := h.Store.ResolvePath(userID, relativePath)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "percorso non valido")
		return
	}

	content, err := h.Store.ReadFileCtx(r.Context(), fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSONError(w, http.StatusNotFound, "file non trovato")
			return
		}
		if r.Context().Err() != nil {
			// client disconnesso o richiesta scaduta prima del completamento
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "errore lettura file")
		return
	}

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filepath.Base(fullPath)+"\"")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}
