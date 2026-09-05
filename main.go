package main

import (
	"database/sql"
	"log"
	"net/http"
	"os"

	_ "github.com/mattn/go-sqlite3"

	"notes-server/internal/db"
	"notes-server/internal/handlers"
	"notes-server/internal/middleware"
	"notes-server/internal/storage"
)

func main() {
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "./notes.db"
	}

	dataDir := os.Getenv("USERS_DATA_DIR")
	if dataDir == "" {
		dataDir = "./data/users"
	}

	jwtSecret := os.Getenv("JWT_SECRET")
	if jwtSecret == "" {
		jwtSecret = "super-secret-key-change-in-production"
	}

	database, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		log.Fatalf("Impossibile aprire il database: %v", err)
	}
	defer database.Close()

	// Inizializzazione Repository
	usersRepo := db.NewUsersRepository(database)
	notesRepo := db.NewNotesRepository(database)
	settingsRepo := db.NewSettingsRepository(database)

	// Inizializzazione Storage Manager
	storageMgr := storage.NewStorageManager(dataDir)

	// Inizializzazione Handler
	authHandler := handlers.NewAPIAuthHandler(usersRepo, jwtSecret)
	syncHandler := handlers.NewAPISyncHandler(notesRepo, storageMgr)
	settingsHandler := handlers.NewSettingsHandler(settingsRepo)
	adminHandler := handlers.NewAdminHandler(usersRepo, storageMgr)

	// Inizializzazione Middleware
	authMiddleware := middleware.NewAuthMiddleware(jwtSecret)

	mux := http.NewServeMux()

	// --- 1. ROTTE API FLUTTER MOBILE (Inalterate) ---
	mux.HandleFunc("POST /api/v1/auth/login", authHandler.Login)
	mux.HandleFunc("POST /api/v1/auth/register", authHandler.Register)

	// Rotte API protette da JWT per il client Flutter
	mux.Handle("POST /api/v1/sync", authMiddleware.Authenticate(http.HandlerFunc(syncHandler.Sync)))
	mux.Handle("GET /api/v1/user/settings", authMiddleware.Authenticate(http.HandlerFunc(settingsHandler.GetSettings)))
	mux.Handle("POST /api/v1/user/settings", authMiddleware.Authenticate(http.HandlerFunc(settingsHandler.UpdateSettings)))

	// --- 2. ROTTE DASHBOARD ADMIN WEB & GESTIONE UTENTI ---
	mux.HandleFunc("GET /admin/users", adminHandler.RenderUsersPage)
	mux.HandleFunc("GET /api/admin/users", adminHandler.ListUsersAPI)
	mux.HandleFunc("POST /api/admin/users", adminHandler.CreateUserHandler)
	mux.HandleFunc("DELETE /admin/users/{id}", adminHandler.DeleteUserHandler)
	mux.HandleFunc("POST /admin/users/delete", adminHandler.DeleteUserHandler)

	// Serving file statici UI
	fileServer := http.FileServer(http.Dir("./web"))
	mux.Handle("/static/", http.StripPrefix("/static/", fileServer))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Server avviato sulla porta %s...", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf("Errore esecuzione server: %v", err)
	}
}
