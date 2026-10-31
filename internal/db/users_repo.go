package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
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
		ID:              uuid.NewString(),
		Username:        username,
		PasswordHash:    passwordHash,
		PasswordVersion: 1,
		CreatedAt:       time.Now().UnixMilli(),
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

// List restituisce gli utenti per la dashboard admin, eventualmente filtrati
// per username o ID. Non legge password_hash: l'hash serve solo al login
// (GetByUsername), non alla UI, quindi evitare di caricarlo in memoria riduce
// l'esposizione accidentale.
func (r *UsersRepo) List(ctx context.Context, search string) ([]models.User, error) {
	query := `SELECT id, username, created_at, blocked_at, rate_limit_exempt FROM users`
	args := make([]any, 0, 2)
	search = strings.TrimSpace(search)
	if search != "" {
		pattern := "%" + escapeLike(search) + "%"
		query += ` WHERE username LIKE ? ESCAPE '\' OR id LIKE ? ESCAPE '\'`
		args = append(args, pattern, pattern)
	}
	query += ` ORDER BY created_at DESC`

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []models.User
	for rows.Next() {
		var u models.User
		var blockedAt sql.NullInt64
		var exempt int
		if err := rows.Scan(&u.ID, &u.Username, &u.CreatedAt, &blockedAt, &exempt); err != nil {
			return nil, err
		}
		if blockedAt.Valid {
			u.BlockedAt = &blockedAt.Int64
		}
		u.RateLimitExempt = exempt != 0
		users = append(users, u)
	}
	return users, rows.Err()
}

// escapeLike neutralizza i metacaratteri di LIKE, così la ricerca admin cerca
// il testo letterale anche quando contiene % o _.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// GetByUsername recupera un utente per username con confronto esatto
// (usato dal login e dal controllo duplicati). "Mario" e "mario" sono due
// account distinti: il case è parte dell'identità.
func (r *UsersRepo) GetByUsername(ctx context.Context, username string) (*models.User, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, password_version, created_at, blocked_at, rate_limit_exempt
		 FROM users WHERE username = ? COLLATE BINARY`, username,
	)
	u, err := scanUserRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return u, err
}

// GetByID recupera un utente per ID.
func (r *UsersRepo) GetByID(ctx context.Context, id string) (*models.User, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, password_version, created_at, blocked_at, rate_limit_exempt
		 FROM users WHERE id = ?`, id,
	)
	u, err := scanUserRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return u, err
}

// AccountStatus è il sottoinsieme di users necessario a ogni richiesta
// autenticata. È separato da GetByID perché il middleware non deve leggere
// l'hash della password per ogni sync.
type AccountStatus struct {
	Blocked         bool
	RateLimitExempt bool
	// PasswordVersion è la versione corrente dell'hash password. Il middleware
	// la confronta con quella nel JWT: un token emesso prima di un reset
	// password (o da un login concorrente che aveva letto l'hash vecchio) non
	// è più accettato, anche se il suo iat è successivo alla revoca.
	PasswordVersion int64
}

// GetAccountStatus legge lo stato di sicurezza dell'utente. found=false
// significa che l'account è stato eliminato.
func (r *UsersRepo) GetAccountStatus(ctx context.Context, id string) (status AccountStatus, found bool, err error) {
	var blockedAt sql.NullInt64
	var exempt int
	var passwordVersion int64
	err = r.db.QueryRowContext(ctx,
		`SELECT blocked_at, rate_limit_exempt, password_version FROM users WHERE id = ?`, id,
	).Scan(&blockedAt, &exempt, &passwordVersion)
	if err == sql.ErrNoRows {
		return AccountStatus{}, false, nil
	}
	if err != nil {
		return AccountStatus{}, false, err
	}
	return AccountStatus{
		Blocked:         blockedAt.Valid,
		RateLimitExempt: exempt != 0,
		PasswordVersion: passwordVersion,
	}, true, nil
}

// SetBlocked blocca o sblocca un account. Il timestamp di blocco viene
// conservato solo come informazione operativa: il middleware controlla la
// sola presenza (blocked_at IS NOT NULL).
func (r *UsersRepo) SetBlocked(ctx context.Context, userID string, blocked bool) error {
	var res sql.Result
	var err error
	if blocked {
		res, err = r.db.ExecContext(ctx,
			`UPDATE users SET blocked_at = ? WHERE id = ?`, time.Now().UnixMilli(), userID)
	} else {
		res, err = r.db.ExecContext(ctx,
			`UPDATE users SET blocked_at = NULL WHERE id = ?`, userID)
	}
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

// SetRateLimitExempt attiva/disattiva l'esenzione dal rate limit per-utente.
func (r *UsersRepo) SetRateLimitExempt(ctx context.Context, userID string, exempt bool) error {
	value := 0
	if exempt {
		value = 1
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE users SET rate_limit_exempt = ? WHERE id = ?`, value, userID)
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

// scanUserRow legge le colonne comuni a List/GetByUsername/GetByID.
func scanUserRow(row interface {
	Scan(dest ...any) error
}) (*models.User, error) {
	var u models.User
	var blockedAt sql.NullInt64
	var exempt int
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.PasswordVersion, &u.CreatedAt, &blockedAt, &exempt); err != nil {
		return nil, err
	}
	if blockedAt.Valid {
		u.BlockedAt = &blockedAt.Int64
	}
	u.RateLimitExempt = exempt != 0
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
// password_version viene incrementata nella stessa UPDATE: è l'epoch che
// invalida i token emessi da un login che aveva letto l'hash precedente.
func (r *UsersRepo) UpdatePassword(ctx context.Context, userID, newPasswordHash string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, password_version = password_version + 1 WHERE id = ?`,
		newPasswordHash, userID)
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
