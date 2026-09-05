package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"

	"notes-server/internal/auth"
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

type CreateUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	IsAdmin  bool   `json:"is_admin"`
}

func (h *AdminHandler) CreateUserHandler(w http.ResponseWriter, r *http.Request) {
	var req CreateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Payload non valido", http.StatusBadRequest)
		return
	}

	if req.Username == "" || req.Password == "" {
		http.Error(w, "Username e password sono obbligatori", http.StatusBadRequest)
		return
	}

	hashedPassword, err := auth.HashPassword(req.Password)
	if err != nil {
		http.Error(w, "Errore durante la cifratura della password", http.StatusInternalServerError)
		return
	}

	user, err := h.usersRepo.Create(req.Username, hashedPassword, req.IsAdmin)
	if err != nil {
		http.Error(w, "Impossibile creare l'utente: "+err.Error(), http.StatusBadRequest)
		return
	}

	_ = h.storageManager.EnsureUserDir(user.ID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(user)
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

	if currentUserID, ok := r.Context().Value("user_id").(int64); ok && currentUserID == userID {
		http.Error(w, "Impossibile eliminare l'account amministratore attualmente in uso", http.StatusForbidden)
		return
	}

	if err := h.usersRepo.DeleteUser(userID); err != nil {
		http.Error(w, "Errore durante la cancellazione dal database: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := h.storageManager.DeleteUserDataDir(userID); err != nil {
		_ = err
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "success",
		"message": "Utente e dati rimossi con successo",
	})
}
