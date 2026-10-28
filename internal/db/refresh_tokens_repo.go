package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrRefreshTokenInactive indica che il token da ruotare non è più attivo:
// è già stato usato, revocato o è scaduto. Il chiamante deve considerarlo
// un tentativo di riuso e revocare l'intera famiglia.
var ErrRefreshTokenInactive = errors.New("refresh token non attivo")

// RefreshToken è una riga di refresh_tokens. TokenHash è l'hash SHA-256 del
// valore consegnato al client; il valore in chiaro non viene mai persistito.
type RefreshToken struct {
	ID         string
	UserID     string
	FamilyID   string
	TokenHash  string
	IssuedAt   int64
	ExpiresAt  int64
	RevokedAt  *int64
	ReplacedBy *string
}

type RefreshTokensRepo struct {
	db *sql.DB
}

func NewRefreshTokensRepo(d *sql.DB) *RefreshTokensRepo {
	return &RefreshTokensRepo{db: d}
}

// Create inserisce un nuovo refresh token (login o rotazione).
func (r *RefreshTokensRepo) Create(ctx context.Context, rt RefreshToken) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO refresh_tokens
			(id, user_id, family_id, token_hash, issued_at, expires_at, revoked_at, replaced_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		rt.ID, rt.UserID, rt.FamilyID, rt.TokenHash, rt.IssuedAt, rt.ExpiresAt, rt.RevokedAt, rt.ReplacedBy,
	)
	return err
}

// GetByHash cerca un refresh token per hash. Restituisce nil se non esiste.
func (r *RefreshTokensRepo) GetByHash(ctx context.Context, hash string) (*RefreshToken, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, user_id, family_id, token_hash, issued_at, expires_at, revoked_at, replaced_by
		FROM refresh_tokens WHERE token_hash = ?`, hash,
	)
	var rt RefreshToken
	var revokedAt sql.NullInt64
	var replacedBy sql.NullString
	err := row.Scan(&rt.ID, &rt.UserID, &rt.FamilyID, &rt.TokenHash, &rt.IssuedAt, &rt.ExpiresAt, &revokedAt, &replacedBy)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if revokedAt.Valid {
		rt.RevokedAt = &revokedAt.Int64
	}
	if replacedBy.Valid {
		rt.ReplacedBy = &replacedBy.String
	}
	return &rt, nil
}

// Rotate invalida il token oldID e inserisce il sostituto nella stessa
// famiglia, in un'unica transazione. L'UPDATE condizionale è la barriera
// contro due refresh concorrenti: solo uno trova la riga ancora attiva.
func (r *RefreshTokensRepo) Rotate(ctx context.Context, oldID string, next RefreshToken, now time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE refresh_tokens
		SET revoked_at = ?, replaced_by = ?
		WHERE id = ? AND revoked_at IS NULL AND expires_at > ?`,
		now.UnixMilli(), next.ID, oldID, now.UnixMilli(),
	)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrRefreshTokenInactive
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO refresh_tokens
			(id, user_id, family_id, token_hash, issued_at, expires_at, revoked_at, replaced_by)
		VALUES (?, ?, ?, ?, ?, ?, NULL, NULL)`,
		next.ID, next.UserID, next.FamilyID, next.TokenHash, next.IssuedAt, next.ExpiresAt,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// RevokeFamily invalida tutti i token ancora attivi di una famiglia.
func (r *RefreshTokensRepo) RevokeFamily(ctx context.Context, familyID string, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE refresh_tokens SET revoked_at = ?
		WHERE family_id = ? AND revoked_at IS NULL`, at.UnixMilli(), familyID)
	return err
}

// RevokeAllForUser invalida tutte le famiglie dell'utente (logout-all,
// reset password, blocco, cancellazione).
func (r *RefreshTokensRepo) RevokeAllForUser(ctx context.Context, userID string, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE refresh_tokens SET revoked_at = ?
		WHERE user_id = ? AND revoked_at IS NULL`, at.UnixMilli(), userID)
	return err
}

// PurgeExpired elimina i refresh token scaduti: non possono più essere usati
// e, passata la scadenza, non servono nemmeno per il rilevamento del riuso.
func (r *RefreshTokensRepo) PurgeExpired(ctx context.Context, now time.Time) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM refresh_tokens WHERE expires_at <= ?`, now.UnixMilli())
	return err
}
