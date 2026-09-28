package models

// User rappresenta un utente registrato dell'applicazione.
type User struct {
	ID           string
	Username     string
	PasswordHash string
	CreatedAt    int64 // unix millis
}

// Folder rappresenta una cartella nell'albero gerarchico dell'utente.
//
// NOTA ARCHITETTURALE: la gerarchia è interamente ID-based. Una cartella non
// ha alcun concetto di "percorso" testuale: la sua posizione nell'albero è
// determinata unicamente da ParentID (nil per una cartella radice). Spostare
// una cartella significa scrivere un nuovo ParentID, punto: nessun rename,
// nessuna riscrittura ricorsiva di percorsi figli, nessuna corsa con il
// filesystem. Il contenuto delle note vive direttamente nel database (vedi
// Note.Content), quindi non esiste più un filesystem per-utente da tenere in
// sincrono con i metadati: i metadati SONO l'unica fonte di verità.
type Folder struct {
	ID        string
	UserID    string
	Name      string
	ParentID  *string // nil = cartella radice
	UpdatedAt int64   // unix millis UTC (orologio CLIENT), usato SOLO per la risoluzione LWW
	SyncedAt  int64   // unix millis (orologio SERVER, monotono), usato SOLO come cursore di pull
	DeletedAt *int64  // nil = attiva; non-nil = soft-deleted (tombstone)
}

// Note rappresenta una nota. Il contenuto Markdown è una colonna del
// database (Content), non un file su disco: questo elimina ogni necessità
// di risolvere/validare percorsi e ogni possibilità di path traversal.
type Note struct {
	ID         string
	UserID     string
	Title      string
	Content    string
	FolderID   *string // nil = nota nella radice ("Tutte le note" la mostra comunque)
	IsFavorite bool
	IsPinned   bool
	OrderIndex int
	UpdatedAt  int64  // unix millis UTC (orologio CLIENT), usato SOLO per la risoluzione LWW
	SyncedAt   int64  // unix millis (orologio SERVER, monotono), usato SOLO come cursore di pull
	DeletedAt  *int64 // nil = attiva; non-nil = soft-deleted (tombstone)
}

// FolderDTO è la rappresentazione JSON di una cartella scambiata durante la
// sync, sia in push (client -> server) sia in pull (server -> client).
type FolderDTO struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	ParentID  *string `json:"parent_id"`
	UpdatedAt int64   `json:"updated_at"`
	DeletedAt *int64  `json:"deleted_at"`
}

// NoteDTO è la rappresentazione JSON di una nota scambiata durante la sync.
//
// IsFavorite/IsPinned/OrderIndex viaggiano nel protocollo di sync esattamente
// come title/content/folder_id: sono metadati mutabili dell'entità "nota", e
// come tali soggetti alla stessa risoluzione LWW basata su UpdatedAt. Prima
// della loro introduzione qui, lo stato "pinned" impostato sul client non
// veniva MAI inviato al server (il DTO non aveva il campo) né, di
// conseguenza, mai restituito dalla pull: il giro di sync successivo
// applicava comunque localmente la nota appena echeggiata dal server con
// updated_at >= a quello locale, sovrascrivendo silenziosamente isPinned con
// il default false. Vedi lo stesso commento in NoteChangeDto lato client.
type NoteDTO struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	Content    string  `json:"content"`
	FolderID   *string `json:"folder_id"`
	IsFavorite bool    `json:"is_favorite"`
	IsPinned   bool    `json:"is_pinned"`
	OrderIndex int     `json:"order_index"`
	UpdatedAt  int64   `json:"updated_at"`
	DeletedAt  *int64  `json:"deleted_at"`
}

// SyncRequest è il body di POST /api/v1/sync.
//
// LastSyncedAt è il "cursore" di pull del client: il server_time (unix
// millis, tempo SERVER) restituito dall'ultima sync riuscita. Il server lo
// confronta SOLO con synced_at (stamp del server). Folders e Notes sono le
// entità modificate localmente e non ancora inviate: il client le individua
// con un flag "dirty" locale, NON confrontando il proprio updated_at con il
// cursore (orologi diversi: con clock skew si perderebbero modifiche).
type SyncRequest struct {
	LastSyncedAt int64       `json:"last_synced_at"`
	Folders      []FolderDTO `json:"folders"`
	Notes        []NoteDTO   `json:"notes"`
}

// SyncResponse è la risposta di POST /api/v1/sync.
//
// ServerTime è lo stamp monotono del server assegnato a questa sync: il
// client lo salva come proprio LastSyncedAt per la sync successiva. È
// l'unico orologio che conta per il cursore; updated_at (orologio del
// client) serve solo alla risoluzione LWW.
//
// Folders e Notes contengono TUTTE le entità dell'utente con
// synced_at in (SyncRequest.LastSyncedAt, ServerTime], calcolato DOPO aver
// applicato le modifiche push del client in questa stessa richiesta. Questo include, in
// un colpo solo:
//   - le modifiche remote arrivate da altri dispositivi dall'ultima sync;
//   - le voci che il client ha appena inviato e che sono state accettate
//     (utile come conferma/eco, il client può ignorarle o usarle per
//     verificare che id/updated_at coincidano);
//   - le voci in cui il client ha PERSO il conflitto LWW: la versione
//     autoritativa del server (più recente di quella inviata) viene
//     restituita qui, così il client la applica localmente.
//
// Non esistono più liste separate "accepted"/"server_wins": la pull unificata
// è già la risposta corretta in tutti e tre i casi, il che semplifica sia il
// protocollo sia il client (non deve più distinguere i tre casi).
//
// FullResync è true quando il cursore inviato dal client è più vecchio della
// finestra di retention dei tombstone (o non plausibile): i tombstone che il
// client non ha mai visto potrebbero essere già stati eliminati fisicamente,
// quindi il server risponde con lo stato COMPLETO dell'utente e il client
// deve eliminare le righe locali "pulite" che non compaiono nella risposta.
type SyncResponse struct {
	ServerTime int64       `json:"server_time"`
	FullResync bool        `json:"full_resync"`
	Folders    []FolderDTO `json:"folders"`
	Notes      []NoteDTO   `json:"notes"`
}

// RejectedItem descrive un singolo record del batch di push rifiutato dal
// server (es. folder_id/parent_id inesistente o di un altro utente).
type RejectedItem struct {
	Kind   string `json:"kind"` // "folder" | "note"
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// SyncRejectedResponse è il body dell'errore 422 restituito da POST
// /api/v1/sync quando almeno un record è stato rifiutato. In quel caso il
// server ha eseguito il ROLLBACK dell'intero batch (nessuna modifica
// applicata) e il client NON deve avanzare il proprio cursore.
type SyncRejectedResponse struct {
	Error    string         `json:"error"`
	Rejected []RejectedItem `json:"rejected"`
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
