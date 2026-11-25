package ext

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"unicode/utf8"
)

// MaxSettingTextBytes caps string settings persisted in settings.json.
const MaxSettingTextBytes = 1000

// SettingsStore persists non-secret extension settings in 0600 settings.json
// and routes secret settings through the extension's Vault ("setting:<key>").
type SettingsStore struct {
	mu      sync.Mutex
	dataDir string
	path    string
	vault   *Vault
	values  map[string]any
	loaded  bool
}

// NewSettingsStore creates a SettingsStore backed by path and v.
func NewSettingsStore(path string, v *Vault) *SettingsStore {
	return &SettingsStore{
		dataDir: filepath.Dir(path),
		path:    path,
		vault:   v,
	}
}

// NewSettingsStoreInDir creates a SettingsStore for extension id rooted at dataDir.
func NewSettingsStoreInDir(dataDir, id string, v *Vault) *SettingsStore {
	return &SettingsStore{
		dataDir: dataDir,
		path:    SettingsPath(dataDir, id),
		vault:   v,
	}
}

func (s *SettingsStore) rootDir() string {
	if s.dataDir != "" {
		return s.dataDir
	}
	return filepath.Dir(s.path)
}

// Reset clears the cached non-secret settings map.
func (s *SettingsStore) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values = nil
	s.loaded = false
}

func (s *SettingsStore) loadLocked() {
	if s.loaded {
		return
	}
	s.loaded = true
	s.values = map[string]any{}
	if b, err := ReadFileNoSymlinkWithin(s.rootDir(), s.path); err == nil {
		raw := map[string]any{}
		if json.Unmarshal(b, &raw) == nil && raw != nil {
			s.values = raw
		}
	}
}

func (s *SettingsStore) saveLocked() error {
	b, err := json.MarshalIndent(s.values, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomicWithin(s.rootDir(), s.path, append(b, '\n'), 0o600)
}

func (s *SettingsStore) migrateLocked(d SettingDecl) {
	if s.vault == nil {
		return
	}
	v, ok := s.values[d.Key].(string)
	if !ok {
		return
	}
	if CheckSecretValue(d.Key, v) != nil || s.vault.Set(VaultSettingPrefix+d.Key, v) != nil {
		return
	}
	delete(s.values, d.Key)
	_ = s.saveLocked()
}

// MigrateLegacySecrets moves any secret setting values mistakenly stored in
// settings.json into the Vault and removes them from settings.json.
func (s *SettingsStore) MigrateLegacySecrets(m *Manifest) {
	if m == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	for _, d := range m.Settings {
		if d.Type == SettingSecret {
			s.migrateLocked(d)
		}
	}
}

func (s *SettingsStore) hasLegacySecret(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	_, ok := s.values[key].(string)
	return ok
}

func settingFits(d SettingDecl, v any) bool {
	switch val := v.(type) {
	case string:
		return d.Type == SettingString || d.Type == SettingSecret || (d.Type == SettingSelect && slices.Contains(d.Options, val))
	case bool:
		return d.Type == SettingBoolean
	case float64:
		return d.Type == SettingNumber && !math.IsNaN(val) && !math.IsInf(val, 0)
	}
	return false
}

// Get returns the typed value for setting d, whether it is explicitly set, and any vault error.
func (s *SettingsStore) Get(d SettingDecl) (any, bool, error) {
	if d.Type == SettingSecret {
		s.mu.Lock()
		s.loadLocked()
		s.migrateLocked(d)
		s.mu.Unlock()
		if s.vault == nil {
			return "", false, nil
		}
		return s.vault.Get(VaultSettingPrefix + d.Key)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	if v, ok := s.values[d.Key]; ok && settingFits(d, v) {
		return v, true, nil
	}
	if d.Default != nil {
		return d.Default, false, nil
	}
	switch d.Type {
	case SettingBoolean:
		return false, false, nil
	case SettingNumber:
		return 0.0, false, nil
	default:
		return "", false, nil
	}
}

// Set validates and persists text for setting d. Passing "" unsets d.
// Secret settings are routed to Vault ("setting:<key>") and never written to settings.json.
func (s *SettingsStore) Set(d SettingDecl, text string) error {
	var parsed any
	switch {
	case text == "":
		parsed = nil
	case d.Type == SettingBoolean:
		b, err := strconv.ParseBool(text)
		if err != nil {
			return fmt.Errorf("setting %s must be true or false", d.Key)
		}
		parsed = b
	case d.Type == SettingNumber:
		n, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsInf(n, 0) || math.IsNaN(n) {
			return fmt.Errorf("setting %s must be a finite number", d.Key)
		}
		parsed = n
	case d.Type == SettingSelect:
		if !slices.Contains(d.Options, text) {
			return fmt.Errorf("setting %s must be one of its declared options", d.Key)
		}
		parsed = text
	case d.Type == SettingSecret:
		if err := CheckSecretValue(d.Key, text); err != nil {
			return err
		}
		parsed = text
	default:
		if len(text) > MaxSettingTextBytes || !utf8.ValidString(text) {
			return fmt.Errorf("setting %s must be valid UTF-8 of at most %d bytes", d.Key, MaxSettingTextBytes)
		}
		parsed = text
	}

	if d.Type == SettingSecret {
		if s.vault == nil {
			return fmt.Errorf("setting %s: vault is not configured", d.Key)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.loadLocked()
		if parsed == nil {
			if _, err := s.vault.Delete(VaultSettingPrefix + d.Key); err != nil {
				return err
			}
		} else {
			if err := s.vault.Set(VaultSettingPrefix+d.Key, text); err != nil {
				return err
			}
		}
		if _, legacy := s.values[d.Key]; legacy {
			delete(s.values, d.Key)
			_ = s.saveLocked()
		}
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	if parsed == nil {
		delete(s.values, d.Key)
	} else {
		s.values[d.Key] = parsed
	}
	return s.saveLocked()
}

// ForHost returns the /v1/settings payload for m:
// non-secret settings resolve to their typed values, while secret settings are
// masked to {"set": bool} and never expose the underlying secret value.
func (s *SettingsStore) ForHost(m *Manifest) (map[string]any, error) {
	if m == nil {
		return map[string]any{}, nil
	}
	out := make(map[string]any, len(m.Settings))
	for _, d := range m.Settings {
		if d.Type == SettingSecret {
			s.mu.Lock()
			s.loadLocked()
			s.migrateLocked(d)
			s.mu.Unlock()
			isSet := (s.vault != nil && s.vault.Has(VaultSettingPrefix+d.Key)) || s.hasLegacySecret(d.Key)
			out[d.Key] = map[string]bool{"set": isSet}
			continue
		}
		v, _, err := s.Get(d)
		if err != nil {
			return nil, fmt.Errorf("setting %s: %w", d.Key, err)
		}
		out[d.Key] = v
	}
	return out, nil
}

// Summaries returns UI SettingSummary items in manifest declaration order.
// Secret settings never expose their value and report only Set=true/false.
func (s *SettingsStore) Summaries(m *Manifest) []SettingSummary {
	if m == nil {
		return nil
	}
	out := make([]SettingSummary, 0, len(m.Settings))
	for _, d := range m.Settings {
		item := SettingSummary{
			Key:         d.Key,
			Type:        d.Type,
			Label:       d.Label,
			Description: d.Description,
			Options:     slices.Clone(d.Options),
			Default:     d.Default,
		}
		if d.Type == SettingSecret {
			item.Set = (s.vault != nil && s.vault.Has(VaultSettingPrefix+d.Key)) || s.hasLegacySecret(d.Key)
			out = append(out, item)
			continue
		}
		v, isSet, err := s.Get(d)
		if err == nil {
			item.Set = isSet
			item.Value = formatSettingValue(v)
		}
		out = append(out, item)
	}
	return out
}

func formatSettingValue(v any) string {
	switch val := v.(type) {
	case string:
		return val
	case bool:
		return strconv.FormatBool(val)
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64)
	default:
		return ""
	}
}
