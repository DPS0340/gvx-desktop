package ext

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/ironpark/gvx-desktop/internal/uiblock"
)

const (
	// DefaultStorageQuota is the 4 MiB per-extension KV storage limit.
	DefaultStorageQuota = 4 << 20
	// MaxStorageKeyBytes is the 128-byte UTF-8 key length limit.
	MaxStorageKeyBytes = 128
)

// Storage is a per-extension key-value store backed by <dataDir>/extension-data/<id>/storage.json.
type Storage struct {
	mu      sync.Mutex
	dataDir string
	path    string
	quota   int
	data    map[string]string
	loaded  bool
}

// NewStorage creates a Storage backed by path with the given byte quota
// (defaults to DefaultStorageQuota if quota <= 0).
func NewStorage(path string, quota int) *Storage {
	if quota <= 0 {
		quota = DefaultStorageQuota
	}
	return &Storage{
		dataDir: filepath.Dir(path),
		path:    path,
		quota:   quota,
	}
}

// NewStorageInDir creates a Storage for extension id rooted at dataDir.
func NewStorageInDir(dataDir, id string, quota int) *Storage {
	if quota <= 0 {
		quota = DefaultStorageQuota
	}
	return &Storage{
		dataDir: dataDir,
		path:    StoragePath(dataDir, id),
		quota:   quota,
	}
}

func (s *Storage) rootDir() string {
	if s.dataDir != "" {
		return s.dataDir
	}
	return filepath.Dir(s.path)
}

// Quota returns the maximum total byte capacity (keys + values).
func (s *Storage) Quota() int {
	return s.quota
}

// Reset clears the in-memory cache so the next operation reloads from disk.
func (s *Storage) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = nil
	s.loaded = false
}

func (s *Storage) loadLocked() error {
	if s.loaded {
		return nil
	}
	b, err := ReadFileNoSymlinkWithin(s.rootDir(), s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.data = map[string]string{}
			s.loaded = true
			return nil
		}
		s.data = nil
		s.loaded = false
		return fmt.Errorf("load %s: %w", filepath.Base(s.path), err)
	}
	var data map[string]string
	if err := json.Unmarshal(b, &data); err != nil {
		s.data = nil
		s.loaded = false
		return fmt.Errorf("load %s: %w", filepath.Base(s.path), err)
	}
	if data == nil {
		s.data = nil
		s.loaded = false
		return fmt.Errorf("load %s: unexpected null JSON", filepath.Base(s.path))
	}
	s.data = data
	s.loaded = true
	return nil
}

func (s *Storage) usedLocked() int {
	n := 0
	for k, v := range s.data {
		n += len(k) + len(v)
	}
	return n
}

func (s *Storage) saveLocked() error {
	b, err := json.Marshal(s.data)
	if err != nil {
		return err
	}
	return WriteFileAtomicWithin(s.rootDir(), s.path, b, 0o600)
}

func checkStorageKey(key string) error {
	if key == "" || len(key) > MaxStorageKeyBytes || !utf8.ValidString(key) {
		return fmt.Errorf("storage key must be valid UTF-8 of 1 to %d bytes", MaxStorageKeyBytes)
	}
	if strings.ContainsFunc(key, uiblock.Spoofing) {
		return errors.New("storage key must not contain control or spoofing characters")
	}
	return nil
}

// Get returns the value stored for key, if present.
func (s *Storage) Get(key string) (string, bool, error) {
	if err := checkStorageKey(key); err != nil {
		return "", false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return "", false, err
	}
	v, ok := s.data[key]
	return v, ok, nil
}

// Set writes value for key, enforcing key length, UTF-8 validity, and total byte quota.
// Returns the total bytes used after the write, rolling back in-memory state if persistence fails.
func (s *Storage) Set(key, value string) (int, error) {
	if err := checkStorageKey(key); err != nil {
		return 0, err
	}
	if !utf8.ValidString(value) {
		return 0, errors.New("storage value must be valid UTF-8")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return 0, err
	}
	old, had := s.data[key]
	used := s.usedLocked() + len(value) - len(old)
	if !had {
		used += len(key)
	}
	if used > s.quota {
		return 0, fmt.Errorf("storage quota exceeded: write requires %d bytes (quota %d bytes)", used, s.quota)
	}
	s.data[key] = value
	if err := s.saveLocked(); err != nil {
		if had {
			s.data[key] = old
		} else {
			delete(s.data, key)
		}
		return 0, err
	}
	return used, nil
}

// Delete removes key from storage, returning whether it existed.
// Rolls back in-memory state if persistence fails.
func (s *Storage) Delete(key string) (bool, error) {
	if err := checkStorageKey(key); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return false, err
	}
	old, had := s.data[key]
	if !had {
		return false, nil
	}
	delete(s.data, key)
	if err := s.saveLocked(); err != nil {
		s.data[key] = old
		return false, err
	}
	return true, nil
}

// Keys returns all keys matching prefix in sorted order.
func (s *Storage) Keys(prefix string) ([]string, error) {
	if !utf8.ValidString(prefix) {
		return nil, errors.New("storage prefix must be valid UTF-8")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return nil, err
	}
	var out []string
	for k := range s.data {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out, nil
}

// Used returns the total bytes currently occupied by keys and values.
func (s *Storage) Used() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return 0, err
	}
	return s.usedLocked(), nil
}
