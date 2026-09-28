package main

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"notes-server/internal/auth"
	"notes-server/internal/db"
	"notes-server/internal/handlers"
	"notes-server/internal/middleware"
)

//go:embed web/templates/*.html
var templatesFS embed.FS

// serverVersion è riportata da /healthz (e verificata dai test).
const serverVersion = "2.1.0"

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
// credenziali) su /admin/login, applicando al solo POST il rate limiting e il
// controllo same-origin.
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

// ---------------------------------------------------------------------------
// Configurazione
// ---------------------------------------------------------------------------

type config struct {
	DBPath          string
	JWTSecret       string
	AdminUser       string
	AdminPass       string
	Port            string
	JWTTTL          time.Duration
	AdminSessionTTL time.Duration
	TrustedProxies  []string
	SyncMaxBody     int64

	TursoURL      string
	TursoToken    string
	TursoInterval time.Duration
}

// Segreti di esempio/placeholder che NON devono mai essere accettati.
var insecureValues = []string{"admin", "password", "changeme", "change-me", "cambiami", "secret", "change-me-in-production-please"}

func looksInsecure(v string) bool {
	l := strings.ToLower(strings.TrimSpace(v))
	for _, bad := range insecureValues {
		if l == bad || strings.HasPrefix(l, bad+"-") || strings.HasPrefix(l, bad+"_") {
			return true
		}
	}
	return false
}

const (
	minJWTSecretLen = 32
	minAdminPassLen = 12
)

// loadConfig legge e VALIDA la configurazione. Non esiste più alcun valore
// di default per i segreti: JWT_SECRET e ADMIN_PASS sono obbligatori e un
// valore mancante, di esempio o troppo corto blocca l'avvio con un errore
// esplicito (prima il server partiva con JWT_SECRET pubblico e ADMIN_PASS=
// admin, permettendo a chiunque di forgiare token o entrare nella dashboard).
func loadConfig() (config, error) {
	c := config{
		DBPath:          getEnv("DB_PATH", "data/app.db"),
		JWTSecret:       os.Getenv("JWT_SECRET"),
		AdminUser:       os.Getenv("ADMIN_USER"),
		AdminPass:       os.Getenv("ADMIN_PASS"),
		Port:            getEnv("PORT", "8080"),
		JWTTTL:          24 * time.Hour,
		AdminSessionTTL: 8 * time.Hour,
		TrustedProxies:  middleware.ParseTrustedProxies(os.Getenv("TRUSTED_PROXIES")),
		TursoURL:        os.Getenv("TURSO_SYNC_URL"),
		TursoToken:      os.Getenv("TURSO_AUTH_TOKEN"),
	}

	var problems []string
	if c.JWTSecret == "" {
		problems = append(problems, "JWT_SECRET non impostato")
	} else if looksInsecure(c.JWTSecret) || len(c.JWTSecret) < minJWTSecretLen {
		problems = append(problems, fmt.Sprintf("JWT_SECRET troppo corto (<%d caratteri) o di esempio", minJWTSecretLen))
	}
	if c.AdminUser == "" {
		problems = append(problems, "ADMIN_USER non impostato")
	}
	if c.AdminPass == "" {
		problems = append(problems, "ADMIN_PASS non impostato")
	} else if looksInsecure(c.AdminPass) || len(c.AdminPass) < minAdminPassLen {
		problems = append(problems, fmt.Sprintf("ADMIN_PASS troppo corta (<%d caratteri) o di esempio", minAdminPassLen))
	}
	if len(problems) > 0 {
		return c, errors.New("configurazione non valida: " + strings.Join(problems, "; ") +
			". Generare un segreto con: openssl rand -base64 48")
	}

	parseDur := func(key string, dst *time.Duration) {
		if raw := os.Getenv(key); raw != "" {
			if d, err := time.ParseDuration(raw); err == nil && d > 0 {
				*dst = d
			} else {
				log.Printf("%s non valido (%q), uso il default (%s)", key, raw, *dst)
			}
		}
	}
	parseDur("JWT_TTL", &c.JWTTTL)
	parseDur("ADMIN_SESSION_TTL", &c.AdminSessionTTL)
	parseDur("TURSO_SYNC_INTERVAL", &c.TursoInterval)

	if raw := os.Getenv("SYNC_MAX_BODY_BYTES"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
			c.SyncMaxBody = n
		} else {
			log.Printf("SYNC_MAX_BODY_BYTES non valido (%q), uso il default", raw)
		}
	}
	return c, nil
}

// healthHandler risponde a /health e /healthz.
func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok","name":"Scripta Notes Server","version":"` + serverVersion + `","architecture":"local-first delta-sync (id-based, last-write-wins, server-stamped cursor)","license":"GPL-3.0","credits":{"database":"github.com/tursodatabase/go-libsql","jwt":"github.com/golang-jwt/jwt/v5","security":"golang.org/x/crypto/bcrypt","uuid":"github.com/google/uuid"}}` + "\n"))
}

// buildRouter assembla l'intero handler HTTP (rotte + middleware). È separato
// da main() per poter essere esercitato dai test di integrazione (main_test.go)
// con un database temporaneo, esattamente come in produzione.
func buildRouter(cfg config, sqlDB *sql.DB, tm *auth.TokenManager) (http.Handler, error) {
	usersRepo := db.NewUsersRepo(sqlDB)
	notesRepo := db.NewNotesRepo(sqlDB)
	settingsRepo := db.NewSettingsRepo(sqlDB)

	adminSessions := auth.NewAdminSessionManager(cfg.JWTSecret, cfg.AdminSessionTTL)

	adminHandler, err := handlers.NewAdminHandler(usersRepo, tm, adminSessions, cfg.AdminUser, cfg.AdminPass, templatesFS)
	if err != nil {
		return nil, fmt.Errorf("caricamento template admin: %w", err)
	}
	authHandler := handlers.NewAuthHandler(usersRepo, tm)
	syncHandler := handlers.NewSyncHandler(sqlDB)
	if cfg.SyncMaxBody > 0 {
		syncHandler.MaxBodyBytes = cfg.SyncMaxBody
	}
	notesHandler := handlers.NewNotesHandler(notesRepo)
	settingsHandler := handlers.NewSettingsHandler(settingsRepo)

	// Rate limiting: la chiave è l'IP del peer TCP; Cf-Connecting-IP /
	// X-Forwarded-For sono creduti SOLO se il peer è in TRUSTED_PROXIES
	// (vedi middleware.ClientIPResolver). Il WAF Cloudflare resta la prima
	// linea di difesa; questa è la seconda.
	ips, err := middleware.NewClientIPResolver(cfg.TrustedProxies)
	if err != nil {
		return nil, err
	}
	loginRateLimit := middleware.RateLimit(0.5, 5, ips)
	sameOrigin := middleware.RequireSameOrigin

	mux := http.NewServeMux()

	// --- Dashboard Admin (login form-based + cookie di sessione) ---
	adminAuth := middleware.SessionAuthAdmin(adminSessions)
	mux.HandleFunc("/admin/login", adminLoginDispatch(adminHandler, chain(loginRateLimit, sameOrigin)))
	mux.Handle("/admin/logout", adminAuth(sameOrigin(http.HandlerFunc(adminHandler.Logout))))
	mux.Handle("/admin", adminAuth(http.HandlerFunc(adminHandler.UsersPage)))
	mux.Handle("/admin/users/create", adminAuth(sameOrigin(http.HandlerFunc(adminHandler.CreateUser))))
	mux.Handle("/admin/users/reset-password", adminAuth(sameOrigin(http.HandlerFunc(adminHandler.ResetPassword))))
	mux.Handle("/admin/users/delete", adminAuth(sameOrigin(http.HandlerFunc(adminHandler.DeleteUser))))

	// --- API pubbliche ---
	mux.Handle("/api/v1/auth/login", loginRateLimit(http.HandlerFunc(authHandler.Login)))

	// --- API protette da JWT ---
	requireJWT := middleware.RequireJWT(tm)
	mux.Handle("/api/v1/auth/logout", requireJWT(http.HandlerFunc(authHandler.Logout)))
	mux.Handle("/api/v1/sync", requireJWT(http.HandlerFunc(syncHandler.Sync)))
	mux.Handle("/api/v1/notes/download", requireJWT(http.HandlerFunc(notesHandler.DownloadMarkdown)))
	mux.Handle("/api/v1/user/settings", requireJWT(http.HandlerFunc(settingsDispatch(settingsHandler))))

	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/health", healthHandler)

	return middleware.SecurityHeaders(middleware.Gzip(mux)), nil
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("avvio bloccato: %v", err)
	}

	sqlDB, err := db.OpenWithConfig(db.Config{
		Path:         cfg.DBPath,
		PrimaryURL:   cfg.TursoURL,
		AuthToken:    cfg.TursoToken,
		SyncInterval: cfg.TursoInterval,
	})
	if err != nil {
		log.Fatalf("errore apertura database: %v", err)
	}
	defer sqlDB.Close()

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Revoca token persistente: le revoche ancora rilevanti vengono
	// ricaricate dal DB, quindi logout/reset password/cancellazione utente
	// restano validi anche dopo un riavvio.
	tokenManager := auth.NewTokenManager(cfg.JWTSecret, cfg.JWTTTL)
	if err := tokenManager.AttachStore(rootCtx, db.NewRevocationRepo(sqlDB)); err != nil {
		log.Fatalf("caricamento revoche token: %v", err)
	}

	handler, err := buildRouter(cfg, sqlDB, tokenManager)
	if err != nil {
		log.Fatalf("errore inizializzazione server: %v", err)
	}

	isLocalDB := cfg.TursoURL == ""
	startTombstonePurgeLoop(rootCtx, tokenManager, db.NewNotesRepo(sqlDB), db.NewFoldersRepo(sqlDB), sqlDB, isLocalDB)

	// Timeout espliciti: senza di essi un client lento (slowloris) può
	// tenere aperte connessioni all'infinito. ReadTimeout/WriteTimeout sono
	// generosi perché un batch di sync può pesare decine di MiB.
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 16,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Printf("server in ascolto su %s (admin: /admin)", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	// Graceful shutdown: su SIGINT/SIGTERM smette di accettare connessioni,
	// attende (max 30s) il completamento delle richieste in corso (in
	// particolare le transazioni di sync), poi chiude il DB (defer sopra).
	select {
	case err := <-serveErr:
		if err != nil {
			log.Fatalf("errore server: %v", err)
		}
	case <-rootCtx.Done():
		log.Println("segnale di arresto ricevuto, chiusura ordinata in corso...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown non completato entro il timeout: %v", err)
			_ = srv.Close()
		}
		log.Println("server arrestato")
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
//     e le note il cui tombstone è stato accettato dal server (synced_at) da
//     più di handlers.TombstoneRetention, pota le revoche JWT scadute, poi
//     – solo in modalità locale pura (isLocalDB) e solo se qualcosa è stato
//     effettivamente cancellato – recupera lo spazio liberato sul file .db
//     con un incremental_vacuum, e in ogni caso tenta un wal_checkpoint
//     (TRUNCATE) per tenere sotto controllo anche app.db-wal.
//   - ogni walCheckpointInterval: solo wal_checkpoint(TRUNCATE), per non
//     lasciare che il WAL cresca troppo tra un ciclo di purge e l'altro.
//
// Si ferma quando ctx viene annullato (graceful shutdown). Non blocca mai
// l'avvio del server: eventuali errori vengono solo loggati, il giro
// successivo riprova.
func startTombstonePurgeLoop(ctx context.Context, tm *auth.TokenManager, notesRepo *db.NotesRepo, foldersRepo *db.FoldersRepo, sqlDB *sql.DB, isLocalDB bool) {
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
		tm.PurgeExpired(ctx)

		purgeTicker := time.NewTicker(tombstonePurgeInterval)
		defer purgeTicker.Stop()
		checkpointTicker := time.NewTicker(walCheckpointInterval)
		defer checkpointTicker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-purgeTicker.C:
				purgeAndReclaim()
				tm.PurgeExpired(ctx)
			case <-checkpointTicker.C:
				checkpointOnly()
			}
		}
	}()
}
