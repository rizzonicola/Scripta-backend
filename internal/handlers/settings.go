package handlers

import (
	"encoding/json"
	"net/http"

	"notes-server/internal/db"
	"notes-server/internal/middleware"
	"notes-server/internal/models"
)

type SettingsHandler struct {
	Settings *db.SettingsRepo
}

func NewSettingsHandler(settings *db.SettingsRepo) *SettingsHandler {
	return &SettingsHandler{Settings: settings}
}

// GetSettings gestisce GET /api/v1/user/settings.
// Restituisce le preferenze salvate dell'utente autenticato, oppure i valori
// di default se non ne ha mai salvate.
func (h *SettingsHandler) GetSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "metodo non consentito")
		return
	}

	userID, ok := middleware.UserIDFromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "utente non autenticato")
		return
	}

	settings, err := h.Settings.Get(r.Context(), userID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "errore lettura impostazioni")
		return
	}

	writeJSON(w, http.StatusOK, settings)
}

// UpdateSettings gestisce PUT /api/v1/user/settings.
// Il body deve contenere l'intero oggetto impostazioni (sostituzione completa,
// non merge parziale) — l'app client invia sempre lo stato corrente completo.
func (h *SettingsHandler) UpdateSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeJSONError(w, http.StatusMethodNotAllowed, "metodo non consentito")
		return
	}

	userID, ok := middleware.UserIDFromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "utente non autenticato")
		return
	}

	var incoming models.UserSettings
	if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
		writeJSONError(w, http.StatusBadRequest, "body JSON non valido")
		return
	}

	if err := validateSettings(&incoming); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	saved, err := h.Settings.Upsert(r.Context(), userID, incoming)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "errore salvataggio impostazioni")
		return
	}

	writeJSON(w, http.StatusOK, saved)
}

// validateSettings applica controlli minimi e valori di fallback sicuri,
// evitando di persistere dati palesemente invalidi (es. font size negativo).
func validateSettings(s *models.UserSettings) error {
	defaults := models.DefaultUserSettings()

	if s.Theme == "" {
		s.Theme = defaults.Theme
	}
	if s.ColorScheme == "" {
		s.ColorScheme = defaults.ColorScheme
	}
	if s.Language == "" {
		s.Language = defaults.Language
	}
	if s.FontFamily == "" {
		s.FontFamily = defaults.FontFamily
	}
	if s.Layout == "" {
		s.Layout = defaults.Layout
	}
	if s.FontSize <= 0 {
		s.FontSize = defaults.FontSize
	}
	if s.FontSize < 8 || s.FontSize > 48 {
		return errInvalidField("font_size deve essere tra 8 e 48")
	}
	if s.LineSpacing <= 0 {
		s.LineSpacing = defaults.LineSpacing
	}
	if s.LineSpacing < 0.8 || s.LineSpacing > 3.0 {
		return errInvalidField("line_spacing deve essere tra 0.8 e 3.0")
	}
	return nil
}

type errInvalidField string

func (e errInvalidField) Error() string { return string(e) }
