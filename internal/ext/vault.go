package ext

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ironpark/gvx-desktop/internal/keychain"
)

const (
	// MaxSecretBytes bounds a secret value (compatible with Windows Credential Manager).
	MaxSecretBytes = 2048
	// MaxSecrets bounds the number of extension-managed secrets ("secret:" namespace).
	MaxSecrets = 32
	// SecretsPerMinute bounds extension Host API secret calls per minute.
	SecretsPerMinute = 60

	// Vault prefixes separate user-configured secret settings from extension runtime secrets.
	VaultSettingPrefix = "setting:"
	VaultSecretPrefix  = "secret:"
)

// ErrRateLimited indicates the per-minute secret rate limit has been exceeded.
var ErrRateLimited = errors.New("secrets rate limit exceeded (60 requests/minute)")

var secretNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// KeychainService returns the OS keychain service name for extension id under dataDir:
// "gvx-desktop extension <id> (<sha256(abs(dataDir))[:8]>)".
func KeychainService(dataDir, id string) string {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		abs = dataDir
	}
	sum := sha256.Sum256([]byte(abs))
	return fmt.Sprintf("gvx-desktop extension %s (%s)", id, hex.EncodeToString(sum[:4]))
}

// CheckSecretName validates a secret name passed to the Host API secrets endpoints.
func CheckSecretName(name string) error {
	if !secretNameRe.MatchString(name) {
		return fmt.Errorf("invalid secret name %q: must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$", name)
	}
	return nil
}

// CheckSecretValue validates that value is valid UTF-8 and at most MaxSecretBytes bytes.
func CheckSecretValue(what, value string) error {
	if len(value) > MaxSecretBytes || !utf8.ValidString(value) {
		return fmt.Errorf("%s must be valid UTF-8 of at most %d bytes", what, MaxSecretBytes)
	}
	return nil
}

// RateLimiter implements a thread-safe sliding-window rate limiter with an injectable clock.
type RateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	now    func() time.Time
	stamps []time.Time
}

// NewRateLimiter creates a RateLimiter allowing limit events per window.
// If now is nil, time.Now is used.
func NewRateLimiter(limit int, window time.Duration, now func() time.Time) *RateLimiter {
	if now == nil {
		now = time.Now
	}
	return &RateLimiter{
		limit:  limit,
		window: window,
		now:    now,
	}
}

// Allow records an attempt if under the limit and reports whether it was allowed.
func (r *RateLimiter) Allow() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.now()
	cutoff := t.Add(-r.window)
	keep := 0
	for _, s := range r.stamps {
		if s.After(cutoff) {
			r.stamps[keep] = s
			keep++
		}
	}
	r.stamps = r.stamps[:keep]
	if len(r.stamps) >= r.limit {
		return false
	}
	r.stamps = append(r.stamps, t)
	return true
}

// Vault stores secrets for a single extension using keychain.Keychain when
// available, or a 0600 secrets.json file fallback when kc == nil.
type Vault struct {
	kc      keychain.Keychain
	dataDir string
	service string
	index   string
	file    string
	rate    *RateLimiter

	mu       sync.Mutex
	loaded   bool
	names    map[string]bool
	values   map[string]string
	fallback map[string]string
}

// NewVault constructs a Vault for extension id under dataDir.
// Pass kc == nil to use the 0600 secrets.json file fallback.
func NewVault(kc keychain.Keychain, dataDir, id string) *Vault {
	return NewVaultWithClock(kc, dataDir, id, nil)
}

// NewVaultWithClock constructs a Vault with an injectable clock for rate-limit testing.
func NewVaultWithClock(kc keychain.Keychain, dataDir, id string, now func() time.Time) *Vault {
	return &Vault{
		kc:      kc,
		dataDir: dataDir,
		service: KeychainService(dataDir, id),
		index:   SecretsIndexPath(dataDir, id),
		file:    SecretsFilePath(dataDir, id),
		rate:    NewRateLimiter(SecretsPerMinute, time.Minute, now),
	}
}

// Service returns the keychain service string used by this Vault.
func (v *Vault) Service() string {
	return v.service
}

// UsingFallback reports whether the Vault is storing secrets in secrets.json
// instead of an OS keychain backend.
func (v *Vault) UsingFallback() bool {
	return v.kc == nil
}

// Reset clears the in-memory cache so the next call reloads from disk/keychain.
func (v *Vault) Reset() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.loaded = false
	v.names = nil
	v.values = nil
	v.fallback = nil
}

func (v *Vault) loadLocked() {
	if v.loaded {
		return
	}
	v.loaded = true
	v.names = map[string]bool{}
	v.values = map[string]string{}
	v.fallback = map[string]string{}

	var fileData map[string]string
	if b, err := ReadFileNoSymlinkWithin(v.dataDir, v.file); err == nil {
		_ = json.Unmarshal(b, &fileData)
	}
	if v.kc == nil {
		for k, val := range fileData {
			v.names[k] = true
			v.values[k] = val
		}
		return
	}

	var names []string
	if b, err := ReadFileNoSymlinkWithin(v.dataDir, v.index); err == nil {
		_ = json.Unmarshal(b, &names)
	}
	for _, n := range names {
		v.names[n] = true
	}
	if len(fileData) == 0 {
		return
	}

	// Lazily migrate secrets from the 0600 fallback file into the keychain.
	left := map[string]string{}
	for k, val := range fileData {
		if err := v.kc.Set(v.service, k, val); err != nil {
			left[k] = val
			continue
		}
		v.names[k] = true
		v.values[k] = val
	}
	if v.saveIndexLocked() != nil {
		v.fallback = fileData
		return
	}
	v.fallback = left
	_ = v.writeFileLocked(left)
}

func (v *Vault) saveIndexLocked() error {
	names := make([]string, 0, len(v.names))
	for n := range v.names {
		names = append(names, n)
	}
	sort.Strings(names)
	b, err := json.Marshal(names)
	if err != nil {
		return err
	}
	return WriteFileAtomicWithin(v.dataDir, v.index, b, 0o600)
}

func (v *Vault) writeFileLocked(values map[string]string) error {
	if len(values) == 0 {
		if err := removeFileNoSymlinkWithin(v.dataDir, v.file); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	b, err := json.Marshal(values)
	if err != nil {
		return err
	}
	return WriteFileAtomicWithin(v.dataDir, v.file, b, 0o600)
}

func (v *Vault) hasLocked(name string) bool {
	if v.names[name] {
		return true
	}
	_, ok := v.fallback[name]
	return ok
}

// Has reports whether the vault holds an entry for full key name (e.g. "setting:foo" or "secret:bar")
// without reading its secret value from the OS keychain.
func (v *Vault) Has(name string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.loadLocked()
	return v.hasLocked(name)
}

// Get retrieves the value for full key name, reconciling keychain.ErrNotFound
// by removing the stale entry from secrets-index.json.
func (v *Vault) Get(name string) (string, bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.loadLocked()
	if val, ok := v.fallback[name]; ok {
		return val, true, nil
	}
	if !v.names[name] {
		return "", false, nil
	}
	if val, ok := v.values[name]; ok {
		return val, true, nil
	}
	if v.kc == nil {
		return "", false, nil
	}
	val, err := v.kc.Get(v.service, name)
	if errors.Is(err, keychain.ErrNotFound) {
		delete(v.names, name)
		_ = v.saveIndexLocked()
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	v.values[name] = val
	return val, true, nil
}

// Set stores value under full key name.
// When using a keychain backend, secrets-index.json is updated before kc.Set and
// rolled back on kc.Set failure so no orphaned secret can exist in the keychain.
func (v *Vault) Set(name, value string) error {
	if err := CheckSecretValue(name, value); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.loadLocked()
	return v.setLocked(name, value)
}

func (v *Vault) setLocked(name, value string) error {
	if v.kc == nil {
		next := maps.Clone(v.values)
		next[name] = value
		if err := v.writeFileLocked(next); err != nil {
			return err
		}
		v.names[name] = true
		v.values[name] = value
		return nil
	}
	had := v.names[name]
	v.names[name] = true
	if err := v.saveIndexLocked(); err != nil {
		if !had {
			delete(v.names, name)
		}
		return err
	}
	if err := v.kc.Set(v.service, name, value); err != nil {
		if !had {
			delete(v.names, name)
			_ = v.saveIndexLocked()
		}
		delete(v.values, name)
		return err
	}
	v.values[name] = value
	if _, inFallback := v.fallback[name]; inFallback {
		next := maps.Clone(v.fallback)
		delete(next, name)
		if err := v.writeFileLocked(next); err == nil {
			v.fallback = next
		}
	}
	return nil
}

// Delete removes full key name from the vault and updates secrets-index.json or secrets.json.
func (v *Vault) Delete(name string) (bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.loadLocked()
	inNames := v.names[name]
	_, inFallback := v.fallback[name]
	if !inNames && !inFallback {
		return false, nil
	}
	if v.kc == nil {
		next := maps.Clone(v.values)
		delete(next, name)
		if err := v.writeFileLocked(next); err != nil {
			return false, err
		}
		delete(v.names, name)
		delete(v.values, name)
		return true, nil
	}
	if inNames {
		if err := v.kc.Delete(v.service, name); err != nil && !errors.Is(err, keychain.ErrNotFound) {
			return false, err
		}
		delete(v.names, name)
		if err := v.saveIndexLocked(); err != nil {
			return true, err
		}
	}
	delete(v.values, name)
	if inFallback {
		next := maps.Clone(v.fallback)
		delete(next, name)
		if err := v.writeFileLocked(next); err != nil {
			return true, err
		}
		v.fallback = next
	}
	return true, nil
}

func (v *Vault) listLocked(prefix string) []string {
	seen := make(map[string]bool, len(v.names)+len(v.fallback))
	var out []string
	for n := range v.names {
		if rest, ok := strings.CutPrefix(n, prefix); ok && !seen[rest] {
			seen[rest] = true
			out = append(out, rest)
		}
	}
	for n := range v.fallback {
		if rest, ok := strings.CutPrefix(n, prefix); ok && !seen[rest] {
			seen[rest] = true
			out = append(out, rest)
		}
	}
	slices.Sort(out)
	return out
}

// List returns the sorted key names having prefix, with prefix stripped.
func (v *Vault) List(prefix string) []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.loadLocked()
	return v.listLocked(prefix)
}

// Purge deletes every indexed secret from the keychain and cleans up the
// index and fallback files on full success. If any keychain delete fails,
// remaining un-deleted names stay indexed and the first error is returned.
func (v *Vault) Purge() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.loadLocked()
	var firstErr error
	if v.kc != nil {
		for n := range v.names {
			if err := v.kc.Delete(v.service, n); err != nil && !errors.Is(err, keychain.ErrNotFound) {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			delete(v.names, n)
			delete(v.values, n)
		}
		if len(v.fallback) > 0 {
			if err := v.writeFileLocked(nil); err != nil && firstErr == nil {
				firstErr = err
			} else if err == nil {
				v.fallback = map[string]string{}
			}
		}
	} else {
		v.names = map[string]bool{}
		v.values = map[string]string{}
	}
	if firstErr != nil {
		_ = v.saveIndexLocked()
		return firstErr
	}
	if err := removeFileNoSymlinkWithin(v.dataDir, v.index); err != nil && !os.IsNotExist(err) && firstErr == nil {
		firstErr = err
	}
	if err := removeFileNoSymlinkWithin(v.dataDir, v.file); err != nil && !os.IsNotExist(err) && firstErr == nil {
		firstErr = err
	}
	v.loaded = false
	v.names = nil
	v.values = nil
	v.fallback = nil
	return firstErr
}

// GetSecret enforces secret-name validation and the 60 req/min rate limit,
// then reads from the "secret:" namespace.
func (v *Vault) GetSecret(name string) (string, bool, error) {
	if err := CheckSecretName(name); err != nil {
		return "", false, err
	}
	if !v.rate.Allow() {
		return "", false, ErrRateLimited
	}
	return v.Get(VaultSecretPrefix + name)
}

// GetSettingSecret enforces key-name validation and the 60 req/min rate limit,
// then reads from the "setting:" namespace.
func (v *Vault) GetSettingSecret(name string) (string, bool, error) {
	if err := CheckSecretName(name); err != nil {
		return "", false, err
	}
	if !v.rate.Allow() {
		return "", false, ErrRateLimited
	}
	return v.Get(VaultSettingPrefix + name)
}

// SetSecret enforces secret-name/value validation, the 60 req/min rate limit,
// and the MaxSecrets (32) cap on the "secret:" namespace.
func (v *Vault) SetSecret(name, value string) error {
	if err := CheckSecretName(name); err != nil {
		return err
	}
	if err := CheckSecretValue(name, value); err != nil {
		return err
	}
	if !v.rate.Allow() {
		return ErrRateLimited
	}
	fullKey := VaultSecretPrefix + name
	v.mu.Lock()
	defer v.mu.Unlock()
	v.loadLocked()
	if !v.hasLocked(fullKey) && len(v.listLocked(VaultSecretPrefix)) >= MaxSecrets {
		return fmt.Errorf("maximum of %d extension secrets reached", MaxSecrets)
	}
	return v.setLocked(fullKey, value)
}

// DeleteSecret enforces secret-name validation and the 60 req/min rate limit,
// then deletes from the "secret:" namespace.
func (v *Vault) DeleteSecret(name string) (bool, error) {
	if err := CheckSecretName(name); err != nil {
		return false, err
	}
	if !v.rate.Allow() {
		return false, ErrRateLimited
	}
	return v.Delete(VaultSecretPrefix + name)
}

// ListSecrets enforces the 60 req/min rate limit and returns sorted names in
// the "secret:" namespace.
func (v *Vault) ListSecrets() ([]string, error) {
	if !v.rate.Allow() {
		return nil, ErrRateLimited
	}
	return v.List(VaultSecretPrefix), nil
}

// SecretStoreStatusFor returns where secrets are stored ("file" or keychain backend name).
func SecretStoreStatusFor(kc keychain.Keychain, note string) SecretStoreStatus {
	if kc != nil {
		return SecretStoreStatus{Store: kc.Name()}
	}
	return SecretStoreStatus{Store: "file", Detail: note}
}
