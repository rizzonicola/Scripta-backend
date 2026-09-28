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
		`SELECT id, user_id, title, content, folder_id, is_favorite, is_pinned, order_index, updated_at, synced_at, deleted_at
		 FROM notes WHERE id = ? AND user_id = ?`,
		id, userID,
	).Scan(&n.ID, &n.UserID, &n.Title, &n.Content, &n.FolderID, &n.IsFavorite, &n.IsPinned, &n.OrderIndex, &n.UpdatedAt, &n.SyncedAt, &n.DeletedAt)
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
//     pull (vedi ListSyncedBetween), senza bisogno di alcuna segnalazione
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
		INSERT INTO notes (id, user_id, title, content, folder_id, is_favorite, is_pinned, order_index, updated_at, synced_at, deleted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			title       = excluded.title,
			content     = excluded.content,
			folder_id   = excluded.folder_id,
			is_favorite = excluded.is_favorite,
			is_pinned   = excluded.is_pinned,
			order_index = excluded.order_index,
			updated_at  = excluded.updated_at,
			synced_at   = excluded.synced_at,
			deleted_at  = excluded.deleted_at
		WHERE excluded.updated_at >= notes.updated_at
		  AND notes.user_id = excluded.user_id
	`, n.ID, n.UserID, n.Title, n.Content, n.FolderID, n.IsFavorite, n.IsPinned, n.OrderIndex, n.UpdatedAt, n.SyncedAt, n.DeletedAt)
	return err
}

// ForceSet sovrascrive incondizionatamente una nota (usata dalla cascade
// soft-delete: quando una cartella viene cancellata, le note al suo interno
// devono risultare cancellate indipendentemente dal loro updated_at
// precedente, perché la cancellazione della cartella padre è per definizione
// l'evento più recente che le riguarda).
func (r *NotesRepo) ForceSet(ctx context.Context, userID, id string, updatedAt, syncedAt int64, deletedAt *int64) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE notes SET updated_at = ?, synced_at = ?, deleted_at = ? WHERE id = ? AND user_id = ?`,
		updatedAt, syncedAt, deletedAt, id, userID,
	)
	return err
}

// ListSyncedBetween restituisce le note di un utente (tombstone inclusi se
// includeTombstones) con synced_at nell'intervallo (since, upTo]. È la query
// di "pull" della sync.
//
// Filtra ESCLUSIVAMENTE su synced_at (orologio SERVER, monotono), mai su
// updated_at (orologio del client): così una modifica accettata dal server
// dopo l'ultima pull di un dispositivo gli viene sempre consegnata, anche se
// il client che l'ha scritta ha l'orologio indietro o è stato a lungo
// offline. Il limite superiore upTo (= stamp della sync corrente) rende il
// cursore restituito al client sicuro: tutto ciò che ha synced_at <= upTo è
// già committato e incluso.
func (r *NotesRepo) ListSyncedBetween(ctx context.Context, userID string, since, upTo int64, includeTombstones bool) ([]models.Note, error) {
	query := `SELECT id, user_id, title, content, folder_id, is_favorite, is_pinned, order_index, updated_at, synced_at, deleted_at
		 FROM notes WHERE user_id = ? AND synced_at > ? AND synced_at <= ?`
	if !includeTombstones {
		query += ` AND deleted_at IS NULL`
	}
	query += ` ORDER BY synced_at ASC, id ASC`
	rows, err := r.db.QueryContext(ctx, query, userID, since, upTo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var notes []models.Note
	for rows.Next() {
		var n models.Note
		if err := rows.Scan(&n.ID, &n.UserID, &n.Title, &n.Content, &n.FolderID, &n.IsFavorite, &n.IsPinned, &n.OrderIndex, &n.UpdatedAt, &n.SyncedAt, &n.DeletedAt); err != nil {
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

// PurgeExpiredTombstones rimuove definitivamente (hard delete) i tombstone il
// cui synced_at (istante SERVER in cui il server ha accettato la
// cancellazione) è più vecchio di "olderThan" (unix millis). Si usa
// synced_at e non deleted_at perché quest'ultimo è scelto dal client (può
// essere arbitrariamente vecchio o sbagliato per clock skew): la retention
// deve contare da quando il server ha ricevuto la cancellazione, non da
// quando il client dichiara di averla fatta.
//
// Va eseguita periodicamente (vedi main.go). La finestra di retention deve
// essere abbastanza larga da garantire che ogni dispositivo attivo riceva il
// tombstone prima che sparisca; i dispositivi più lenti vengono riallineati
// da SyncResponse.FullResync (vedi handlers/api_sync.go).
func (r *NotesRepo) PurgeExpiredTombstones(ctx context.Context, olderThan int64) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM notes WHERE deleted_at IS NOT NULL AND synced_at < ?`,
		olderThan,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// TouchSynced aggiorna SOLO synced_at di una riga esistente dell'utente. Serve
// quando un push perde il confronto LWW (il server ha già una versione più
// recente): la riga vincente non cambia, ma va ri-consegnata al dispositivo
// "perdente" nella pull della stessa richiesta, altrimenti quel dispositivo
// resterebbe con una copia divergente (il suo cursore è già oltre il vecchio
// synced_at della riga vincente).
func (r *NotesRepo) TouchSynced(ctx context.Context, userID, id string, syncedAt int64) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE notes SET synced_at = ? WHERE id = ? AND user_id = ?`,
		syncedAt, id, userID,
	)
	return err
}
