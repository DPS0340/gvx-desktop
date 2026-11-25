package ext

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// newNonReservedTestDir returns a temporary directory that passes
// validateAllowedFolder (avoiding reserved guest paths like /tmp on Linux).
func newNonReservedTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if canonical, err := validateAllowedFolder(dir); err == nil {
		return canonical
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Fatalf("cannot create non-reserved test dir (temp dir %q is reserved): %v", dir, err)
	}
	custom, err := os.MkdirTemp(home, "gvx-ext-test-*")
	if err != nil {
		t.Fatalf("os.MkdirTemp in home %q: %v", home, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(custom) })
	canonical, err := validateAllowedFolder(custom)
	if err != nil {
		t.Fatalf("validateAllowedFolder(%q): %v", custom, err)
	}
	return canonical
}

func TestPermissionsCatalog(t *testing.T) {
	got, err := CheckPermissions([]string{PermDocker, PermNetwork, PermStorage})
	if err != nil {
		t.Fatalf("CheckPermissions valid failed: %v", err)
	}
	if !slices.Equal(got, []string{PermDocker, PermNetwork, PermStorage}) {
		t.Fatalf("got %v", got)
	}
	for _, imp := range ImplicitPermissions {
		if _, err := CheckPermissions([]string{imp}); err == nil {
			t.Errorf("CheckPermissions(%q) succeeded, want implicit rejection", imp)
		}
	}
	if PermissionRisk(PermDocker) != "high" {
		t.Errorf("PermDocker risk = %q, want high", PermissionRisk(PermDocker))
	}
	if PermissionDescription(PermStorage) == "" {
		t.Error("PermStorage description is empty")
	}
}

func TestGrantStoreApproveDenyEscalationAndFolders(t *testing.T) {
	dataDir := t.TempDir()
	grantsPath := GrantsPath(dataDir)

	gs, err := LoadGrantStore(grantsPath)
	if err != nil {
		t.Fatalf("LoadGrantStore(empty): %v", err)
	}

	m1, err := ParseManifest([]byte(`
schema_version = 1
id = "demo-ext"
version = "0.1.0"
publisher = "acme"
name = "Demo"
permissions = ["storage", "docker"]
`))
	if err != nil {
		t.Fatal(err)
	}

	// Never approved -> waiting.
	waiting, denied, asked := NeedsApproval(m1, gs.Get(m1.ID), "commit-1")
	if !waiting || denied || len(asked) != 2 {
		t.Fatalf("initial NeedsApproval = (%v, %v, %v), want waiting=true", waiting, denied, asked)
	}

	// Deny -> denied.
	if err := gs.Deny(m1.ID, m1); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	waiting, denied, _ = NeedsApproval(m1, gs.Get(m1.ID), "commit-1")
	if waiting || !denied {
		t.Fatalf("after Deny: (%v, %v), want (false, true)", waiting, denied)
	}

	// Approve -> neither waiting nor denied.
	if err := gs.Approve(m1.ID, m1, "commit-1"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	waiting, denied, asked = NeedsApproval(m1, gs.Get(m1.ID), "commit-1")
	if waiting || denied || len(asked) != 0 {
		t.Fatalf("after Approve: (%v, %v, %v), want (false, false, nil)", waiting, denied, asked)
	}
	if !gs.HasPermission(m1.ID, PermDocker, m1.Permissions) {
		t.Fatal("HasPermission(docker) = false, want true")
	}
	if gs.HasPermission(m1.ID, PermSecrets, m1.Permissions) {
		t.Fatal("HasPermission(secrets) = true, want false")
	}

	// Escalation 1: new git commit SHA -> waiting.
	waiting, _, _ = NeedsApproval(m1, gs.Get(m1.ID), "commit-2")
	if !waiting {
		t.Fatal("expected waiting=true when commit SHA changed")
	}

	// Escalation 2: new permission added -> waiting.
	m2, err := ParseManifest([]byte(`
schema_version = 1
id = "demo-ext"
version = "0.2.0"
publisher = "acme"
name = "Demo"
permissions = ["storage", "docker", "secrets"]
`))
	if err != nil {
		t.Fatal(err)
	}
	waiting, _, asked = NeedsApproval(m2, gs.Get(m1.ID), "commit-1")
	if !waiting || !slices.Equal(asked, []string{"secrets"}) {
		t.Fatalf("expected waiting=true asked=[secrets], got (%v, %v)", waiting, asked)
	}

	// Disabled & AllowedFolders.
	if err := gs.SetDisabled(m1.ID, true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	if !gs.Get(m1.ID).Disabled {
		t.Fatal("expected Disabled=true")
	}

	projectDir := newNonReservedTestDir(t)
	canonical, err := gs.AddAllowedFolder(m1.ID, projectDir)
	if err != nil {
		t.Fatalf("AddAllowedFolder: %v", err)
	}
	// Duplicate add should be idempotent.
	if _, err := gs.AddAllowedFolder(m1.ID, projectDir); err != nil {
		t.Fatalf("AddAllowedFolder duplicate: %v", err)
	}
	if got := gs.Get(m1.ID).AllowedFolders; len(got) != 1 || got[0] != canonical {
		t.Fatalf("AllowedFolders = %v, want [%s]", got, canonical)
	}
	// Reject relative, non-existent, and reserved paths.
	if _, err := gs.AddAllowedFolder(m1.ID, "relative/dir"); err == nil {
		t.Fatal("AddAllowedFolder accepted relative path")
	}
	if _, err := gs.AddAllowedFolder(m1.ID, filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("AddAllowedFolder accepted missing path")
	}
	if runtime.GOOS != "windows" {
		if _, err := gs.AddAllowedFolder(m1.ID, "/etc"); err == nil {
			t.Fatal("AddAllowedFolder accepted reserved /etc path")
		}
	}
	if runtime.GOOS == "darwin" {
		if _, err := gs.AddAllowedFolder(m1.ID, "/private"); err == nil {
			t.Fatal("AddAllowedFolder accepted reserved /private path on darwin")
		}
	}

	removed, err := gs.RemoveAllowedFolder(m1.ID, projectDir)
	if err != nil || !removed {
		t.Fatalf("RemoveAllowedFolder = (%v, %v), want (true, nil)", removed, err)
	}
	if len(gs.Get(m1.ID).AllowedFolders) != 0 {
		t.Fatal("expected AllowedFolders to be empty after removal")
	}

	// Persistence across reload.
	gs2, err := LoadGrantStore(grantsPath)
	if err != nil {
		t.Fatalf("LoadGrantStore(reload): %v", err)
	}
	if !gs2.Get(m1.ID).Disabled || gs2.Get(m1.ID).ApprovedCommit != "commit-1" {
		t.Fatalf("reloaded GrantEntry mismatch: %+v", gs2.Get(m1.ID))
	}

	// RevokePermissions clears granted & approval while keeping Disabled.
	if err := gs2.RevokePermissions(m1.ID); err != nil {
		t.Fatalf("RevokePermissions: %v", err)
	}
	e := gs2.Get(m1.ID)
	if !e.Disabled || len(e.Granted) != 0 || e.ApprovedManifestHash != "" {
		t.Fatalf("unexpected entry after RevokePermissions: %+v", e)
	}

	// Bundled extensions never wait.
	bundled, err := ParseBundledManifest([]byte(`
schema_version = 1
id = "gvx-core"
version = "1.0.0"
publisher = "gvx"
name = "GVX Core"
permissions = ["docker"]
`))
	if err != nil {
		t.Fatal(err)
	}
	if w, d, _ := NeedsApproval(bundled, GrantEntry{}, ""); w || d {
		t.Fatalf("bundled extension should not wait or be denied: (%v, %v)", w, d)
	}
}

func TestGrantStoreCorruptFileWriteProtection(t *testing.T) {
	path := GrantsPath(t.TempDir())
	corrupt := []byte("{not valid json")
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}

	gs, err := LoadGrantStore(path)
	if err == nil || gs.LoadError() == nil {
		t.Fatal("expected load error for corrupt grants file")
	}
	if err := gs.SetDisabled("demo-ext", true); err == nil {
		t.Fatal("expected SetDisabled to refuse overwriting corrupt grants file")
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || string(after) != string(corrupt) {
		t.Fatalf("corrupt grants file was modified: %q", string(after))
	}
}
