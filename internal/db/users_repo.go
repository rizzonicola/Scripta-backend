package db

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"

	"notes-server/internal/models"
)

type UsersRepo struct {
	db *sql.DB
}

func NewUsersRepo(db *sql.DB) *UsersRepo {
	return &UsersRepo{db: db}
}

// Create inserisce un nuovo utente con la password già cifrata (bcrypt hash).
func (r *UsersRepo) Create(ctx context.Context, username, passwordHash string) (*models.User, error) {
	u := &models.User{
		ID:           uuid.NewString(),
		Username:     username,
		PasswordHash: passwordHash,
		CreatedAt:    time.Now().UnixMilli(),
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO users (id, username, password_hash, created_at) VALUES (?, ?, ?, ?)`,
		u.ID, u.Username, u.PasswordHash, u.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return u, nil
}

// List restituisce tutti gli utenti (senza esporre l'hash della password nella UI, se non necessario).
func (r *UsersRepo) List(ctx context.Context) ([]models.User, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, username, password_hash, created_at FROM users ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []models.User
	for rows.Next() {
		var u models.User
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// GetByUsername recupera un utente per username (usato dal login).
func (r *UsersRepo) GetByUsername(ctx context.Context, username string) (*models.User, error) {
	var u models.User
	err := r.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, created_at FROM users WHERE username = ?`, username,
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
