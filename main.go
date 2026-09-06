package main

import (
	"context"
	"embed"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	// Import anonimi per registrare i driver del database (SQLite / LibSQL)
	//_ "github.com/mattn/go-sqlite3"
	//_ "github.com/tursodatabase/go-libsql"

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
	tokenManager := auth.NewTokenManager(jwtSecret, 7*24*time.Hour)

	// --- Handlers ---
	// Nessuno storage su filesystem da inizializzare: cartelle e note vivono
	// interamente nel database (schema ID-based in internal/db/db.go).
	adminHandler, err := handlers.NewAdminHandler(usersRepo, templatesFS)
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

	// --- Purge periodico dei tombstone scaduti ---
	// Una cancellazione (soft-delete) resta visibile ai client tramite la
	// pull della sync per handlers.TombstoneRetention, poi viene rimossa
	// definitivamente dal database: questo mantiene le tabelle folders/notes
	// libere da tombstone ormai propagati a tutti i dispositivi, senza dover
	// tracciare esplicitamente quali device abbiano già fatto pull di quale
	// tombstone (complessità non necessaria per il caso d'uso di Scripta).
	startTombstonePurgeLoop(notesRepo, db.NewFoldersRepo(sqlDB))

	mux := http.NewServeMux()

	// --- Dashboard Admin (protetta con Basic Auth) ---
	adminAuth := middleware.BasicAuthAdmin(adminUser, adminPass)
	mux.Handle("/admin", adminAuth(http.HandlerFunc(adminHandler.UsersPage)))
	mux.Handle("/admin/users/create", adminAuth(http.HandlerFunc(adminHandler.CreateUser)))
	mux.Handle("/admin/users/reset-password", adminAuth(http.HandlerFunc(adminHandler.ResetPassword)))
	mux.Handle("/admin/users/delete", adminAuth(http.HandlerFunc(adminHandler.DeleteUser)))

	// --- API pubbliche (mobile app) ---
	mux.HandleFunc("/api/v1/auth/login", authHandler.Login)

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

	addr := ":" + port
	log.Printf("server in ascolto su %s (admin: http://localhost%s/admin)", addr, addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("errore server: %v", err)
	}
}

// startTombstonePurgeLoop avvia una goroutine in background che, una volta
// al giorno (e una volta subito all'avvio, per ripulire eventuale arretrato),
// rimuove definitivamente dal database le cartelle e le note soft-deleted da
// più tempo di handlers.TombstoneRetention. Non blocca mai l'avvio del
// server: eventuali errori vengono solo loggati, il prossimo giro riprova.
func startTombstonePurgeLoop(notesRepo *db.NotesRepo, foldersRepo *db.FoldersRepo) {
	runOnce := func() {
		cutoff := time.Now().Add(-handlers.TombstoneRetention).UnixMilli()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		nDeleted, err := notesRepo.PurgeExpiredTombstones(ctx, cutoff)
		if err != nil {
			log.Printf("purge tombstone note fallito: %v", err)
		} else if nDeleted > 0 {
			log.Printf("purge tombstone: rimosse %d note cancellate da oltre %s", nDeleted, handlers.TombstoneRetention)
		}

		fDeleted, err := foldersRepo.PurgeExpiredTombstones(ctx, cutoff)
		if err != nil {
			log.Printf("purge tombstone cartelle fallito: %v", err)
		} else if fDeleted > 0 {
			log.Printf("purge tombstone: rimosse %d cartelle cancellate da oltre %s", fDeleted, handlers.TombstoneRetention)
		}
	}

	go func() {
		runOnce()
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			runOnce()
		}
	}()
}
