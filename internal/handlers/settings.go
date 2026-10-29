package handlers

import (
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

// maxSettingsBodyBytes limita il body di PUT /api/v1/user/settings: è un
// oggetto piatto di pochi campi scalari, 16 KiB è già ordini di grandezza
// oltre il necessario.
const maxSettingsBodyBytes = 16 << 10 // 16 KiB

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
	if !decodeJSONBody(w, r, &incoming, maxSettingsBodyBytes) {
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

var (
	validThemes = map[string]struct{}{
		"light": {}, "dark": {}, "system": {},
	}
	validColorSchemes = map[string]struct{}{
		"dark_teal": {}, "oled": {}, "nord": {}, "midnight_purple": {},
		"forest": {}, "coffee": {}, "clean_light": {}, "solarized_light": {},
	}
	validLanguages = map[string]struct{}{
		"it": {}, "en": {}, "fr": {}, "system": {},
	}
	validFontFamilies = map[string]struct{}{
		"Inter": {}, "JetBrains Mono": {},
	}
	validLayouts = map[string]struct{}{
		"single_pane": {}, "split": {}, "grid": {},
	}
)

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
	if _, ok := validThemes[s.Theme]; !ok {
		return errInvalidField("theme non valido")
	}
	if _, ok := validColorSchemes[s.ColorScheme]; !ok {
		return errInvalidField("color_scheme non valido")
	}
	if _, ok := validLanguages[s.Language]; !ok {
		return errInvalidField("language non valido")
	}
	if _, ok := validFontFamilies[s.FontFamily]; !ok {
		return errInvalidField("font_family non valido")
	}
	if _, ok := validLayouts[s.Layout]; !ok {
		return errInvalidField("layout non valido")
	}
	return nil
}

type errInvalidField string

func (e errInvalidField) Error() string { return string(e) }
