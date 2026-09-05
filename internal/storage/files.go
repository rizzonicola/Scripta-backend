package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type StorageManager struct {
	BaseDir string
}

func NewStorageManager(baseDir string) *StorageManager {
	return &StorageManager{BaseDir: baseDir}
}

// GetUserDir restituisce il percorso della cartella dedicata all'utente.
func (s *StorageManager) GetUserDir(userID int64) string {
	return filepath.Join(s.BaseDir, strconv.FormatInt(userID, 10))
}

// EnsureUserDir assicura che la directory fisica dell'utente esista.
func (s *StorageManager) EnsureUserDir(userID int64) error {
	dir := s.GetUserDir(userID)
	return os.MkdirAll(dir, 0755)
}

// isSafePath previene attacchi di Path Traversal (es. ../../etc/passwd).
func (s *StorageManager) isSafePath(basePath, targetPath string) bool {
	rel, err := filepath.Rel(basePath, targetPath)
	if err != nil || strings.HasPrefix(rel, "..") || rel == ".." {
		return false
	}
	return true
}

// SaveFile scrive o aggiorna un file (nota o allegato) nella cartella dell'utente.
func (s *StorageManager) SaveFile(userID int64, filename string, data []byte) error {
	if err := s.EnsureUserDir(userID); err != nil {
		return fmt.Errorf("impossibile creare la directory utente: %w", err)
	}

	userDir := s.GetUserDir(userID)
	targetPath := filepath.Clean(filepath.Join(userDir, filename))

	if !s.isSafePath(userDir, targetPath) {
		return fmt.Errorf("tentativo di path traversal rilevato: %s", filename)
	}

	return os.WriteFile(targetPath, data, 0644)
}

// ReadFile legge il contenuto di un file dell'utente dal disco.
func (s *StorageManager) ReadFile(userID int64, filename string) ([]byte, error) {
	userDir := s.GetUserDir(userID)
	targetPath := filepath.Clean(filepath.Join(userDir, filename))

	if !s.isSafePath(userDir, targetPath) {
		return nil, fmt.Errorf("tentativo di path traversal rilevato: %s", filename)
	}

	return os.ReadFile(targetPath)
}

// DeleteFile rimuove un singolo file dalla cartella dell'utente.
func (s *StorageManager) DeleteFile(userID int64, filename string) error {
	userDir := s.GetUserDir(userID)
	targetPath := filepath.Clean(filepath.Join(userDir, filename))

	if !s.isSafePath(userDir, targetPath) {
		return fmt.Errorf("tentativo di path traversal rilevato: %s", filename)
	}

	if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("impossibile eliminare il file %s: %w", filename, err)
	}
	return nil
}

// ListUserFiles restituisce i nomi di tutti i file contenuti nella directory dell'utente.
func (s *StorageManager) ListUserFiles(userID int64) ([]string, error) {
	userDir := s.GetUserDir(userID)
	entries, err := os.ReadDir(userDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}

	var files []string
	for _, entry := range entries {
		if !entry.IsDir() {
			files = append(files, entry.Name())
		}
	}
	return files, nil
}

// DeleteUserDataDir rimuove in modo sicuro l'intera directory fisica dell'utente e il suo contenuto.
func (s *StorageManager) DeleteUserDataDir(userID int64) error {
	userDir := filepath.Clean(s.GetUserDir(userID))
	baseDir := filepath.Clean(s.BaseDir)

	if !s.isSafePath(baseDir, userDir) || userDir == baseDir {
		return fmt.Errorf("tentativo di eliminazione directory non valido: %s", userDir)
	}

	if err := os.RemoveAll(userDir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("impossibile rimuovere la directory utente %s: %w", userDir, err)
	}

	return nil
}
