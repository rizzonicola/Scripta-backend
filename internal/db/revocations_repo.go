package db

import (
	"context"
	"database/sql"
	"time"
)

// RevocationRepo persiste le revoche dei token JWT, così che sopravvivano al
// riavvio del server (prima la blacklist viveva solo in RAM: dopo un restart
// un token rubato/"disconnesso" tornava valido fino alla scadenza naturale).
//
// Due granularità (vedi lo schema in db.go):
//   - per utente  (user_token_revocations): reset password / cancellazione;
//   - per token   (revoked_tokens): logout esplicito, chiave = claim jti.
type RevocationRepo struct {
	db *sql.DB
}

func NewRevocationRepo(d *sql.DB) *RevocationRepo { return &RevocationRepo{db: d} }

// SaveUserRevocation registra (o avanza) l'istante di revoca di tutti i token
// dell'utente emessi fino a quel momento. Non torna mai indietro nel tempo.
func (r *RevocationRepo) SaveUserRevocation(ctx context.Context, userID string, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO user_token_revocations (user_id, revoked_at) VALUES (?, ?)
		ON CONFLICT(user_id) DO UPDATE SET revoked_at = excluded.revoked_at
		WHERE excluded.revoked_at > user_token_revocations.revoked_at`,
		userID, at.UnixMilli())
	return err
}

// SaveTokenRevocation registra la revoca di un singolo token fino alla sua
// scadenza naturale (dopo di che la riga è inutile e viene potata).
func (r *RevocationRepo) SaveTokenRevocation(ctx context.Context, jti, userID string, expiresAt time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO revoked_tokens (jti, user_id, expires_at) VALUES (?, ?, ?)
		ON CONFLICT(jti) DO NOTHING`,
		jti, userID, expiresAt.UnixMilli())
	return err
}

// LoadActive carica tutte le revoche ancora rilevanti: quelle per utente e
// quelle per token non ancora scaduti. Chiamata una volta all'avvio per
// popolare la cache in RAM usata dal middleware (lookup O(1) per richiesta).
func (r *RevocationRepo) LoadActive(ctx context.Context, now time.Time) (users map[string]time.Time, tokens map[string]time.Time, err error) {
	users = make(map[string]time.Time)
	tokens = make(map[string]time.Time)

	rows, err := r.db.QueryContext(ctx, `SELECT user_id, revoked_at FROM user_token_revocations`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id string
		var ms int64
		if err := rows.Scan(&id, &ms); err != nil {
			rows.Close()
			return nil, nil, err
		}
		users[id] = time.UnixMilli(ms)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, err
	}
	rows.Close()

	rows, err = r.db.QueryContext(ctx, `SELECT jti, expires_at FROM revoked_tokens WHERE expires_at > ?`, now.UnixMilli())
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var jti string
		var ms int64
		if err := rows.Scan(&jti, &ms); err != nil {
			return nil, nil, err
		}
		tokens[jti] = time.UnixMilli(ms)
	}
	return users, tokens, rows.Err()
}

// PurgeExpired elimina le revoche ormai inutili: i token revocati già scaduti
// e le revoche per utente più vecchie di userRevocationRetention (ogni token
// emesso prima di allora è comunque scaduto).
func (r *RevocationRepo) PurgeExpired(ctx context.Context, now time.Time, userRevocationRetention time.Duration) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM revoked_tokens WHERE expires_at <= ?`, now.UnixMilli()); err != nil {
		return err
	}
	_, err := r.db.ExecContext(ctx, `DELETE FROM user_token_revocations WHERE revoked_at < ?`,
		now.Add(-userRevocationRetention).UnixMilli())
	return err
}
