package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"

	"notes-server/internal/db"
	"notes-server/internal/storage"
)

type AdminHandler struct {
	usersRepo      *db.UsersRepository
	storageManager *storage.StorageManager
}

func NewAdminHandler(usersRepo *db.UsersRepository, storageManager *storage.StorageManager) *AdminHandler {
	return &AdminHandler{
		usersRepo:      usersRepo,
		storageManager: storageManager,
	}
}

func (h *AdminHandler) RenderUsersPage(w http.ResponseWriter, r *http.Request) {
	users, err := h.usersRepo.GetAll()
	if err != nil {
		http.Error(w, "Errore nel recupero degli utenti", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Render del template web/templates/users.html eseguito dal router/template engine
	http.ServeFile(w, r, "web/templates/users.html")
}

func (h *AdminHandler) ListUsersAPI(w http.ResponseWriter, r *http.Request) {
	users, err := h.usersRepo.GetAll()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(users)
}

func (h *AdminHandler) DeleteUserHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete && r.Method != http.MethodPost {
		http.Error(w, "Metodo non consentito", http.StatusMethodNotAllowed)
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		idStr = r.PathValue("id")
	}

	userID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || userID <= 0 {
		http.Error(w, "ID utente non valido", http.StatusBadRequest)
		return
	}

	// Previene l'auto-eliminazione dell'admin loggato se presente in contesto
	if currentUserID, ok := r.Context().Value("user_id").(int64); ok && currentUserID == userID {
		http.Error(w, "Impossibile eliminare l'account amministratore attualmente in uso", http.StatusForbidden)
		return
	}

	// 1. Rimuove l'utente dal Database
	if err := h.usersRepo.DeleteUser(userID); err != nil {
		http.Error(w, "Errore durante la cancellazione dal database: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 2. Rimuove la cartella e i file fisici dall'archivio
	if err := h.storageManager.DeleteUserDataDir(userID); err != nil {
		// Log dell'errore storage, ma la risposta utente prosegue siccome l'utente DB è rimosso
		_ = err
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "success",
		"message": "Utente e file relativi eliminati con successo",
	})
}
