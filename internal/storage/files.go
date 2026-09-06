package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
)

// Store gestisce la persistenza dei file .md su filesystem per ogni utente.
type Store struct {
	baseDir   string       // es. /data/users
	locks     keyedMutex   // lock fine-grained per singolo path (file o cartella)
	userLocks keyedRWMutex // lock a livello di intero utente (vedi RLockUser/LockUser)
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

// LockPaths acquisisce, in un ordine deterministico (ordinamento lessicografico
// dei path, con deduplica), i lock per-path di più percorsi contemporaneamente.
// È usata dallo spostamento di note/cartelle, che deve serializzare sia il
// percorso di origine sia quello di destinazione: bloccare sempre nello stesso
// ordine, indipendentemente dalla direzione dello spostamento richiesto,
// evita deadlock tra due richieste concorrenti che spostano file "incrociando"
// gli stessi due path in direzioni opposte (A->B e B->A in parallelo).
// Restituisce una singola funzione di unlock (da usare con defer) che rilascia
// tutti i lock acquisiti, in ordine inverso.
func (s *Store) LockPaths(paths ...string) func() {
	seen := make(map[string]bool, len(paths))
	uniq := make([]string, 0, len(paths))
	for _, p := range paths {
		if !seen[p] {
			seen[p] = true
			uniq = append(uniq, p)
		}
	}
	sort.Strings(uniq)

	unlocks := make([]func(), 0, len(uniq))
	for _, p := range uniq {
		unlocks = append(unlocks, s.locks.lock(p))
	}
	return func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}
}

// RLockUser acquisisce un lock condiviso ("in lettura") sull'intero utente:
// più chiamate concorrenti a RLockUser per lo stesso utente possono procedere
// in parallelo tra loro (es. più note sincronizzate contemporaneamente da
// device diversi), ma vengono tutte messe in attesa se è in corso un
// LockUser esclusivo (vedi sotto). Va acquisito da ogni operazione "normale"
// che tocca solo un file/cartella puntuale (creazione, scrittura, hard/soft
// delete di UNA voce), PRIMA dell'eventuale LockPath/LockPaths più
// granulare su quel singolo path.
func (s *Store) RLockUser(userID string) func() {
	return s.userLocks.rlock(userID)
}

// LockUser acquisisce un lock esclusivo sull'intero utente: attende che
// tutti gli RLockUser in corso per quell'utente si rilascino, poi blocca
// qualunque nuova RLockUser/LockUser finché non viene rilasciato.
//
// Perché serve, in aggiunta al lock per-path già esistente (LockPath/
// LockPaths): quest'ultimo protegge una singola voce (un file o una
// cartella) da letture/scritture concorrenti su quel MEDESIMO path, ma non
// protegge da operazioni "strutturali" che agiscono sull'intero sottoalbero
// di un utente:
//   - uno spostamento di cartella (os.Rename su una directory) può avvenire
//     mentre un'altra richiesta sta scrivendo un file al suo interno con
//     AtomicWrite (che fa MkdirAll+CreateTemp+Rename in quella stessa
//     directory): i due lock per-path non si "vedono" perché sono su path
//     diversi (la cartella vs. il file al suo interno);
//   - la cancellazione utente (os.RemoveAll sull'intera cartella
//     dell'utente, vedi DeleteUserData) può correre in parallelo a una sync
//     in corso per lo stesso utente: senza un lock a livello di utente,
//     AtomicWrite potrebbe ricreare directory/file appena dopo la
//     RemoveAll, lasciando dati residui su disco per un account che
//     dovrebbe risultare completamente cancellato.
//
// Usato da: syncMove (spostamento nota/cartella) e DeleteUserData
// (cancellazione utente), in esclusiva; tutte le altre operazioni puntuali
// usano RLockUser, quindi restano concorrenti tra loro come prima.
func (s *Store) LockUser(userID string) func() {
	return s.userLocks.lock(userID)
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

// EnsureFolder crea (se non esiste già) la directory corrispondente a
// fullPath, creando anche tutte le directory intermedie mancanti. È usata per
// persistere sul filesystem le cartelle create dal client anche quando non
// contengono ancora alcuna nota .md: senza questa chiamata esplicita, una
// cartella vuota non lascerebbe alcuna traccia su disco (a differenza di una
// cartella con almeno un file, che viene creata "di riflesso" da AtomicWrite
// quando scrive il file al suo interno).
func (s *Store) EnsureFolder(fullPath string) error {
	if err := os.MkdirAll(fullPath, 0o755); err != nil {
		return fmt.Errorf("mkdir cartella: %w", err)
	}
	return nil
}

// DeleteFolder rimuove una cartella dal filesystem, ma SOLO se è vuota:
// usiamo os.Remove (non os.RemoveAll) apposta, perché se la cartella contiene
// ancora file non è sicuro cancellarla "in cascata" solo perché il client ha
// segnalato la cancellazione della cartella stessa (le note al suo interno
// potrebbero non essere ancora state processate in questo stesso batch di
// sync, o su questo device essere ancora considerate valide). Se la cartella
// non è vuota o non esiste più, l'operazione è un no-op silenzioso: verrà
// rimossa automaticamente in un successivo giro di sync quando risulterà
// effettivamente vuota.
func (s *Store) DeleteFolder(fullPath string) error {
	err := os.Remove(fullPath)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	if isDirNotEmpty(err) {
		return nil
	}
	return err
}

// isDirNotEmpty riconosce l'errore "directory not empty" restituito da
// os.Remove su una cartella con contenuto. Il controllo primario è
// sull'errno POSIX (errors.Is(err, syscall.ENOTEMPTY)), portabile su
// Linux/macOS/Windows tramite il mapping che il pacchetto syscall di Go già
// applica su ciascuna piattaforma; il confronto testuale è mantenuto solo
// come rete di sicurezza per filesystem/driver che restituissero un errore
// non direttamente riconducibile a quell'errno.
func isDirNotEmpty(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ENOTEMPTY) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not empty") || strings.Contains(msg, "directory not empty")
}

// MovePath sposta fisicamente un file o una cartella (con tutto il suo
// contenuto) da oldFullPath a newFullPath, creando le eventuali directory
// intermedie mancanti sul percorso di destinazione. Usa os.Rename, che sullo
// stesso filesystem (come qui, essendo oldFullPath e newFullPath sempre
// dentro la stessa baseDir/utente) è un'operazione atomica: non riscrive il
// contenuto byte per byte, quindi preserva esattamente i dati esistenti anche
// quando il client, in uno spostamento "puro", non rinvia il contenuto
// aggiornato della nota.
func (s *Store) MovePath(oldFullPath, newFullPath string) error {
	if oldFullPath == newFullPath {
		return nil
	}
	if _, err := os.Lstat(oldFullPath); err != nil {
		// Propaghiamo l'errore (incluso os.IsNotExist) così il chiamante può
		// distinguere "sorgente assente" da altri errori e decidere un
		// fallback appropriato.
		return err
	}

	destDir := filepath.Dir(newFullPath)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("mkdir destinazione: %w", err)
	}

	if err := os.Rename(oldFullPath, newFullPath); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// UserRootDir restituisce la directory radice di un utente (baseDir/<userID>),
// che contiene la sottocartella "notes" e, potenzialmente, altri dati futuri
// legati all'account. È il percorso che viene rimosso interamente in fase di
// cancellazione utente (vedi DeleteUserData).
func (s *Store) UserRootDir(userID string) string {
	return filepath.Join(s.baseDir, userID)
}

// DeleteUserData rimuove ricorsivamente e definitivamente tutti i dati su
// disco di un utente (baseDir/<userID>/...), usata dalla dashboard admin in
// cascata alla cancellazione dell'account dal database. Prima di invocare
// os.RemoveAll verifichiamo esplicitamente che il percorso risultante resti
// dentro baseDir: gli userID in pratica sono sempre UUID generati dal server
// (mai forniti direttamente dall'utente finale), ma questo controllo è una
// difesa in profondità a costo pressoché nullo contro un userID vuoto,
// malformato o comunque inatteso, che altrimenti rischierebbe di far
// eseguire un os.RemoveAll sull'intera baseDir.
func (s *Store) DeleteUserData(userID string) error {
	if strings.TrimSpace(userID) == "" {
		return fmt.Errorf("userID vuoto: rifiuto la cancellazione")
	}

	dir := s.UserRootDir(userID)

	base, err := filepath.Abs(s.baseDir)
	if err != nil {
		return fmt.Errorf("risoluzione baseDir: %w", err)
	}
	target, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("risoluzione percorso utente: %w", err)
	}

	rel, err := filepath.Rel(base, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("percorso utente non valido, rifiuto la cancellazione: %s", dir)
	}

	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("rimozione dati utente: %w", err)
	}
	return nil
}

// ReadFile legge il contenuto grezzo di un file .md.
func (s *Store) ReadFile(fullPath string) ([]byte, error) {
	return os.ReadFile(fullPath)
}

// HashFile calcola lo SHA-256 di un file leggendolo in streaming (buffer
// interno di io.Copy, tipicamente 32 KiB), SENZA mai tenere l'intero
// contenuto in un'unica slice separata. Va preferita a
// "ReadFile + sha256.Sum256" in tutti i casi in cui serve SOLO l'hash e non
// il contenuto stesso (tipicamente dopo un os.Rename in syncMove, dove il
// client spesso non rinvia il content e quindi il checksum va ricalcolato
// leggendo il file appena spostato): evita sia il buffer di ReadFile sia le
// successive conversioni byte<->string altrimenti necessarie per riusare
// l'helper sha256Hex(content string), dimezzando i picchi di allocazione per
// note di grandi dimensioni.
func (s *Store) HashFile(fullPath string) (string, error) {
	f, err := os.Open(fullPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash file: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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

// keyedRWMutex è l'equivalente di keyedMutex ma basato su sync.RWMutex:
// fornisce, per ciascuna chiave (qui: userID), un lock condiviso (rlock,
// più titolari contemporanei) e uno esclusivo (lock, un solo titolare che
// esclude anche tutti i condivisi). Stessa strategia di refcounting e
// rimozione on-demand delle voci per non far crescere la mappa
// indefinitamente sotto carico prolungato.
type keyedRWMutex struct {
	mu sync.Mutex
	m  map[string]*refCountedRWMutex
}

type refCountedRWMutex struct {
	mu   sync.RWMutex
	refs int
}

func (k *keyedRWMutex) entry(key string) *refCountedRWMutex {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.m == nil {
		k.m = make(map[string]*refCountedRWMutex)
	}
	rc, ok := k.m[key]
	if !ok {
		rc = &refCountedRWMutex{}
		k.m[key] = rc
	}
	rc.refs++
	return rc
}

func (k *keyedRWMutex) release(key string, rc *refCountedRWMutex) {
	k.mu.Lock()
	rc.refs--
	if rc.refs == 0 {
		delete(k.m, key)
	}
	k.mu.Unlock()
}

func (k *keyedRWMutex) rlock(key string) func() {
	rc := k.entry(key)
	rc.mu.RLock()
	var once sync.Once
	return func() {
		once.Do(func() {
			rc.mu.RUnlock()
			k.release(key, rc)
		})
	}
}

func (k *keyedRWMutex) lock(key string) func() {
	rc := k.entry(key)
	rc.mu.Lock()
	var once sync.Once
	return func() {
		once.Do(func() {
			rc.mu.Unlock()
			k.release(key, rc)
		})
	}
}
