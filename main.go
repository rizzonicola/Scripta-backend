package main

import (
	"context"
	"database/sql"
	"embed"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"notes-server/internal/auth"
	"notes-server/internal/db"
	"notes-server/internal/handlers"
	"notes-server/internal/middleware"
)

//go:embed web/templates/*.html
var templatesFS embed.FS

// settingsDispatch instrada GET e PUT su /api/v1/user/settings verso i rispettivi
// handler (mux.Handle non fa dispatch per metodo su un singolo pattern).
func settingsDispatch(h *handlers.SettingsHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			h.GetSettings(w, r)
		case http.MethodPut:
			h.UpdateSettings(w, r)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = w.Write([]byte(`{"error":"metodo non consentito"}`))
		}
	}
}

// adminLoginDispatch instrada GET (mostra il form) e POST (verifica le
// credenziali) su /admin/login verso i rispettivi handler, applicando al
// solo POST il rate limiting e il controllo same-origin: il GET si limita a
// servire una pagina statica e non ha bisogno di nessuna delle due difese.
func adminLoginDispatch(h *handlers.AdminHandler, loginGuards func(http.Handler) http.Handler) http.HandlerFunc {
	post := loginGuards(http.HandlerFunc(h.Login))
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			h.LoginPage(w, r)
		case http.MethodPost:
			post.ServeHTTP(w, r)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = w.Write([]byte(`{"error":"metodo non consentito"}`))
		}
	}
}

// chain compone più middleware in ordine di applicazione (il primo elencato
// è il più esterno, eseguito per primo).
func chain(mws ...func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		for i := len(mws) - 1; i >= 0; i-- {
			h = mws[i](h)
		}
		return h
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	dbPath := getEnv("DB_PATH", "data/app.db")
	jwtSecret := getEnv("JWT_SECRET", "change-me-in-production-please")
	adminUser := getEnv("ADMIN_USER", "admin")
	adminPass := getEnv("ADMIN_PASS", "admin")
	port := getEnv("PORT", "8080")

	if jwtSecret == "change-me-in-production-please" {
		log.Println("ATTENZIONE: JWT_SECRET non impostato, viene usato un valore di default INSICURO. Impostare la variabile d'ambiente JWT_SECRET in produzione.")
	}

	// TURSO_SYNC_URL / TURSO_AUTH_TOKEN sono opzionali: se assenti il server
	// funziona come prima, con un file .db locale puro. Se TURSO_SYNC_URL è
	// impostato, dbPath diventa una embedded replica sincronizzata con quel
	// server libSQL/Turso remoto.
	tursoSyncURL := getEnv("TURSO_SYNC_URL", "")
	tursoAuthToken := getEnv("TURSO_AUTH_TOKEN", "")
	var tursoSyncInterval time.Duration
	if raw := getEnv("TURSO_SYNC_INTERVAL", ""); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			tursoSyncInterval = d
		} else {
			log.Printf("TURSO_SYNC_INTERVAL non valido (%q), ignorato: %v", raw, err)
		}
	}

	sqlDB, err := db.OpenWithConfig(db.Config{
		Path:         dbPath,
		PrimaryURL:   tursoSyncURL,
		AuthToken:    tursoAuthToken,
		SyncInterval: tursoSyncInterval,
	})
	if err != nil {
		log.Fatalf("errore apertura database: %v", err)
	}
	defer sqlDB.Close()

	usersRepo := db.NewUsersRepo(sqlDB)
	notesRepo := db.NewNotesRepo(sqlDB)
	settingsRepo := db.NewSettingsRepo(sqlDB)

	// jwtTTL: 24h di default (era 7 giorni). Un TTL corto limita la finestra
	// di validità di un token rubato SENZA bisogno di consultare il DB ad
	// ogni richiesta (la revoca esplicita, vedi TokenManager.Revoke, resta
	// riservata ai soli eventi rari — reset password, cancellazione utente
	// — dove serve invalidare un token PRIMA della sua scadenza naturale).
	jwtTTL := 24 * time.Hour
	if raw := getEnv("JWT_TTL", ""); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			jwtTTL = d
		} else {
			log.Printf("JWT_TTL non valido (%q), uso il default (%s)", raw, jwtTTL)
		}
	}
	tokenManager := auth.NewTokenManager(jwtSecret, jwtTTL)

	// adminSessionTTL: durata del cookie di sessione della dashboard /admin
	// (login form-based, vedi handlers.AdminHandler.Login). Volutamente più
	// lunga del TTL dei token utente: è un'area amministrativa usata
	// saltuariamente, non uno strumento su cui forzare re-login frequenti.
	adminSessionTTL := 8 * time.Hour
	if raw := getEnv("ADMIN_SESSION_TTL", ""); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			adminSessionTTL = d
		} else {
			log.Printf("ADMIN_SESSION_TTL non valido (%q), uso il default (%s)", raw, adminSessionTTL)
		}
	}
	adminSessions := auth.NewAdminSessionManager(jwtSecret, adminSessionTTL)

	// --- Handlers ---
	// Nessuno storage su filesystem da inizializzare: cartelle e note vivono
	// interamente nel database (schema ID-based in internal/db/db.go).
	adminHandler, err := handlers.NewAdminHandler(usersRepo, tokenManager, adminSessions, adminUser, adminPass, templatesFS)
	if err != nil {
		log.Fatalf("errore caricamento template admin: %v", err)
	}
	authHandler := handlers.NewAuthHandler(usersRepo, tokenManager)
	syncHandler := handlers.NewSyncHandler(sqlDB)
	if raw := getEnv("SYNC_MAX_BODY_BYTES", ""); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
			syncHandler.MaxBodyBytes = n
		} else {
			log.Printf("SYNC_MAX_BODY_BYTES non valido (%q), uso il default", raw)
		}
	}
	notesHandler := handlers.NewNotesHandler(notesRepo)
	settingsHandler := handlers.NewSettingsHandler(settingsRepo)

	// --- Purge periodico dei tombstone scaduti + recupero spazio su disco ---
	// Una cancellazione (soft-delete) resta visibile ai client tramite la
	// pull della sync per handlers.TombstoneRetention, poi viene rimossa
	// definitivamente dal database: questo mantiene le tabelle folders/notes
	// libere da tombstone ormai propagati a tutti i dispositivi, senza dover
	// tracciare esplicitamente quali device abbiano già fatto pull di quale
	// tombstone (complessità non necessaria per il caso d'uso di Scripta).
	//
	// L'hard-delete SQL da solo NON riduce la dimensione di app.db/app.db-wal
	// su disco (le pagine liberate restano nel file, vedi maintenance.go):
	// isLocalDB seleziona se la manutenzione (incremental_vacuum +
	// wal_checkpoint TRUNCATE) è applicabile, cosa vera solo quando il DB è
	// aperto in modalità locale pura e non come embedded replica remota.
	isLocalDB := tursoSyncURL == ""
	startTombstonePurgeLoop(notesRepo, db.NewFoldersRepo(sqlDB), sqlDB, isLocalDB)

	mux := http.NewServeMux()

	// --- Rate limiting locale (seconda linea di difesa) ---
	// La difesa PRIMARIA contro credential stuffing/brute force è la
	// Cloudflare WAF Rate Limiting Rule configurata davanti al tunnel
	// (valutata al edge, prima ancora che la richiesta arrivi qui, senza
	// consumare CPU di questo processo): vedi README/documentazione di
	// deploy. Questi limiter locali restano comunque attivi come seconda
	// linea, per il caso in cui il WAF sia assente, disattivato o
	// mal configurato, e per l'uso in sviluppo locale senza Cloudflare
	// davanti. Le soglie sono volutamente permissive (l'utente legittimo non
	// deve mai accorgersene): 1 richiesta ogni 2s a regime, burst di 5.
	loginRateLimit := middleware.RateLimit(0.5, 5)

	// --- Difesa CSRF per le rotte POST della dashboard admin ---
	sameOrigin := middleware.RequireSameOrigin

	// --- Dashboard Admin (login form-based + cookie di sessione) ---
	// Sostituisce il precedente HTTP Basic Auth: vedi
	// internal/handlers/admin.go e web/templates/admin_login.html per i
	// dettagli (login riconosciuto dai password manager, logout esplicito).
	// Su Cloudflare Tunnel è comunque consigliato affiancare Cloudflare
	// Access/Zero Trust davanti a questo path per un'autenticazione a monte.
	adminAuth := middleware.SessionAuthAdmin(adminSessions)
	mux.HandleFunc("/admin/login", adminLoginDispatch(adminHandler, chain(loginRateLimit, sameOrigin)))
	mux.Handle("/admin/logout", adminAuth(sameOrigin(http.HandlerFunc(adminHandler.Logout))))
	mux.Handle("/admin", adminAuth(http.HandlerFunc(adminHandler.UsersPage)))
	mux.Handle("/admin/users/create", adminAuth(sameOrigin(http.HandlerFunc(adminHandler.CreateUser))))
	mux.Handle("/admin/users/reset-password", adminAuth(sameOrigin(http.HandlerFunc(adminHandler.ResetPassword))))
	mux.Handle("/admin/users/delete", adminAuth(sameOrigin(http.HandlerFunc(adminHandler.DeleteUser))))

	// --- API pubbliche (mobile app) ---
	mux.Handle("/api/v1/auth/login", loginRateLimit(http.HandlerFunc(authHandler.Login)))

	// --- API protette da JWT ---
	requireJWT := middleware.RequireJWT(tokenManager)
	mux.Handle("/api/v1/sync", requireJWT(http.HandlerFunc(syncHandler.Sync)))
	mux.Handle("/api/v1/notes/download", requireJWT(http.HandlerFunc(notesHandler.DownloadMarkdown)))
	mux.Handle("/api/v1/user/settings", requireJWT(http.HandlerFunc(settingsDispatch(settingsHandler))))

	// --- Health check & System info ---
	healthHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","name":"Scripta Notes Server","version":"2.0.0","architecture":"local-first delta-sync (id-based, last-write-wins)","license":"GPL-3.0","credits":{"database":"github.com/tursodatabase/go-libsql","jwt":"github.com/golang-jwt/jwt/v5","security":"golang.org/x/crypto/bcrypt","uuid":"github.com/google/uuid"}}` + "\n"))
	}
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/health", healthHandler)

	// SecurityHeaders avvolge l'intero mux (economico da tenere comunque nel
	// backend anche dietro Cloudflare: riguarda il rendering della singola
	// risposta HTML, non qualcosa che un WAF di rete possa sostituire).
	// Gzip comprime le risposte JSON dell'API, le pagine HTML della
	// dashboard admin e gli export Markdown quando il client dichiara
	// supporto, senza alcuna modifica ai contratti/endpoint esposti (vedi
	// internal/middleware/compress.go).
	handler := middleware.SecurityHeaders(middleware.Gzip(mux))

	addr := ":" + port
	log.Printf("server in ascolto su %s (admin: http://localhost%s/admin)", addr, addr)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("errore server: %v", err)
	}
}

// walCheckpointInterval è la cadenza del solo checkpoint/truncate del WAL
// (fase economica, senza incremental_vacuum): tenerla più frequente del ciclo
// di purge giornaliero evita che app.db-wal cresca troppo durante la
// giornata sotto normale traffico di scrittura (note salvate, sync), a
// prescindere da quanti tombstone ci siano da cancellare.
const walCheckpointInterval = 1 * time.Hour

// tombstonePurgeInterval è la cadenza del ciclo "pesante": hard-delete dei
// tombstone scaduti + recupero effettivo dello spazio su disco liberato
// (incremental_vacuum) + checkpoint del WAL.
const tombstonePurgeInterval = 24 * time.Hour

// startTombstonePurgeLoop avvia una goroutine in background con due cicli
// indipendenti:
//
//   - ogni tombstonePurgeInterval (e una volta subito all'avvio, per ripulire
//     eventuale arretrato): rimuove definitivamente dal database le cartelle
//     e le note soft-deleted da più tempo di handlers.TombstoneRetention, poi
//     – solo in modalità locale pura (isLocalDB) e solo se qualcosa è stato
//     effettivamente cancellato – recupera lo spazio liberato sul file .db
//     con un incremental_vacuum, e in ogni caso tenta un wal_checkpoint
//     (TRUNCATE) per tenere sotto controllo anche app.db-wal.
//   - ogni walCheckpointInterval: solo wal_checkpoint(TRUNCATE), per non
//     lasciare che il WAL cresca troppo tra un ciclo di purge e l'altro.
//
// Non blocca mai l'avvio del server: eventuali errori vengono solo loggati,
// il giro successivo riprova.
func startTombstonePurgeLoop(notesRepo *db.NotesRepo, foldersRepo *db.FoldersRepo, sqlDB *sql.DB, isLocalDB bool) {
	purgeAndReclaim := func() {
		cutoff := time.Now().Add(-handlers.TombstoneRetention).UnixMilli()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

		var nDeleted, fDeleted int64
		var err error

		nDeleted, err = notesRepo.PurgeExpiredTombstones(ctx, cutoff)
		if err != nil {
			log.Printf("purge tombstone note fallito: %v", err)
		} else if nDeleted > 0 {
			log.Printf("purge tombstone: rimosse %d note cancellate da oltre %s", nDeleted, handlers.TombstoneRetention)
		}

		fDeleted, err = foldersRepo.PurgeExpiredTombstones(ctx, cutoff)
		if err != nil {
			log.Printf("purge tombstone cartelle fallito: %v", err)
		} else if fDeleted > 0 {
			log.Printf("purge tombstone: rimosse %d cartelle cancellate da oltre %s", fDeleted, handlers.TombstoneRetention)
		}
		cancel()

		maintCtx, maintCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer maintCancel()
		if err := db.RunMaintenance(maintCtx, sqlDB, isLocalDB, nDeleted+fDeleted > 0); err != nil {
			log.Printf("manutenzione DB (incremental_vacuum/wal_checkpoint) fallita, verrà ritentata al prossimo giro: %v", err)
		}
	}

	checkpointOnly := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := db.RunMaintenance(ctx, sqlDB, isLocalDB, false); err != nil {
			log.Printf("wal_checkpoint periodico fallito, verrà ritentato al prossimo giro: %v", err)
		}
	}

	go func() {
		purgeAndReclaim()

		purgeTicker := time.NewTicker(tombstonePurgeInterval)
		defer purgeTicker.Stop()
		checkpointTicker := time.NewTicker(walCheckpointInterval)
		defer checkpointTicker.Stop()

		for {
			select {
			case <-purgeTicker.C:
				purgeAndReclaim()
			case <-checkpointTicker.C:
				checkpointOnly()
			}
		}
	}()
}
