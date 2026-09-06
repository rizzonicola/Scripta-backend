package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"notes-server/internal/models"
)

type SettingsRepo struct {
	db *sql.DB
}

func NewSettingsRepo(db *sql.DB) *SettingsRepo {
	return &SettingsRepo{db: db}
}

// Get recupera le impostazioni salvate di un utente. Se l'utente non ha mai
// salvato preferenze personalizzate, restituisce i valori di default
// (senza scrivere nulla nel DB), così GET non ha effetti collaterali.
func (r *SettingsRepo) Get(ctx context.Context, userID string) (models.UserSettings, error) {
	var raw string
	var updatedAt int64
	err := r.db.QueryRowContext(ctx,
		`SELECT settings_json, updated_at FROM user_settings WHERE user_id = ?`, userID,
	).Scan(&raw, &updatedAt)

	if err == sql.ErrNoRows {
		return models.DefaultUserSettings(), nil
	}
	if err != nil {
		return models.UserSettings{}, err
	}

	var settings models.UserSettings
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return models.UserSettings{}, err
	}
	settings.UpdatedAt = updatedAt
	return settings, nil
}

// Upsert salva (crea o sovrascrive) le impostazioni di un utente come JSON.
func (r *SettingsRepo) Upsert(ctx context.Context, userID string, settings models.UserSettings) (models.UserSettings, error) {
	settings.UpdatedAt = time.Now().UnixMilli()

	raw, err := json.Marshal(settings)
	if err != nil {
		return models.UserSettings{}, err
	}

	_, err = r.db.ExecContext(ctx, `
		INSERT INTO user_settings (user_id, settings_json, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			settings_json = excluded.settings_json,
			updated_at    = excluded.updated_at
	`, userID, string(raw), settings.UpdatedAt)
	if err != nil {
		return models.UserSettings{}, err
	}

	return settings, nil
}
