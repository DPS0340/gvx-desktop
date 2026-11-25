package ext

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/keychain"
)

func TestVaultKeychainMigrationLimitsAndPurge(t *testing.T) {
	dataDir1 := t.TempDir()
	dataDir2 := t.TempDir()

	svc1 := KeychainService(dataDir1, "demo-ext")
	svc2 := KeychainService(dataDir2, "demo-ext")
	if !strings.HasPrefix(svc1, "gvx-desktop extension demo-ext (") || !strings.HasSuffix(svc1, ")") {
		t.Fatalf("unexpected KeychainService format: %q", svc1)
	}
	if svc1 == svc2 {
		t.Fatalf("two distinct dataDirs produced identical KeychainService %q", svc1)
	}

	// 1. File fallback mode (kc == nil) writes 0600 secrets.json.
	vFile := NewVault(nil, dataDir1, "demo-ext")
	if !vFile.UsingFallback() {
		t.Fatal("UsingFallback() = false when kc == nil")
	}
	if st := SecretStoreStatusFor(nil, "no keychain"); st.Store != "file" || st.Detail != "no keychain" {
		t.Fatalf("SecretStoreStatusFor(nil) = %+v", st)
	}
	if err := vFile.SetSecret("api_key", "super-secret-1"); err != nil {
		t.Fatalf("SetSecret(file): %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(SecretsFilePath(dataDir1, "demo-ext"))
		if err != nil {
			t.Fatalf("Stat(secrets.json): %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("secrets.json perm = %04o, want 0600", info.Mode().Perm())
		}
	}

	// 2. Re-opening with keychain.Fake lazily migrates secrets.json and deletes it.
	fakeKC := &keychain.Fake{}
	if st := SecretStoreStatusFor(fakeKC, ""); st.Store != fakeKC.Name() {
		t.Fatalf("SecretStoreStatusFor(fake) = %+v", st)
	}
	vKC := NewVault(fakeKC, dataDir1, "demo-ext")
	val, ok, err := vKC.GetSecret("api_key")
	if err != nil || !ok || val != "super-secret-1" {
		t.Fatalf("GetSecret after migration = (%q, %v, %v)", val, ok, err)
	}
	if _, err := os.Stat(SecretsFilePath(dataDir1, "demo-ext")); !os.IsNotExist(err) {
		t.Fatalf("secrets.json should be removed after migration, stat err=%v", err)
	}

	// 3. Keychain failure on new key rolls back index (no phantom index entry).
	fakeKC.Fail = errors.New("keychain locked")
	if err := vKC.SetSecret("should_not_persist", "val"); err == nil {
		t.Fatal("expected SetSecret error when kc.Fail is set")
	}
	if vKC.Has(VaultSecretPrefix + "should_not_persist") {
		t.Fatal("phantom index entry left behind after kc.Set failure")
	}
	fakeKC.Fail = nil

	// 4. Stale index entry when secret removed externally from keychain is pruned on Get.
	if err := fakeKC.Delete(vKC.Service(), VaultSecretPrefix+"api_key"); err != nil {
		t.Fatal(err)
	}
	vKC.Reset()
	if _, ok, err := vKC.GetSecret("api_key"); err != nil || ok {
		t.Fatalf("expected pruned missing secret, got (ok=%v, err=%v)", ok, err)
	}
	if vKC.Has(VaultSecretPrefix + "api_key") {
		t.Fatal("stale index entry was not pruned on ErrNotFound")
	}

	// 5. Enforce MaxSecretBytes (2048B) and MaxSecrets (32) in secret: namespace.
	if err := vKC.SetSecret("too_big", strings.Repeat("x", MaxSecretBytes+1)); err == nil {
		t.Fatal("expected error for >2048B secret")
	}
	// setting: namespace does not count toward the 32 secret: limit.
	if err := vKC.Set(VaultSettingPrefix+"pat", "ghp_123"); err != nil {
		t.Fatalf("Set(setting:pat): %v", err)
	}
	for i := 0; i < MaxSecrets; i++ {
		if err := vKC.SetSecret(fmt.Sprintf("k_%02d", i), "v"); err != nil {
			t.Fatalf("SetSecret(%d): %v", i, err)
		}
	}
	if err := vKC.SetSecret("k_33rd", "v"); err == nil {
		t.Fatal("expected error on 33rd secret")
	}

	// 6. Another extension's secrets in the same Fake keychain must survive Purge().
	vOther := NewVault(fakeKC, dataDir1, "other-ext")
	if err := vOther.SetSecret("keep_me", "alive"); err != nil {
		t.Fatal(err)
	}

	if err := vKC.Purge(); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if got, ok, _ := vOther.GetSecret("keep_me"); !ok || got != "alive" {
		t.Fatalf("other-ext secret affected by demo-ext Purge: (%q, %v)", got, ok)
	}
	for _, entry := range fakeKC.Entries() {
		if entry[0] == vKC.Service() {
			t.Fatalf("demo-ext entry %v remained in keychain after Purge", entry)
		}
	}
}

func TestVaultRateLimiterWithClock(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fakeKC := &keychain.Fake{}
	v := NewVaultWithClock(fakeKC, t.TempDir(), "rate-ext", func() time.Time { return now })

	for i := 0; i < SecretsPerMinute; i++ {
		if _, err := v.ListSecrets(); err != nil {
			t.Fatalf("call %d failed unexpectedly: %v", i+1, err)
		}
	}
	if _, err := v.ListSecrets(); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("61st call error = %v, want ErrRateLimited", err)
	}

	now = now.Add(61 * time.Second)
	if _, err := v.ListSecrets(); err != nil {
		t.Fatalf("call after window expired failed: %v", err)
	}
}

func TestSettingsStoreMaskingTypesAndLegacyMigration(t *testing.T) {
	dataDir := t.TempDir()
	fakeKC := &keychain.Fake{}
	vault := NewVault(fakeKC, dataDir, "settings-ext")
	settingsPath := SettingsPath(dataDir, "settings-ext")
	ss := NewSettingsStore(settingsPath, vault)

	m, err := ParseManifest([]byte(`
schema_version = 1
id = "settings-ext"
version = "1.0.0"
publisher = "acme"
name = "Settings Ext"

[contributes.settings]
endpoint = { type = "string", label = "Endpoint", default = "https://example.com" }
enabled = { type = "boolean", default = false }
retries = { type = "number", default = 3 }
mode = { type = "select", options = ["fast", "safe"], default = "safe" }
api_token = { type = "secret", label = "API Token" }
`))
	if err != nil {
		t.Fatal(err)
	}

	// Set typed values and a secret.
	if err := ss.Set(m.Settings[0], "https://api.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := ss.Set(m.Settings[1], "true"); err != nil {
		t.Fatal(err)
	}
	if err := ss.Set(m.Settings[2], "7"); err != nil {
		t.Fatal(err)
	}
	if err := ss.Set(m.Settings[3], "fast"); err != nil {
		t.Fatal(err)
	}
	if err := ss.Set(m.Settings[4], "secret-pat-value"); err != nil {
		t.Fatal(err)
	}

	// Invalid inputs rejected.
	if err := ss.Set(m.Settings[1], "maybe"); err == nil {
		t.Fatal("expected boolean parse error")
	}
	if err := ss.Set(m.Settings[2], "NaN"); err == nil {
		t.Fatal("expected NaN rejection")
	}
	if err := ss.Set(m.Settings[3], "invalid-option"); err == nil {
		t.Fatal("expected select option rejection")
	}

	// settings.json must NEVER contain the secret value.
	rawSettings, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawSettings), "secret-pat-value") || strings.Contains(string(rawSettings), "api_token") {
		t.Fatalf("settings.json leaked secret setting: %s", rawSettings)
	}

	// ForHost masks secret to {"set": true}.
	hostMap, err := ss.ForHost(m)
	if err != nil {
		t.Fatalf("ForHost: %v", err)
	}
	if hostMap["endpoint"] != "https://api.example.com" || hostMap["enabled"] != true || hostMap["retries"] != 7.0 || hostMap["mode"] != "fast" {
		t.Fatalf("unexpected host settings: %+v", hostMap)
	}
	secretObj, ok := hostMap["api_token"].(map[string]bool)
	if !ok || !secretObj["set"] {
		t.Fatalf("expected api_token={set:true}, got %#v", hostMap["api_token"])
	}

	// Summaries preserve manifest order and mask secret value.
	summaries := ss.Summaries(m)
	gotKeys := make([]string, len(summaries))
	for i, s := range summaries {
		gotKeys[i] = s.Key
	}
	if !slices.Equal(gotKeys, []string{"endpoint", "enabled", "retries", "mode", "api_token"}) {
		t.Fatalf("summary order = %v", gotKeys)
	}
	if !summaries[4].Set || summaries[4].Value != "" {
		t.Fatalf("secret summary leaked or wrong Set: %+v", summaries[4])
	}

	// Legacy secret migration out of settings.json into Vault.
	legacyDir := t.TempDir()
	legacyVault := NewVault(&keychain.Fake{}, legacyDir, "settings-ext")
	legacyPath := SettingsPath(legacyDir, "settings-ext")
	if err := WriteFileAtomic(legacyPath, []byte(`{"endpoint":"x","api_token":"legacy-pat"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	legacySS := NewSettingsStore(legacyPath, legacyVault)
	legacySS.MigrateLegacySecrets(m)

	gotSecret, ok, err := legacyVault.Get(VaultSettingPrefix + "api_token")
	if err != nil || !ok || gotSecret != "legacy-pat" {
		t.Fatalf("legacy migration failed: (%q, %v, %v)", gotSecret, ok, err)
	}
	afterRaw, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(afterRaw), "legacy-pat") || strings.Contains(string(afterRaw), "api_token") {
		t.Fatalf("legacy secret still present in settings.json: %s", afterRaw)
	}
}

type selectiveFailKeychain struct {
	inner   keychain.Fake
	failSet func(service, account string) error
}

func (s *selectiveFailKeychain) Name() string { return s.inner.Name() }
func (s *selectiveFailKeychain) Get(service, account string) (string, error) {
	return s.inner.Get(service, account)
}
func (s *selectiveFailKeychain) Set(service, account, value string) error {
	if s.failSet != nil {
		if err := s.failSet(service, account); err != nil {
			return err
		}
	}
	return s.inner.Set(service, account, value)
}
func (s *selectiveFailKeychain) Delete(service, account string) error {
	return s.inner.Delete(service, account)
}

func TestVaultPartialMigrationSameSessionVisibility(t *testing.T) {
	dataDir := t.TempDir()
	vFile := NewVault(nil, dataDir, "partial-ext")
	if err := vFile.SetSecret("k1", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := vFile.SetSecret("k2", "v2"); err != nil {
		t.Fatal(err)
	}

	failK2 := true
	kc := &selectiveFailKeychain{
		failSet: func(_, account string) error {
			if failK2 && account == VaultSecretPrefix+"k2" {
				return errors.New("transient keychain failure for k2")
			}
			return nil
		},
	}

	v := NewVault(kc, dataDir, "partial-ext")
	// Unmigrated k2 must be visible, readable, and listable in the same session.
	if !v.Has(VaultSecretPrefix + "k2") {
		t.Fatal("Has(secret:k2) = false after partial migration")
	}
	val, ok, err := v.GetSecret("k2")
	if err != nil || !ok || val != "v2" {
		t.Fatalf("GetSecret(k2) = (%q, %v, %v), want (v2, true, nil)", val, ok, err)
	}
	list, err := v.ListSecrets()
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if !slices.Equal(list, []string{"k1", "k2"}) {
		t.Fatalf("ListSecrets = %v, want [k1 k2]", list)
	}

	// Deleting an unmigrated fallback secret in the same session cleans up secrets.json.
	deleted, err := v.DeleteSecret("k2")
	if err != nil || !deleted {
		t.Fatalf("DeleteSecret(k2) = (%v, %v), want (true, nil)", deleted, err)
	}
	if _, err := os.Stat(SecretsFilePath(dataDir, "partial-ext")); !os.IsNotExist(err) {
		t.Fatalf("secrets.json should be removed after deleting last fallback entry, err=%v", err)
	}

	// Also verify SetSecret overwriting a fallback entry after keychain recovers clears secrets.json.
	dataDir2 := t.TempDir()
	vFile2 := NewVault(nil, dataDir2, "partial-ext")
	if err := vFile2.SetSecret("k1", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := vFile2.SetSecret("k2", "v2"); err != nil {
		t.Fatal(err)
	}
	failK2 = true
	v2 := NewVault(kc, dataDir2, "partial-ext")
	if _, ok, err := v2.GetSecret("k2"); err != nil || !ok {
		t.Fatalf("expected k2 visible before overwrite, ok=%v err=%v", ok, err)
	}
	failK2 = false
	if err := v2.SetSecret("k2", "v2-migrated"); err != nil {
		t.Fatalf("SetSecret(k2) after recovery: %v", err)
	}
	if _, err := os.Stat(SecretsFilePath(dataDir2, "partial-ext")); !os.IsNotExist(err) {
		t.Fatalf("secrets.json should be removed after SetSecret migrated k2, err=%v", err)
	}
	if got, ok, err := v2.GetSecret("k2"); err != nil || !ok || got != "v2-migrated" {
		t.Fatalf("GetSecret(k2) after overwrite = (%q, %v, %v)", got, ok, err)
	}
}

func TestVaultSetSecretMaxSecretsConcurrency(t *testing.T) {
	for iter := 0; iter < 5; iter++ {
		fakeKC := &keychain.Fake{}
		now := time.Unix(1_700_000_000, 0)
		// Use a fresh rate-limiter-friendly clock or enough headroom: 31 + 10 = 41 < 60/min.
		v := NewVaultWithClock(fakeKC, t.TempDir(), fmt.Sprintf("race-ext-%d", iter), func() time.Time { return now })
		for i := 0; i < MaxSecrets-1; i++ {
			if err := v.SetSecret(fmt.Sprintf("pre_%02d", i), "val"); err != nil {
				t.Fatalf("setup SetSecret(%d): %v", i, err)
			}
		}

		const workers = 10
		var succeeded atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				<-start
				if err := v.SetSecret(fmt.Sprintf("race_%d", idx), "val"); err == nil {
					succeeded.Add(1)
				}
			}(w)
		}
		close(start)
		wg.Wait()

		if got := succeeded.Load(); got != 1 {
			t.Fatalf("iter %d: %d concurrent SetSecrets succeeded at 31/32, want exactly 1", iter, got)
		}
		all, err := v.ListSecrets()
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != MaxSecrets {
			t.Fatalf("iter %d: total secrets = %d, want %d", iter, len(all), MaxSecrets)
		}
	}
}

func TestVaultGetSettingSecret(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fakeKC := &keychain.Fake{}
	v := NewVaultWithClock(fakeKC, t.TempDir(), "setting-secret-ext", func() time.Time { return now })

	if err := v.Set(VaultSettingPrefix+"api_token", "pat-123"); err != nil {
		t.Fatalf("Set(setting:api_token): %v", err)
	}
	// Must not appear in ListSecrets or consume secret: capacity.
	list, err := v.ListSecrets()
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("ListSecrets = %v, want empty (setting: must be isolated from secret:)", list)
	}

	val, ok, err := v.GetSettingSecret("api_token")
	if err != nil || !ok || val != "pat-123" {
		t.Fatalf("GetSettingSecret(api_token) = (%q, %v, %v), want (pat-123, true, nil)", val, ok, err)
	}
	if _, ok, err := v.GetSettingSecret("unset_key"); err != nil || ok {
		t.Fatalf("GetSettingSecret(unset_key) = (ok=%v, err=%v), want (false, nil)", ok, err)
	}
	for _, bad := range []string{"", "bad:key", "../etc", "a/b"} {
		if _, _, err := v.GetSettingSecret(bad); err == nil {
			t.Fatalf("GetSettingSecret(%q) succeeded, want validation error", bad)
		}
	}

	// Shared 60/min rate limit applies to GetSettingSecret.
	// Note: 1 ListSecrets + 2 valid GetSettingSecret calls already recorded (3/60).
	for i := 0; i < SecretsPerMinute-3; i++ {
		if _, _, err := v.GetSettingSecret("api_token"); err != nil {
			t.Fatalf("GetSettingSecret call %d failed unexpectedly: %v", i+1, err)
		}
	}
	if _, _, err := v.GetSettingSecret("api_token"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("61st call error = %v, want ErrRateLimited", err)
	}
}

func TestVaultPurge_RejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		return
	}

	// Case 1: symlinked extension-data/<id> with kc == nil must not unlink outside secrets.json or secrets-index.json.
	dataDir := t.TempDir()
	outside := t.TempDir()
	outsideIndex := filepath.Join(outside, "secrets-index.json")
	outsideFile := filepath.Join(outside, "secrets.json")
	if err := os.WriteFile(outsideIndex, []byte(`["secret:keep"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outsideFile, []byte(`{"secret:keep":"dont-delete"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	extDataRoot := ExtensionDataRoot(dataDir)
	if err := os.MkdirAll(extDataRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, ExtensionDataDir(dataDir, "sym-vault")); err != nil {
		t.Fatal(err)
	}

	vFile := NewVault(nil, dataDir, "sym-vault")
	if err := vFile.Purge(); err == nil {
		t.Fatal("Vault.Purge(kc=nil) succeeded through symlinked extension-data/<id>")
	}
	if _, err := os.Stat(outsideIndex); err != nil {
		t.Fatalf("outside secrets-index.json was affected: %v", err)
	}
	if _, err := os.Stat(outsideFile); err != nil {
		t.Fatalf("outside secrets.json was affected: %v", err)
	}

	// Case 2: symlinked extension-data/<id> with keychain backend must not unlink outside files.
	vKC := NewVault(&keychain.Fake{}, dataDir, "sym-vault")
	if err := vKC.Purge(); err == nil {
		t.Fatal("Vault.Purge(kc!=nil) succeeded through symlinked extension-data/<id>")
	}
	if _, err := os.Stat(outsideIndex); err != nil {
		t.Fatalf("outside secrets-index.json was deleted by kc Vault.Purge: %v", err)
	}
	if _, err := os.Stat(outsideFile); err != nil {
		t.Fatalf("outside secrets.json was deleted by kc Vault.Purge: %v", err)
	}

	// Case 3: symlinked leaf secrets-index.json / secrets.json must not be unlinked or followed.
	dataDir2 := t.TempDir()
	outside2 := t.TempDir()
	target2 := filepath.Join(outside2, "keep.json")
	if err := os.WriteFile(target2, []byte(`keep`), 0o600); err != nil {
		t.Fatal(err)
	}
	extDir2 := ExtensionDataDir(dataDir2, "sym-leaf")
	if err := os.MkdirAll(extDir2, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target2, SecretsIndexPath(dataDir2, "sym-leaf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target2, SecretsFilePath(dataDir2, "sym-leaf")); err != nil {
		t.Fatal(err)
	}
	if err := NewVault(nil, dataDir2, "sym-leaf").Purge(); err == nil {
		t.Fatal("Vault.Purge succeeded with symlink leaf")
	}
	if _, err := os.Stat(target2); err != nil {
		t.Fatalf("outside leaf target missing: %v", err)
	}
}

type failingVaultKeychain struct {
	inner      keychain.Fake
	failSet    bool
	failDelete bool
}

func (f *failingVaultKeychain) Name() string { return f.inner.Name() }
func (f *failingVaultKeychain) Get(service, account string) (string, error) {
	return f.inner.Get(service, account)
}
func (f *failingVaultKeychain) Set(service, account, value string) error {
	if f.failSet {
		return errors.New("simulated keychain write failure")
	}
	return f.inner.Set(service, account, value)
}
func (f *failingVaultKeychain) Delete(service, account string) error {
	if f.failDelete {
		return errors.New("simulated keychain delete failure")
	}
	return f.inner.Delete(service, account)
}

func TestSettingsStore_LegacySecretPreservedOnVaultFailure(t *testing.T) {
	dataDir := t.TempDir()
	extID := "sec-legacy-loss"
	settingsPath := SettingsPath(dataDir, extID)
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, []byte(`{"api_key":"legacy-secret-123"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	kc := &failingVaultKeychain{failSet: true, failDelete: true}
	vault := NewVault(kc, dataDir, extID)
	store := NewSettingsStore(settingsPath, vault)
	decl := SettingDecl{Key: "api_key", Type: SettingSecret, Label: "API Key"}
	m := &Manifest{ID: extID, Settings: []SettingDecl{decl}}

	// 1. Vault Set failure must preserve legacy secret in memory and settings.json.
	if err := store.Set(decl, "new-secret-456"); err == nil {
		t.Fatal("expected Set to fail when keychain Set fails")
	}
	if !store.hasLegacySecret("api_key") {
		t.Fatal("legacy secret lost in SettingsStore after Set failure")
	}
	if sums := store.Summaries(m); len(sums) != 1 || !sums[0].Set {
		t.Fatalf("expected Summaries to still report secret set, got %+v", sums)
	}
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "legacy-secret-123") {
		t.Fatalf("legacy secret wiped from settings.json after Set failure: %s", string(raw))
	}

	// 2. Seed vault entry while preserving legacy settings.json, then verify Vault Delete failure also preserves legacy secret.
	kc.failSet = false
	if err := vault.Set(VaultSettingPrefix+"api_key", "vault-copy"); err != nil {
		t.Fatalf("seed vault copy: %v", err)
	}
	if err := os.WriteFile(settingsPath, []byte(`{"api_key":"legacy-secret-123"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.Reset()
	if err := store.Set(decl, ""); err == nil {
		t.Fatal("expected Set(\"\") to fail when keychain Delete fails")
	}
	raw, err = os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "legacy-secret-123") {
		t.Fatalf("legacy secret wiped from settings.json after Delete failure: %s", string(raw))
	}

	// 3. Once vault recovers, Set succeeds and migrates legacy key out of settings.json.
	kc.failSet = false
	if err := store.Set(decl, "new-secret-456"); err != nil {
		t.Fatalf("Set after keychain recovery: %v", err)
	}
	gotVal, ok, err := store.Get(decl)
	if err != nil || !ok || gotVal != "new-secret-456" {
		t.Fatalf("expected updated secret after recovery, got (%v, %v, %v)", gotVal, ok, err)
	}
	raw, err = os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "legacy-secret-123") || strings.Contains(string(raw), "api_key") {
		t.Fatalf("legacy secret not cleaned up after successful Set: %s", string(raw))
	}
}
