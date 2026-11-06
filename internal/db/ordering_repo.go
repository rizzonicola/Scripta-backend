package db

import (
	"context"
	"database/sql"
	"encoding/json"
)

// OrderingRepo conserva il blob di posizionamento dell'account. Il server non
// interpreta il JSON: lo tratta come uno stato opaco e lo restituisce al
// client. Version implementa il controllo ottimistico: un push costruito su
// una versione superata viene ignorato (il client riceve lo stato corrente).
type OrderingRepo struct {
	db execer
}

func NewOrderingRepo(d execer) *OrderingRepo {
	return &OrderingRepo{db: d}
}

// Get restituisce versione e blob correnti. Un account che non ha mai
// modificato il posizionamento parte da versione 0 e blob vuoto.
func (r *OrderingRepo) Get(ctx context.Context, userID string) (int64, json.RawMessage, error) {
	var version int64
	var raw string
	err := r.db.QueryRowContext(ctx,
		`SELECT version, ordering_json FROM user_ordering WHERE user_id = ?`, userID,
	).Scan(&version, &raw)
	if err == sql.ErrNoRows {
		return 0, json.RawMessage("{}"), nil
	}
	if err != nil {
		return 0, nil, err
	}
	if !json.Valid([]byte(raw)) {
		raw = "{}"
	}
	return version, json.RawMessage(raw), nil
}

// Apply scrive data solo se baseVersion è ancora quella corrente. Il chiamante
// legge comunque lo stato autoritativo con Get: qui basta sapere se il push è
// stato accettato.
func (r *OrderingRepo) Apply(ctx context.Context, userID string, baseVersion int64, data json.RawMessage) (bool, error) {
	version, _, err := r.Get(ctx, userID)
	if err != nil {
		return false, err
	}
	if baseVersion != version {
		return false, nil
	}
	next := version + 1
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO user_ordering (user_id, ordering_json, version) VALUES (?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			ordering_json = excluded.ordering_json,
			version = excluded.version`,
		userID, string(data), next,
	)
	if err != nil {
		return false, err
	}
	return true, nil
}

// Reset azzera il posizionamento dell'account e incrementa la versione, così
// ogni client applicherà lo stato vuoto alla prossima sync invece di
// ripristinare una copia locale rimasta indietro.
func (r *OrderingRepo) Reset(ctx context.Context, userID string) (int64, error) {
	version, _, err := r.Get(ctx, userID)
	if err != nil {
		return 0, err
	}
	next := version + 1
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO user_ordering (user_id, ordering_json, version) VALUES (?, '{}', ?)
		ON CONFLICT(user_id) DO UPDATE SET
			ordering_json = '{}',
			version = excluded.version`,
		userID, next,
	)
	if err != nil {
		return 0, err
	}
	return next, nil
}
