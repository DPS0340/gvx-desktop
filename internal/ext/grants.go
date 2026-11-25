package ext

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/ironpark/gvx-desktop/internal/recipes"
)

// GrantEntry stores persisted permissions, folder grants, disabled flag,
// and approved source/manifest state for a single extension.
type GrantEntry struct {
	Granted              []string `json:"granted,omitempty"`
	Denied               []string `json:"denied,omitempty"`
	Disabled             bool     `json:"disabled,omitempty"`
	AllowedFolders       []string `json:"allowed_folders,omitempty"`
	ApprovedCommit       string   `json:"approved_commit,omitempty"`
	ApprovedManifestHash string   `json:"approved_manifest_hash,omitempty"`
	ApprovedVersion      string   `json:"approved_version,omitempty"`
	DeniedManifestHash   string   `json:"denied_manifest_hash,omitempty"`
}

func (g GrantEntry) clone() GrantEntry {
	g.Granted = slices.Clone(g.Granted)
	g.Denied = slices.Clone(g.Denied)
	g.AllowedFolders = slices.Clone(g.AllowedFolders)
	return g
}

// GrantStore manages <dataDir>/extension-grants.json.
// If loading fails (for example due to corrupt JSON), loadErr is preserved and
// all mutating operations refuse to overwrite the file on disk.
type GrantStore struct {
	mu      sync.RWMutex
	root    string
	path    string
	entries map[string]GrantEntry
	loadErr error
}

// LoadGrantStore loads extension-grants.json from path.
// If the file is corrupt or contains null JSON, LoadGrantStore returns both a
// write-protected GrantStore and the load error.
func LoadGrantStore(path string) (*GrantStore, error) {
	root := filepath.Dir(path)
	gs := &GrantStore{
		root:    root,
		path:    path,
		entries: map[string]GrantEntry{},
	}
	b, err := ReadFileNoSymlinkWithin(root, path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return gs, nil
		}
		gs.loadErr = fmt.Errorf("load %s: %w", filepath.Base(path), err)
		return gs, gs.loadErr
	}
	var entries map[string]GrantEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		gs.loadErr = fmt.Errorf("load %s: %w", filepath.Base(path), err)
		return gs, gs.loadErr
	}
	if entries == nil {
		gs.loadErr = fmt.Errorf("load %s: unexpected null JSON", filepath.Base(path))
		return gs, gs.loadErr
	}
	gs.entries = entries
	return gs, nil
}

// LoadError returns any error encountered when loading the grants file.
func (s *GrantStore) LoadError() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loadErr
}

// Get returns a copy of the GrantEntry for id.
func (s *GrantStore) Get(id string) GrantEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.entries[id].clone()
}

// HasPermission reports whether id currently holds perm and perm is declared in manifestPerms.
func (s *GrantStore) HasPermission(id, perm string, manifestPerms []string) bool {
	if !slices.Contains(manifestPerms, perm) {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Contains(s.entries[id].Granted, perm)
}

func (s *GrantStore) update(id string, fn func(*GrantEntry) error) error {
	if err := ValidateID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return fmt.Errorf("refusing to overwrite corrupt grants file: %w", s.loadErr)
	}
	prev, had := s.entries[id]
	next := prev.clone()
	if err := fn(&next); err != nil {
		return err
	}
	if isEmptyGrant(next) {
		delete(s.entries, id)
	} else {
		s.entries[id] = next
	}
	if err := s.saveLocked(); err != nil {
		if had {
			s.entries[id] = prev
		} else {
			delete(s.entries, id)
		}
		return err
	}
	return nil
}

func isEmptyGrant(g GrantEntry) bool {
	return len(g.Granted) == 0 &&
		len(g.Denied) == 0 &&
		!g.Disabled &&
		len(g.AllowedFolders) == 0 &&
		g.ApprovedCommit == "" &&
		g.ApprovedManifestHash == "" &&
		g.ApprovedVersion == "" &&
		g.DeniedManifestHash == ""
}

func (s *GrantStore) saveLocked() error {
	b, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}
	root := s.root
	if root == "" {
		root = filepath.Dir(s.path)
	}
	return WriteFileAtomicWithin(root, s.path, append(b, '\n'), 0o600)
}

// Approve records user approval of m's permissions, version, manifest hash, and commitSHA.
func (s *GrantStore) Approve(id string, m *Manifest, commitSHA string) error {
	if m == nil {
		return errors.New("cannot approve nil manifest")
	}
	return s.update(id, func(g *GrantEntry) error {
		g.Granted = slices.Clone(m.Permissions)
		g.Denied = nil
		g.DeniedManifestHash = ""
		g.ApprovedCommit = commitSHA
		g.ApprovedManifestHash = m.Hash
		g.ApprovedVersion = m.Version
		return nil
	})
}

// Deny records user denial for m (or explicit permissions), clearing granted permissions.
func (s *GrantStore) Deny(id string, m *Manifest) error {
	if m == nil {
		return errors.New("cannot deny nil manifest")
	}
	return s.update(id, func(g *GrantEntry) error {
		g.Granted = nil
		g.ApprovedCommit = ""
		g.ApprovedManifestHash = ""
		g.ApprovedVersion = ""
		g.Denied = slices.Clone(m.Permissions)
		g.DeniedManifestHash = m.Hash
		return nil
	})
}

// RevokePermissions clears granted permissions and approval state while preserving
// Disabled and AllowedFolders.
func (s *GrantStore) RevokePermissions(id string) error {
	return s.update(id, func(g *GrantEntry) error {
		g.Granted = nil
		g.Denied = nil
		g.ApprovedCommit = ""
		g.ApprovedManifestHash = ""
		g.ApprovedVersion = ""
		g.DeniedManifestHash = ""
		return nil
	})
}

// SetDisabled sets or clears the user-disabled flag for id.
func (s *GrantStore) SetDisabled(id string, disabled bool) error {
	return s.update(id, func(g *GrantEntry) error {
		g.Disabled = disabled
		return nil
	})
}

// AddAllowedFolder validates dir as an existing non-reserved host directory,
// adds its canonical path to id's AllowedFolders (deduplicated), and persists it.
func (s *GrantStore) AddAllowedFolder(id, dir string) (string, error) {
	canonical, err := validateAllowedFolder(dir)
	if err != nil {
		return "", err
	}
	err = s.update(id, func(g *GrantEntry) error {
		if !slices.Contains(g.AllowedFolders, canonical) {
			g.AllowedFolders = append(g.AllowedFolders, canonical)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return canonical, nil
}

// RemoveAllowedFolder removes dir (matching cleaned or symlink-resolved path) from
// id's AllowedFolders, returning whether an entry was removed.
func (s *GrantStore) RemoveAllowedFolder(id, dir string) (bool, error) {
	clean := filepath.Clean(strings.TrimSpace(dir))
	realPath, _ := filepath.EvalSymlinks(clean)
	removed := false
	err := s.update(id, func(g *GrantEntry) error {
		before := len(g.AllowedFolders)
		g.AllowedFolders = slices.DeleteFunc(g.AllowedFolders, func(f string) bool {
			return f == clean || (realPath != "" && f == realPath)
		})
		removed = len(g.AllowedFolders) < before
		return nil
	})
	return removed, err
}

// Remove deletes all grant state for id.
func (s *GrantStore) Remove(id string) error {
	return s.update(id, func(g *GrantEntry) error {
		*g = GrantEntry{}
		return nil
	})
}

func validateAllowedFolder(dir string) (string, error) {
	trimmed := strings.TrimSpace(dir)
	if trimmed == "" {
		return "", errors.New("folder path is empty")
	}
	clean := filepath.Clean(trimmed)
	if !filepath.IsAbs(clean) {
		return "", fmt.Errorf("folder %q must be an absolute path", dir)
	}
	info, err := os.Stat(clean)
	if err != nil {
		return "", fmt.Errorf("inspect folder %q: %w", dir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("folder %q is not a directory", dir)
	}
	realDir, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", fmt.Errorf("resolve folder %q: %w", dir, err)
	}
	for _, candidate := range []string{clean, realDir} {
		guest, err := recipes.GuestPath(candidate)
		if err != nil {
			return "", fmt.Errorf("folder %q cannot be shared: %w", dir, err)
		}
		if recipes.ReservedGuestPath(guest) {
			return "", fmt.Errorf("folder %q is a reserved system path", dir)
		}
		if runtime.GOOS == "darwin" {
			if candidate == "/private" || guest == "/private" {
				return "", fmt.Errorf("folder %q is a reserved system path", dir)
			}
			if strings.HasPrefix(guest, "/private/") && recipes.ReservedGuestPath(strings.TrimPrefix(guest, "/private")) {
				return "", fmt.Errorf("folder %q is a reserved system path", dir)
			}
		}
	}
	return realDir, nil
}

// NeedsApproval evaluates whether extension m requires user approval ("waiting")
// or has been denied ("denied") given its GrantEntry and current source commitSHA.
//
// Escalation rules (§4.3):
//   - Bundled extensions never wait for approval.
//   - Any newly added permission not yet granted triggers waiting.
//   - Any manifest hash change, version change, or git commit SHA change since
//     the last approval triggers waiting so the user reviews the diff before build/run.
//   - If denied at the current manifest hash (and no new un-denied permissions
//     were introduced), denied is true.
func NeedsApproval(m *Manifest, entry GrantEntry, commitSHA string) (waiting bool, denied bool, asked []string) {
	if m == nil || m.Bundled {
		return false, false, nil
	}
	for _, p := range m.Permissions {
		if !slices.Contains(entry.Granted, p) {
			asked = append(asked, p)
		}
	}

	// Never approved yet.
	if entry.ApprovedManifestHash == "" {
		if entry.DeniedManifestHash == m.Hash {
			if len(asked) == 0 {
				// 0-permission extension explicitly denied at this manifest hash.
				return false, true, nil
			}
			for _, p := range asked {
				if !slices.Contains(entry.Denied, p) {
					return true, false, asked
				}
			}
			return false, true, asked
		}
		if len(asked) > 0 && entry.DeniedManifestHash == "" {
			allDenied := len(entry.Denied) > 0
			for _, p := range asked {
				if !slices.Contains(entry.Denied, p) {
					allDenied = false
					break
				}
			}
			if allDenied {
				return false, true, asked
			}
		}
		return true, false, asked
	}

	// Previously approved: check for escalation or new ungranted permissions.
	if len(asked) > 0 {
		return true, false, asked
	}
	if entry.ApprovedManifestHash != m.Hash {
		return true, false, asked
	}
	if entry.ApprovedVersion != "" && entry.ApprovedVersion != m.Version {
		return true, false, asked
	}
	if commitSHA != "" && entry.ApprovedCommit != commitSHA {
		return true, false, asked
	}
	return false, false, nil
}
