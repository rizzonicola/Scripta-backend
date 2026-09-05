package db

import (
	"database/sql"
	"fmt"
)

type User struct {
	ID           int64  `json:"id"`
	Username     string `json:"username"`
	PasswordHash string `json:"-"`
	IsAdmin      bool   `json:"is_admin"`
	CreatedAt    string `json:"created_at"`
}

type UsersRepository struct {
	db *sql.DB
}

func NewUsersRepository(db *sql.DB) *UsersRepository {
	return &UsersRepository{db: db}
}

func (r *UsersRepository) GetByID(id int64) (*User, error) {
	u := &User{}
	query := `SELECT id, username, password_hash, is_admin, created_at FROM users WHERE id = ?`
	err := r.db.QueryRow(query, id).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.IsAdmin, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (r *UsersRepository) GetByUsername(username string) (*User, error) {
	u := &User{}
	query := `SELECT id, username, password_hash, is_admin, created_at FROM users WHERE username = ?`
	err := r.db.QueryRow(query, username).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.IsAdmin, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (r *UsersRepository) GetAll() ([]User, error) {
	query := `SELECT id, username, is_admin, created_at FROM users ORDER BY id ASC`
	rows, err := r.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.IsAdmin, &u.CreatedAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, nil
}

func (r *UsersRepository) Create(username, passwordHash string, isAdmin bool) (*User, error) {
	query := `INSERT INTO users (username, password_hash, is_admin) VALUES (?, ?, ?)`
	res, err := r.db.Exec(query, username, passwordHash, isAdmin)
	if err != nil {
		return nil, err
	}

	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}

	return r.GetByID(id)
}

func (r *UsersRepository) DeleteUser(id int64) error {
	tx, err := r.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Eliminazione dei dati associati nelle tabelle correlate
	_, _ = tx.Exec(`DELETE FROM notes WHERE user_id = ?`, id)
	_, _ = tx.Exec(`DELETE FROM user_settings WHERE user_id = ?`, id)
	_, _ = tx.Exec(`DELETE FROM tokens WHERE user_id = ?`, id)
	_, _ = tx.Exec(`DELETE FROM sessions WHERE user_id = ?`, id)

	res, err := tx.Exec(`DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("failed to delete user record: %w", err)
	}

	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return sql.ErrNoRows
	}

	return tx.Commit()
}
