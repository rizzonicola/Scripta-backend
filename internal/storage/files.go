package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Store gestisce la persistenza dei file .md su filesystem per ogni utente.
type Store struct {
	baseDir string // es. /data/users
	locks   keyedMutex
}

func NewStore(baseDir string) *Store {
	return &Store{baseDir: baseDir}
}

// UserNotesDir restituisce la directory delle note di un utente.
func (s *Store) UserNotesDir(userID string) string {
	return filepath.Join(s.baseDir, userID, "notes")
}

// ResolvePath valida e risolve un relativePath fornito dal client,
// impedendo path traversal (es. "../../etc/passwd").
func (s *Store) ResolvePath(userID, relativePath string) (string, error) {
	clean := filepath.Clean("/" + relativePath) // forza percorso "assoluto" relativo alla root utente
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" || clean == "." {
		return "", fmt.Errorf("percorso relativo non valido")
	}

	base := s.UserNotesDir(userID)
	full := filepath.Join(base, clean)

	// Verifica finale: il path risolto deve restare dentro alla base dir.
	rel, err := filepath.Rel(base, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path traversal rilevato")
	}
	return full, nil
}

// LockPath serializza tutte le operazioni (lettura+scrittura, upsert metadati, ecc.)
// relative allo stesso file, per lo stesso utente. Va usato dal chiamante per
// racchiudere l'intera sequenza "leggi stato -> decidi -> scrivi" (es. la
// risoluzione dei conflitti nella sync) evitando race condition tra richieste
// concorrenti sullo stesso file (stesso utente che sincronizza da più dispositivi
// in parallelo). Restituisce una funzione da chiamare (tipicamente via defer)
// per rilasciare il lock. Il mutex per-path viene creato on demand e rimosso
// automaticamente quando non più referenziato, evitando memory leak.
func (s *Store) LockPath(fullPath string) func() {
	return s.locks.lock(fullPath)
}

// AtomicWrite scrive il contenuto su file in modo atomico: scrive su un file
// temporaneo (.tmp) nella stessa directory e poi esegue os.Rename. os.Rename
// è atomico sullo stesso filesystem, quindi eventuali lettori vedranno sempre
// o il contenuto precedente o quello nuovo completo, mai uno stato parziale.
func (s *Store) AtomicWrite(fullPath string, content []byte) error {
	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()

	// Se qualcosa fallisce dopo la creazione, ripuliamo il temp file residuo.
	// Dopo un rename riuscito il file non esiste più: os.Remove fallirà con
	// ErrNotExist, che ignoriamo silenziosamente (nessuna Stat extra necessaria).
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}

	if err := os.Rename(tmpPath, fullPath); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	renamed = true
	return nil
}

// Delete rimuove il file .md dal filesystem (usato per note soft-deleted).
func (s *Store) Delete(fullPath string) error {
	err := os.Remove(fullPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ReadFile legge il contenuto grezzo di un file .md.
func (s *Store) ReadFile(fullPath string) ([]byte, error) {
	return os.ReadFile(fullPath)
}

// ReadFileCtx si comporta come ReadFile ma rispetta la cancellazione del
// contesto della richiesta HTTP: se il client si disconnette o la richiesta
// scade prima della lettura, evitiamo di eseguire I/O disco inutile.
func (s *Store) ReadFileCtx(ctx context.Context, fullPath string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return os.ReadFile(fullPath)
}

// keyedMutex fornisce un lock per-chiave (striping "perfetto" per path).
// Le voci vengono create on-demand e rimosse quando non più referenziate,
// così la mappa non cresce indefinitamente sotto carico prolungato.
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*refCountedMutex
}

type refCountedMutex struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = make(map[string]*refCountedMutex)
	}
	rc, ok := k.m[key]
	if !ok {
		rc = &refCountedMutex{}
		k.m[key] = rc
	}
	rc.refs++
	k.mu.Unlock()

	rc.mu.Lock()

	var once sync.Once
	return func() {
		once.Do(func() {
			rc.mu.Unlock()
			k.mu.Lock()
			rc.refs--
			if rc.refs == 0 {
				delete(k.m, key)
			}
			k.mu.Unlock()
		})
	}
}
