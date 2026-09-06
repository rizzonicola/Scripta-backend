package db

import (
	"context"
	"database/sql"

	"github.com/google/uuid"

	"notes-server/internal/models"
)

// execer è l'interfaccia minima comune tra *sql.DB e *sql.Tx: permette a
// NotesRepo/FoldersRepo di operare sia in modalità standalone sia dentro una
// transazione esplicita condivisa (vedi SyncHandler.Sync), semplicemente
// costruendo il repository sopra un *sql.Tx invece che sopra il *sql.DB
// radice, senza duplicare alcuna query.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// NotesRepo gestisce la persistenza delle note. A differenza della
// generazione precedente, il contenuto Markdown è la colonna "content": non
// esiste più alcun filesystem da tenere sincronizzato, quindi non servono né
// lock per-path né una fase separata di scrittura file.
type NotesRepo struct {
	db execer
}

func NewNotesRepo(d execer) *NotesRepo {
	return &NotesRepo{db: d}
}

// Get recupera una nota per (userID, id), inclusi i tombstone (deleted_at
// valorizzato). Restituisce (nil, nil) se non esiste o appartiene a un altro
// utente.
func (r *NotesRepo) Get(ctx context.Context, userID, id string) (*models.Note, error) {
	var n models.Note
	err := r.db.QueryRowContext(ctx,
		`SELECT id, user_id, title, content, folder_id, updated_at, deleted_at
		 FROM notes WHERE id = ? AND user_id = ?`,
		id, userID,
	).Scan(&n.ID, &n.UserID, &n.Title, &n.Content, &n.FolderID, &n.UpdatedAt, &n.DeletedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// UpsertLWW inserisce o aggiorna una nota applicando la risoluzione dei
// conflitti Last-Write-Wins direttamente a livello di UPSERT SQL, in modo
// atomico e senza bisogno di alcun lock applicativo:
//
//   - se la nota non esiste ancora, viene inserita;
//   - se esiste già, viene aggiornata SOLO SE updated_at in arrivo è >=
//     dell'updated_at attualmente memorizzato (client vince i pareggi, come
//     nel comportamento storico del server);
//   - altrimenti la riga resta invariata (il server ha già una versione più
//     recente): non è un errore, è il caso "server wins", che il chiamante
//     scoprirà semplicemente rileggendo lo stato con la successiva query di
//     pull (vedi ListUpdatedSince), senza bisogno di alcuna segnalazione
//     esplicita qui.
//
// La clausola "AND notes.user_id = excluded.user_id" è una difesa in
// profondità: impedisce che un client autenticato come utente A possa
// sovrascrivere il contenuto di una nota che appartiene a un utente B anche
// nell'eventualità (qui non raggiungibile dai livelli superiori, che passano
// sempre lo user_id autenticato) in cui indovinasse l'ID di una nota altrui.
func (r *NotesRepo) UpsertLWW(ctx context.Context, n *models.Note) error {
	if n.ID == "" {
		n.ID = uuid.NewString()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO notes (id, user_id, title, content, folder_id, updated_at, deleted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			title      = excluded.title,
			content    = excluded.content,
			folder_id  = excluded.folder_id,
			updated_at = excluded.updated_at,
			deleted_at = excluded.deleted_at
		WHERE excluded.updated_at >= notes.updated_at
		  AND notes.user_id = excluded.user_id
	`, n.ID, n.UserID, n.Title, n.Content, n.FolderID, n.UpdatedAt, n.DeletedAt)
	return err
}

// ForceSet sovrascrive incondizionatamente una nota (usata dalla cascade
// soft-delete: quando una cartella viene cancellata, le note al suo interno
// devono risultare cancellate indipendentemente dal loro updated_at
// precedente, perché la cancellazione della cartella padre è per definizione
// l'evento più recente che le riguarda).
func (r *NotesRepo) ForceSet(ctx context.Context, userID, id string, updatedAt int64, deletedAt *int64) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE notes SET updated_at = ?, deleted_at = ? WHERE id = ? AND user_id = ?`,
		updatedAt, deletedAt, id, userID,
	)
	return err
}

// ListUpdatedSince restituisce tutte le note (incluse quelle soft-deleted,
// cioè i tombstone) di un utente con updated_at strettamente maggiore di
// "since". È la query di "pull" della sync: rappresenta sia le modifiche
// arrivate da altri dispositivi sia l'esito (accettato o server-wins) delle
// modifiche che il client ha appena inviato nella stessa richiesta (se questo
// metodo viene chiamato, come fa SyncHandler, DOPO aver applicato i push
// nella stessa transazione).
func (r *NotesRepo) ListUpdatedSince(ctx context.Context, userID string, since int64) ([]models.Note, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, user_id, title, content, folder_id, updated_at, deleted_at
		 FROM notes WHERE user_id = ? AND updated_at > ? ORDER BY updated_at ASC`,
		userID, since,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var notes []models.Note
	for rows.Next() {
		var n models.Note
		if err := rows.Scan(&n.ID, &n.UserID, &n.Title, &n.Content, &n.FolderID, &n.UpdatedAt, &n.DeletedAt); err != nil {
			return nil, err
		}
		notes = append(notes, n)
	}
	return notes, rows.Err()
}

// ListActiveIDsByFolder restituisce gli ID delle note attive (non ancora
// soft-deleted) contenute direttamente in una data cartella. Usata dalla
// cascade soft-delete per propagare la cancellazione di una cartella a tutte
// le note al suo interno.
func (r *NotesRepo) ListActiveIDsByFolder(ctx context.Context, userID, folderID string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id FROM notes WHERE user_id = ? AND folder_id = ? AND deleted_at IS NULL`,
		userID, folderID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// PurgeExpiredTombstones rimuove definitivamente (hard delete) i tombstone
// (deleted_at valorizzato) più vecchi di "olderThan" (unix millis). Va
// eseguita periodicamente in background (vedi main.go): la finestra di
// retention deve essere abbastanza larga da garantire che ogni dispositivo
// dell'utente abbia avuto modo di fare almeno una sync e ricevere quindi il
// tombstone prima che sparisca definitivamente dal server.
func (r *NotesRepo) PurgeExpiredTombstones(ctx context.Context, olderThan int64) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM notes WHERE deleted_at IS NOT NULL AND deleted_at < ?`,
		olderThan,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
