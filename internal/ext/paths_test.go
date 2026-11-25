package ext

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPathsUnderDataDirNeverHomeGVX(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "gvx-desktop")
	p := NewPaths(dataDir)
	id := "github-actions"

	all := []string{
		p.GrantsFile(),
		p.ExtensionsDir(),
		p.ExtensionDir(id),
		p.ManifestSnapshot(id),
		p.SourceFile(id),
		p.SourceCheckoutDir(id),
		p.DataRootDir(),
		p.ExtDataDir(id),
		p.SettingsFile(id),
		p.StorageFile(id),
		p.SecretsIndexFile(id),
		p.SecretsFile(id),
		p.FilesDir(id),
		p.LogsDir(id),
	}
	for _, got := range all {
		if !strings.HasPrefix(got, dataDir+string(filepath.Separator)) {
			t.Errorf("path %q is not under dataDir %q", got, dataDir)
		}
		if strings.Contains(got, ".gvx") {
			t.Errorf("path %q unexpectedly references .gvx", got)
		}
	}
}

func TestValidateID(t *testing.T) {
	valid := []string{
		"a",
		"ab",
		"github-actions",
		"ext-123-abc",
		strings.Repeat("a", 64),
	}
	for _, id := range valid {
		if !ValidID(id) {
			t.Errorf("ValidID(%q) = false, want true", id)
		}
		if err := ValidateID(id); err != nil {
			t.Errorf("ValidateID(%q) unexpected error: %v", id, err)
		}
	}

	invalid := []string{
		"",
		"-abc",
		"abc-",
		"UpperCase",
		"with.dot",
		"with/slash",
		"with\\slash",
		"..",
		"with_underscore",
		strings.Repeat("a", 65),
	}
	for _, id := range invalid {
		if ValidID(id) {
			t.Errorf("ValidID(%q) = true, want false", id)
		}
		if err := ValidateID(id); err == nil {
			t.Errorf("ValidateID(%q) = nil, want error", id)
		}
	}
}

func TestWriteFileAtomicPermissionsAndCleanup(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "nested", "secret.json")
	payload := []byte(`{"ok":true}`)

	if err := WriteFileAtomic(dst, payload, 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("content = %q, want %q", got, payload)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(dst)
		if err != nil {
			t.Fatalf("Stat(%s): %v", dst, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("file perm = %04o, want 0600", perm)
		}
		dinfo, err := os.Stat(filepath.Dir(dst))
		if err != nil {
			t.Fatalf("Stat(dir): %v", err)
		}
		if perm := dinfo.Mode().Perm(); perm != 0o700 {
			t.Fatalf("dir perm = %04o, want 0700", perm)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(dst))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 file after atomic write, got %d", len(entries))
	}
}

func TestValidateRelPathAndResolveWithinRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "escape.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := ValidateRelPath(".", false); err == nil {
		t.Fatal("ValidateRelPath(., false) should fail")
	}
	if got, err := ValidateRelPath(".", true); err != nil || got != "." {
		t.Fatalf("ValidateRelPath(., true) = (%q, %v), want (\".\", nil)", got, err)
	}

	badPaths := []string{
		"",
		"../outside",
		"sub/../../outside",
		"/etc/passwd",
		"sub\\file",
		"C:foo",
		"bad\x00path",
		string([]byte{0xff, 0xfe}),
	}
	for _, p := range badPaths {
		if _, err := ResolveWithinRoot(root, p, false); err == nil {
			t.Errorf("ResolveWithinRoot(%q) = nil, want error", p)
		}
	}

	resolved, err := ResolveWithinRoot(root, "sub/Dockerfile", false)
	if err != nil {
		t.Fatalf("ResolveWithinRoot(sub/Dockerfile): %v", err)
	}
	if !strings.HasSuffix(resolved, filepath.Join("sub", "Dockerfile")) {
		t.Fatalf("unexpected resolved path %q", resolved)
	}

	if runtime.GOOS != "windows" {
		// Escaping file symlink must be rejected.
		escapeLink := filepath.Join(root, "escape-link")
		if err := os.Symlink(filepath.Join(outside, "escape.txt"), escapeLink); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolveWithinRoot(root, "escape-link", false); err == nil {
			t.Fatal("ResolveWithinRoot accepted symlink escaping root")
		}

		// Escaping directory symlink must be rejected even for nested paths.
		escapeDir := filepath.Join(root, "escape-dir")
		if err := os.Symlink(outside, escapeDir); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolveWithinRoot(root, "escape-dir/escape.txt", false); err == nil {
			t.Fatal("ResolveWithinRoot accepted directory symlink escaping root")
		}

		// Internal symlink staying within root is allowed.
		internalLink := filepath.Join(root, "Dockerfile.link")
		if err := os.Symlink(filepath.Join(root, "sub", "Dockerfile"), internalLink); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolveWithinRoot(root, "Dockerfile.link", false); err != nil {
			t.Fatalf("ResolveWithinRoot rejected internal symlink: %v", err)
		}
	}
}

func TestSocketDirLengthLimitAndFallback(t *testing.T) {
	dataDir := t.TempDir()
	shortTemp := t.TempDir()

	dir, err := socketDirWithTemp(dataDir, shortTemp)
	if err != nil {
		t.Fatalf("socketDirWithTemp(short): %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	fullSock := filepath.Join(dir, ShortSocketName("d", "very-long-extension-identifier-that-keeps-going"))
	if runtime.GOOS != "windows" && len(fullSock) > MaxUnixSocketPathLen {
		t.Fatalf("socket path %q len=%d > %d", fullSock, len(fullSock), MaxUnixSocketPathLen)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Lstat(dir)
		if err != nil {
			t.Fatalf("Lstat(%s): %v", dir, err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("socket dir perm = %04o, want 0700", info.Mode().Perm())
		}

		// Artificially long TMPDIR (>90 chars) must fall back to /tmp so socket path <= 100B.
		longTemp := filepath.Join(t.TempDir(), strings.Repeat("verylongdir/", 8))
		fbDir, err := socketDirWithTemp(dataDir, longTemp)
		if err != nil {
			t.Fatalf("socketDirWithTemp(long): %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(fbDir) })
		if !strings.HasPrefix(fbDir, "/tmp/") {
			t.Fatalf("expected /tmp fallback for long tempDir, got %q", fbDir)
		}
		fbSock := filepath.Join(fbDir, ShortSocketName("e", "github-actions"))
		if len(fbSock) > MaxUnixSocketPathLen {
			t.Fatalf("fallback socket path %q len=%d > %d", fbSock, len(fbSock), MaxUnixSocketPathLen)
		}
	}
}

func TestEnsureDirWithin_RejectsLiveAndDanglingSymlinksBelowRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		return
	}
	root := filepath.Join(t.TempDir(), "data")
	outside := t.TempDir()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}

	// 1. Live symlinked child dir.
	if err := os.Symlink(outside, filepath.Join(root, "sym-child")); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDirWithin(root, filepath.Join(root, "sym-child")); err == nil {
		t.Fatal("EnsureDirWithin accepted live symlinked child")
	}
	if err := EnsureDirWithin(root, filepath.Join(root, "sym-child", "sub")); err == nil {
		t.Fatal("EnsureDirWithin traversed live symlinked child")
	}

	// 2. Dangling symlink.
	if err := os.Symlink(filepath.Join(outside, "nonexistent"), filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDirWithin(root, filepath.Join(root, "dangling")); err == nil {
		t.Fatal("EnsureDirWithin accepted dangling symlink")
	}
	if err := CheckNoSymlinkWithin(root, filepath.Join(root, "dangling", "file.txt")); err == nil {
		t.Fatal("CheckNoSymlinkWithin accepted dangling symlink")
	}

	// 3. Intermediate symlink.
	if err := os.MkdirAll(filepath.Join(root, "a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "a", "sym")); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDirWithin(root, filepath.Join(root, "a", "sym", "deep")); err == nil {
		t.Fatal("EnsureDirWithin traversed intermediate symlink")
	}
	if err := EnsureDir(filepath.Join(root, "dangling")); err == nil {
		t.Fatal("EnsureDir accepted direct dangling symlink")
	}

	// 4. Outside escape via '..'.
	if err := EnsureDirWithin(root, filepath.Join(root, "..", "outside")); err == nil {
		t.Fatal("EnsureDirWithin accepted '..' escape")
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("expected outside dir untouched, got entries=%v err=%v", entries, err)
	}

	// 5. Legitimate nested dir creation succeeds with 0700.
	validDir := filepath.Join(root, "valid", "nested")
	if err := EnsureDirWithin(root, validDir); err != nil {
		t.Fatalf("EnsureDirWithin(valid): %v", err)
	}
	info, err := os.Stat(validDir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("validDir stat = (%v, %v)", info, err)
	}
}

func TestWriteFileAtomicWithin_RejectsSymlinkParentAndLeaf(t *testing.T) {
	if runtime.GOOS == "windows" {
		return
	}
	root := filepath.Join(t.TempDir(), "data")
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}

	// Live symlink parent.
	if err := os.Symlink(outside, filepath.Join(root, "sym-parent")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomicWithin(root, filepath.Join(root, "sym-parent", "x.json"), []byte("{}"), 0o600); err == nil {
		t.Fatal("WriteFileAtomicWithin wrote through symlink parent")
	}

	// Live symlink leaf.
	outsideTarget := filepath.Join(outside, "target.txt")
	if err := os.WriteFile(outsideTarget, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	liveLeaf := filepath.Join(root, "sub", "live-leaf.txt")
	if err := os.Symlink(outsideTarget, liveLeaf); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomicWithin(root, liveLeaf, []byte("pwned"), 0o600); err == nil {
		t.Fatal("WriteFileAtomicWithin overwrote live symlink leaf")
	}
	if err := WriteFileAtomic(liveLeaf, []byte("pwned"), 0o600); err == nil {
		t.Fatal("WriteFileAtomic overwrote live symlink leaf")
	}
	if _, err := ReadFileNoSymlinkWithin(root, liveLeaf); err == nil {
		t.Fatal("ReadFileNoSymlinkWithin read through live symlink leaf")
	}
	if got, _ := os.ReadFile(outsideTarget); string(got) != "original" {
		t.Fatalf("outside target modified: %q", string(got))
	}

	// Dangling symlink leaf.
	danglingTarget := filepath.Join(outside, "created.txt")
	danglingLeaf := filepath.Join(root, "sub", "dangling-leaf.txt")
	if err := os.Symlink(danglingTarget, danglingLeaf); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomicWithin(root, danglingLeaf, []byte("pwned"), 0o600); err == nil {
		t.Fatal("WriteFileAtomicWithin wrote through dangling symlink leaf")
	}
	if _, err := os.Stat(danglingTarget); !os.IsNotExist(err) {
		t.Fatalf("dangling symlink target should not exist (err=%v)", err)
	}

	// Normal file write & read within root succeeds.
	okFile := filepath.Join(root, "sub", "ok.json")
	if err := WriteFileAtomicWithin(root, okFile, []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatalf("WriteFileAtomicWithin(ok): %v", err)
	}
	got, err := ReadFileNoSymlinkWithin(root, okFile)
	if err != nil || string(got) != `{"ok":true}` {
		t.Fatalf("ReadFileNoSymlinkWithin(ok) = (%q, %v)", got, err)
	}
}

func TestCheckNoSymlinkWithin_AllowsMacOSAncestorSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		return
	}
	base := t.TempDir()
	outside := t.TempDir()
	realParent := filepath.Join(base, "realParent")
	realDataRoot := filepath.Join(realParent, "dataDir")
	if err := os.MkdirAll(realDataRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	linkParent := filepath.Join(base, "linkParent")
	if err := os.Symlink(realParent, linkParent); err != nil {
		t.Fatal(err)
	}

	// Ancestor symlink above root (like macOS /var -> /private/var) MUST be allowed.
	dataRoot := filepath.Join(linkParent, "dataDir")
	okSettings := filepath.Join(dataRoot, "extension-data", "ext1", "settings.json")
	if err := WriteFileAtomicWithin(dataRoot, okSettings, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatalf("WriteFileAtomicWithin through ancestor symlink failed: %v", err)
	}
	if got, err := ReadFileNoSymlinkWithin(dataRoot, okSettings); err != nil || string(got) != `{"a":1}` {
		t.Fatalf("ReadFileNoSymlinkWithin through ancestor symlink = (%q, %v)", got, err)
	}

	// Symlink at or below dataRoot MUST still be rejected.
	if err := os.Symlink(outside, filepath.Join(dataRoot, "extension-data", "ext2")); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDirWithin(dataRoot, filepath.Join(dataRoot, "extension-data", "ext2", "files")); err == nil {
		t.Fatal("EnsureDirWithin accepted symlink below root")
	}
}
