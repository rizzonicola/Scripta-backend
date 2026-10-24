package db

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"

	"notes-server/internal/models"
)

// ErrUsernameTaken: esiste già un utente con lo stesso username (confronto
// case-sensitive: "Mario" e "mario" sono account distinti).
var ErrUsernameTaken = errors.New("username già in uso")

type UsersRepo struct {
	db *sql.DB
}

func NewUsersRepo(db *sql.DB) *UsersRepo {
	return &UsersRepo{db: db}
}

// Create inserisce un nuovo utente con la password già cifrata (bcrypt hash).
// L'username va già normalizzato dal chiamante (auth.NormalizeUsername); qui
// si verifica comunque l'assenza di duplicati con confronto esatto.
func (r *UsersRepo) Create(ctx context.Context, username, passwordHash string) (*models.User, error) {
	existing, err := r.GetByUsername(ctx, username)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, ErrUsernameTaken
	}
	u := &models.User{
		ID:           uuid.NewString(),
		Username:     username,
		PasswordHash: passwordHash,
		CreatedAt:    time.Now().UnixMilli(),
	}
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO users (id, username, password_hash, created_at) VALUES (?, ?, ?, ?)`,
		u.ID, u.Username, u.PasswordHash, u.CreatedAt,
	)
	if err != nil {
		// Il pre-check sopra non è atomico: due richieste concorrenti possono
		// superarlo entrambe e una sola INSERT vince l'indice univoco. In quel
		// caso restituiamo lo stesso ErrUsernameTaken del percorso normale,
		// invece dell'errore grezzo del driver (che la dashboard mostrerebbe
		// come "creazione fallita" senza spiegare il motivo).
		if existing, getErr := r.GetByUsername(ctx, username); getErr == nil && existing != nil {
			return nil, ErrUsernameTaken
		}
		return nil, err
	}
	return u, nil
}

// List restituisce tutti gli utenti per la dashboard admin. Non legge
// password_hash: l'hash serve solo al login (GetByUsername), non alla UI,
// quindi evitare di caricarlo in memoria riduce l'esposizione accidentale.
func (r *UsersRepo) List(ctx context.Context) ([]models.User, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, username, created_at FROM users ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []models.User
	for rows.Next() {
		var u models.User
		if err := rows.Scan(&u.ID, &u.Username, &u.CreatedAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// GetByUsername recupera un utente per username con confronto esatto
// (usato dal login e dal controllo duplicati). "Mario" e "mario" sono due
// account distinti: il case è parte dell'identità.
func (r *UsersRepo) GetByUsername(ctx context.Context, username string) (*models.User, error) {
	var u models.User
	err := r.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, created_at FROM users WHERE username = ? COLLATE BINARY`, username,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetByID recupera un utente per ID.
func (r *UsersRepo) GetByID(ctx context.Context, id string) (*models.User, error) {
	var u models.User
	err := r.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, created_at FROM users WHERE id = ?`, id,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// Delete rimuove definitivamente un utente dal database. Grazie ai vincoli
// "ON DELETE CASCADE" definiti sulle tabelle folders, notes e user_settings
// (vedi internal/db/db.go, sempre con PRAGMA foreign_keys=ON attivo su ogni
// connessione), questa singola DELETE elimina in cascata anche tutte le
// cartelle, il contenuto di tutte le note (che vive nel DB, non più su
// filesystem) e le impostazioni dell'utente: è l'UNICA operazione
// necessaria per una cancellazione account completa e atomica, senza alcun
// residuo su disco da ripulire separatamente.
func (r *UsersRepo) Delete(ctx context.Context, userID string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, userID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UpdatePassword sovrascrive l'hash della password di un utente esistente (reset password).
// La vecchia password non è mai leggibile: viene semplicemente sostituito l'hash.
func (r *UsersRepo) UpdatePassword(ctx context.Context, userID, newPasswordHash string) error {
	res, err := r.db.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, newPasswordHash, userID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
