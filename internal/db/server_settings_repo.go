package db

import (
	"context"
	"database/sql"
	"time"
)

// ServerSettings sono le impostazioni globali modificabili dalla dashboard
// admin. La tabella ha una sola riga (id = 1) creata dalla migrazione.
type ServerSettings struct {
	UserRateLimitEnabled bool
	UpdatedAt            int64
}

type ServerSettingsRepo struct {
	db *sql.DB
}

func NewServerSettingsRepo(db *sql.DB) *ServerSettingsRepo {
	return &ServerSettingsRepo{db: db}
}

// Get legge le impostazioni globali. Se la riga mancasse (database molto
// vecchio o manomesso) restituisce il default sicuro: rate limit attivo.
func (r *ServerSettingsRepo) Get(ctx context.Context) (ServerSettings, error) {
	var enabled int
	var updatedAt int64
	err := r.db.QueryRowContext(ctx,
		`SELECT user_rate_limit_enabled, updated_at FROM server_settings WHERE id = 1`,
	).Scan(&enabled, &updatedAt)
	if err == sql.ErrNoRows {
		return ServerSettings{UserRateLimitEnabled: true}, nil
	}
	if err != nil {
		return ServerSettings{}, err
	}
	return ServerSettings{UserRateLimitEnabled: enabled != 0, UpdatedAt: updatedAt}, nil
}

// SetUserRateLimitEnabled attiva/disattiva il limite per-utente della sync.
func (r *ServerSettingsRepo) SetUserRateLimitEnabled(ctx context.Context, enabled bool) error {
	value := 0
	if enabled {
		value = 1
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO server_settings (id, user_rate_limit_enabled, updated_at)
		 VALUES (1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   user_rate_limit_enabled = excluded.user_rate_limit_enabled,
		   updated_at = excluded.updated_at`,
		value, time.Now().UnixMilli(),
	)
	return err
}
