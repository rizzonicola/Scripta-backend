package models

// User rappresenta un utente registrato dell'applicazione.
type User struct {
	ID           string
	Username     string
	PasswordHash string
	CreatedAt    int64 // unix millis
}

// Note rappresenta i metadati di una nota Markdown tracciati nel DB.
// Il contenuto vero e proprio vive su filesystem in:
// /data/users/{UserID}/notes/{RelativePath}
type Note struct {
	ID           string
	UserID       string
	RelativePath string
	UpdatedAt    int64 // unix millis, usato per la risoluzione dei conflitti LWW
	Deleted      bool
	Checksum     string
}

// NoteChange è il payload inviato dal client mobile durante la sync.
type NoteChange struct {
	RelativePath string `json:"relative_path"`
	Content      string `json:"content"`
	UpdatedAt    int64  `json:"updated_at"` // unix millis
	Deleted      bool   `json:"deleted"`
}

// NoteResult è la rappresentazione di una nota restituita al client
// (sia per conferma sync sia per conflitti risolti dal server).
type NoteResult struct {
	RelativePath string `json:"relative_path"`
	Content      string `json:"content,omitempty"`
	UpdatedAt    int64  `json:"updated_at"`
	Deleted      bool   `json:"deleted"`
}

// SyncRequest è il body di POST /api/v1/sync.
type SyncRequest struct {
	Notes []NoteChange `json:"notes"`
}

// SyncResponse è la risposta di POST /api/v1/sync.
type SyncResponse struct {
	Accepted   []NoteResult `json:"accepted"`    // note del client accettate così come inviate
	ServerWins []NoteResult `json:"server_wins"` // note in cui la versione server era più recente
}

// UserSettings rappresenta le preferenze dell'utente (tema, font, lingua, layout).
// Viene salvata come singola colonna JSON nel DB (tabella user_settings) per
// restare flessibile senza dover fare migrazioni ad ogni nuova preferenza.
type UserSettings struct {
	Theme       string  `json:"theme"`        // es. "light" | "dark" | "system"
	ColorScheme string  `json:"color_scheme"` // es. "blue", "solarized", ecc.
	Language    string  `json:"language"`     // es. "it", "en"
	FontFamily  string  `json:"font_family"`  // es. "Inter", "JetBrains Mono"
	FontSize    int     `json:"font_size"`    // in px/pt
	LineSpacing float64 `json:"line_spacing"` // es. 1.0, 1.5
	Layout      string  `json:"layout"`       // es. "single_pane", "split", "grid"
	UpdatedAt   int64   `json:"updated_at"`   // unix millis
}

// DefaultUserSettings restituisce le preferenze di default per un utente
// che non ha ancora mai salvato impostazioni personalizzate.
func DefaultUserSettings() UserSettings {
	return UserSettings{
		Theme:       "system",
		ColorScheme: "default",
		Language:    "it",
		FontFamily:  "Inter",
		FontSize:    16,
		LineSpacing: 1.4,
		Layout:      "single_pane",
	}
}

// LoginRequest è il body di POST /api/v1/auth/login.
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// LoginResponse è la risposta di POST /api/v1/auth/login.
type LoginResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
	UserID    string `json:"user_id"`
	Username  string `json:"username"`
}
