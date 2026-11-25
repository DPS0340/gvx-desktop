package ext

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"unicode/utf8"
)

// MaxUnixSocketPathLen keeps full Unix socket paths comfortably under
// macOS (104B) and Linux (107B) sun_path limits.
const MaxUnixSocketPathLen = 100

var idRegex = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)

// ValidID reports whether id is a valid extension or publisher slug.
func ValidID(id string) bool {
	return idRegex.MatchString(id)
}

// ValidateID returns an error if id is not a valid extension identifier.
func ValidateID(id string) error {
	if !ValidID(id) {
		return fmt.Errorf("invalid extension id %q: must match ^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$", id)
	}
	return nil
}

// Paths provides dataDir-rooted storage locations for extensions.
// Nothing is ever stored in ~/.gvx.
type Paths struct {
	DataDir string
}

// NewPaths returns a Paths helper rooted at dataDir.
func NewPaths(dataDir string) Paths {
	return Paths{DataDir: dataDir}
}

func (p Paths) GrantsFile() string                 { return GrantsPath(p.DataDir) }
func (p Paths) ExtensionsDir() string              { return ExtensionsDir(p.DataDir) }
func (p Paths) ExtensionDir(id string) string      { return ExtensionDir(p.DataDir, id) }
func (p Paths) ManifestSnapshot(id string) string  { return ManifestSnapshotPath(p.DataDir, id) }
func (p Paths) SourceFile(id string) string        { return SourcePath(p.DataDir, id) }
func (p Paths) SourceCheckoutDir(id string) string { return SourceCheckoutDir(p.DataDir, id) }
func (p Paths) DataRootDir() string                { return ExtensionDataRoot(p.DataDir) }
func (p Paths) ExtDataDir(id string) string        { return ExtensionDataDir(p.DataDir, id) }
func (p Paths) SettingsFile(id string) string      { return SettingsPath(p.DataDir, id) }
func (p Paths) StorageFile(id string) string       { return StoragePath(p.DataDir, id) }
func (p Paths) SecretsIndexFile(id string) string  { return SecretsIndexPath(p.DataDir, id) }
func (p Paths) SecretsFile(id string) string       { return SecretsFilePath(p.DataDir, id) }
func (p Paths) FilesDir(id string) string          { return FilesDir(p.DataDir, id) }
func (p Paths) LogsDir(id string) string           { return LogsDir(p.DataDir, id) }
func (p Paths) SocketDir() (string, error)         { return SocketDir(p.DataDir) }
func (p Paths) SocketPath(prefix, id string) (string, error) {
	dir, err := SocketDir(p.DataDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ShortSocketName(prefix, id)), nil
}

// GrantsPath returns <dataDir>/extension-grants.json.
func GrantsPath(dataDir string) string {
	return filepath.Join(dataDir, "extension-grants.json")
}

// ExtensionsDir returns <dataDir>/extensions.
func ExtensionsDir(dataDir string) string {
	return filepath.Join(dataDir, "extensions")
}

// ExtensionDir returns <dataDir>/extensions/<id>.
func ExtensionDir(dataDir, id string) string {
	return filepath.Join(dataDir, "extensions", id)
}

// ManifestSnapshotPath returns <dataDir>/extensions/<id>/extension.toml.
func ManifestSnapshotPath(dataDir, id string) string {
	return filepath.Join(dataDir, "extensions", id, ManifestName)
}

// SourcePath returns <dataDir>/extensions/<id>/source.json.
func SourcePath(dataDir, id string) string {
	return filepath.Join(dataDir, "extensions", id, "source.json")
}

// SourceCheckoutDir returns <dataDir>/extensions/<id>/src.
func SourceCheckoutDir(dataDir, id string) string {
	return filepath.Join(dataDir, "extensions", id, "src")
}

// ExtensionDataRoot returns <dataDir>/extension-data.
func ExtensionDataRoot(dataDir string) string {
	return filepath.Join(dataDir, "extension-data")
}

// ExtensionDataDir returns <dataDir>/extension-data/<id>.
func ExtensionDataDir(dataDir, id string) string {
	return filepath.Join(dataDir, "extension-data", id)
}

// SettingsPath returns <dataDir>/extension-data/<id>/settings.json.
func SettingsPath(dataDir, id string) string {
	return filepath.Join(dataDir, "extension-data", id, "settings.json")
}

// StoragePath returns <dataDir>/extension-data/<id>/storage.json.
func StoragePath(dataDir, id string) string {
	return filepath.Join(dataDir, "extension-data", id, "storage.json")
}

// SecretsIndexPath returns <dataDir>/extension-data/<id>/secrets-index.json.
func SecretsIndexPath(dataDir, id string) string {
	return filepath.Join(dataDir, "extension-data", id, "secrets-index.json")
}

// SecretsFilePath returns <dataDir>/extension-data/<id>/secrets.json.
func SecretsFilePath(dataDir, id string) string {
	return filepath.Join(dataDir, "extension-data", id, "secrets.json")
}

// FilesDir returns <dataDir>/extension-data/<id>/files.
func FilesDir(dataDir, id string) string {
	return filepath.Join(dataDir, "extension-data", id, "files")
}

// LogsDir returns <dataDir>/extension-data/<id>/logs.
func LogsDir(dataDir, id string) string {
	return filepath.Join(dataDir, "extension-data", id, "logs")
}

// EnsureDir creates dir (and parents) with private 0700 permissions,
// refusing to traverse or accept a symlink at dir itself.
func EnsureDir(dir string) error {
	clean := filepath.Clean(dir)
	if info, err := os.Lstat(clean); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink directory %q", clean)
		}
		if !info.IsDir() {
			return fmt.Errorf("path %q is not a directory", clean)
		}
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.MkdirAll(clean, 0o700)
}

// CheckNoSymlinkWithin verifies that target lies within root and that no path
// component starting at root downward (including root itself and any existing
// intermediate or leaf component, whether live or dangling) is a symlink.
// Ancestors above root are not inspected so platform symlinks above dataDir
// (such as macOS /var -> /private/var) remain valid.
func CheckNoSymlinkWithin(root, target string) error {
	if strings.TrimSpace(root) == "" {
		return errors.New("root directory is empty")
	}
	if strings.TrimSpace(target) == "" {
		return errors.New("target path is empty")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve root %q: %w", root, err)
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("resolve target %q: %w", target, err)
	}
	absRoot = filepath.Clean(absRoot)
	absTarget = filepath.Clean(absTarget)

	rel, err := filepath.Rel(absRoot, absTarget)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("path %q escapes root %q", target, absRoot)
	}

	rootInfo, err := os.Lstat(absRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symlink root %q", absRoot)
	}
	if !rootInfo.IsDir() {
		return fmt.Errorf("root %q is not a directory", absRoot)
	}
	if rel == "." {
		return nil
	}

	segs := strings.Split(rel, string(filepath.Separator))
	cur := absRoot
	for i, seg := range segs {
		if seg == "" || seg == "." {
			continue
		}
		cur = filepath.Join(cur, seg)
		info, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				break
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink at %q within %q", cur, absRoot)
		}
		if i < len(segs)-1 && !info.IsDir() {
			return fmt.Errorf("path component %q is not a directory", cur)
		}
	}
	return nil
}

// EnsureDirWithin creates dir (and any missing parents beneath root) with 0700
// permissions while verifying via os.Lstat that neither root nor any component
// beneath root is a symlink.
func EnsureDirWithin(root, dir string) error {
	if strings.TrimSpace(root) == "" {
		return errors.New("root directory is empty")
	}
	if strings.TrimSpace(dir) == "" {
		return errors.New("target directory is empty")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve root %q: %w", root, err)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve dir %q: %w", dir, err)
	}
	absRoot = filepath.Clean(absRoot)
	absDir = filepath.Clean(absDir)

	rel, err := filepath.Rel(absRoot, absDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("directory %q escapes root %q", dir, absRoot)
	}

	rootInfo, err := os.Lstat(absRoot)
	if errors.Is(err, fs.ErrNotExist) {
		if mkErr := os.MkdirAll(absRoot, 0o700); mkErr != nil {
			return mkErr
		}
		rootInfo, err = os.Lstat(absRoot)
	}
	if err != nil {
		return err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symlink root %q", absRoot)
	}
	if !rootInfo.IsDir() {
		return fmt.Errorf("root %q is not a directory", absRoot)
	}
	if rel == "." {
		return nil
	}

	cur := absRoot
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		if seg == "" || seg == "." {
			continue
		}
		cur = filepath.Join(cur, seg)
		info, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			if mkErr := os.Mkdir(cur, 0o700); mkErr != nil && !errors.Is(mkErr, os.ErrExist) {
				return mkErr
			}
			info, err = os.Lstat(cur)
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink directory %q within %q", cur, absRoot)
		}
		if !info.IsDir() {
			return fmt.Errorf("path %q is not a directory", cur)
		}
	}
	return nil
}

// ReadFileNoSymlinkWithin verifies that path lies within root without any
// symlink components at or below root, and reads its contents.
func ReadFileNoSymlinkWithin(root, target string) ([]byte, error) {
	if err := CheckNoSymlinkWithin(root, target); err != nil {
		return nil, err
	}
	return os.ReadFile(target)
}

func checkStrictChildNoSymlinkWithin(root, target string) error {
	if err := CheckNoSymlinkWithin(root, target); err != nil {
		return err
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve root %q: %w", root, err)
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("resolve target %q: %w", target, err)
	}
	if filepath.Clean(absTarget) == filepath.Clean(absRoot) {
		return fmt.Errorf("refusing to remove root directory %q", filepath.Clean(absRoot))
	}
	if info, err := os.Lstat(target); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink target %q within %q", target, filepath.Clean(absRoot))
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// removeFileNoSymlinkWithin verifies that target lies strictly inside root with
// no symlink components at or below root (including a symlink leaf), then removes it.
func removeFileNoSymlinkWithin(root, target string) error {
	if err := checkStrictChildNoSymlinkWithin(root, target); err != nil {
		return err
	}
	return os.Remove(target)
}

// removeAllNoSymlinkWithin verifies that target lies strictly inside root with
// no symlink components at or below root (including a symlink leaf), then removes it recursively.
func removeAllNoSymlinkWithin(root, target string) error {
	if err := checkStrictChildNoSymlinkWithin(root, target); err != nil {
		return err
	}
	return os.RemoveAll(target)
}

// WriteFileAtomic writes data to dst atomically with private permissions (default 0600).
func WriteFileAtomic(dst string, data []byte, perm os.FileMode) error {
	if info, err := os.Lstat(dst); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink target %q", dst)
		}
		if info.IsDir() {
			return fmt.Errorf("target %q is a directory", dst)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	dir := filepath.Dir(dst)
	if err := EnsureDir(dir); err != nil {
		return err
	}
	return writeFileAtomicInDir(dir, dst, data, perm)
}

// WriteFileAtomicWithin ensures dst lies within root with no symlink components
// at or below root (neither in parent directories nor at dst itself) and writes
// data atomically with private permissions (default 0600).
func WriteFileAtomicWithin(root, dst string, data []byte, perm os.FileMode) error {
	if err := CheckNoSymlinkWithin(root, dst); err != nil {
		return err
	}
	dir := filepath.Dir(dst)
	if err := EnsureDirWithin(root, dir); err != nil {
		return err
	}
	if info, err := os.Lstat(dst); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink target %q", dst)
		}
		if info.IsDir() {
			return fmt.Errorf("target %q is a directory", dst)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return writeFileAtomicInDir(dir, dst, data, perm)
}

func writeFileAtomicInDir(dir, dst string, data []byte, perm os.FileMode) error {
	if perm == 0 {
		perm = 0o600
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(dst)+".tmp.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(perm); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// ValidateRelPath validates a relative slash-separated path without touching disk.
// It rejects empty paths, NUL bytes, backslashes, colons/drive prefixes,
// leading slashes, ".." segments, and "." unless allowDot is true.
func ValidateRelPath(rel string, allowDot bool) (string, error) {
	if rel == "" {
		return "", errors.New("relative path is empty")
	}
	if !utf8.ValidString(rel) {
		return "", errors.New("relative path is not valid UTF-8")
	}
	if strings.ContainsRune(rel, 0) {
		return "", errors.New("relative path contains NUL byte")
	}
	if strings.ContainsRune(rel, '\\') {
		return "", fmt.Errorf("relative path %q must use '/' separators", rel)
	}
	if strings.ContainsRune(rel, ':') {
		return "", fmt.Errorf("relative path %q must not contain ':'", rel)
	}
	if strings.HasPrefix(rel, "/") || path.IsAbs(rel) {
		return "", fmt.Errorf("relative path %q must not be absolute", rel)
	}
	if slices.Contains(strings.Split(rel, "/"), "..") {
		return "", fmt.Errorf("relative path %q must not contain '..'", rel)
	}
	cleaned := path.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("relative path %q escapes root", rel)
	}
	if cleaned == "." && !allowDot {
		return "", fmt.Errorf("relative path %q must point inside root", rel)
	}
	return cleaned, nil
}

// ResolveWithinRoot resolves rel inside root, rejecting path traversal and
// any symlink component whose target resolves outside root.
func ResolveWithinRoot(root, rel string, allowDot bool) (string, error) {
	cleaned, err := ValidateRelPath(rel, allowDot)
	if err != nil {
		return "", err
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve root %q: %w", root, err)
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", fmt.Errorf("inspect root %q: %w", root, err)
	}
	info, err := os.Stat(realRoot)
	if err != nil {
		return "", fmt.Errorf("stat root %q: %w", root, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("root %q is not a directory", root)
	}
	if cleaned == "." {
		return realRoot, nil
	}

	cur := realRoot
	for _, seg := range strings.Split(cleaned, "/") {
		if seg == "" || seg == "." {
			continue
		}
		cur = filepath.Join(cur, seg)
		linfo, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				break
			}
			return "", err
		}
		if linfo.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", fmt.Errorf("resolve symlink %q: %w", rel, err)
			}
			if !isWithinDir(realRoot, resolved) {
				return "", fmt.Errorf("path %q escapes root via symlink", rel)
			}
		}
	}

	target := filepath.Join(realRoot, filepath.FromSlash(cleaned))
	if _, err := os.Lstat(target); err == nil {
		resolved, err := filepath.EvalSymlinks(target)
		if err != nil {
			return "", fmt.Errorf("resolve %q: %w", rel, err)
		}
		if !isWithinDir(realRoot, resolved) {
			return "", fmt.Errorf("path %q escapes root via symlink", rel)
		}
		return resolved, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	return target, nil
}

func isWithinDir(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	return true
}

// ShortSocketName returns a short, deterministic socket filename for prefix
// ("h", "d", or "e") and extension id, e.g. "d-01234567.sock".
func ShortSocketName(prefix, id string) string {
	sum := sha256.Sum256([]byte(id))
	return fmt.Sprintf("%s-%s.sock", prefix, hex.EncodeToString(sum[:4]))
}

// SocketDir returns the short private IPC socket directory for dataDir,
// ensuring it exists with mode 0700 and falls back to /tmp on Unix when
// os.TempDir() would exceed MaxUnixSocketPathLen (sun_path limit).
func SocketDir(dataDir string) (string, error) {
	return socketDirWithTemp(dataDir, os.TempDir())
}

func socketDirWithTemp(dataDir, tempDir string) (string, error) {
	absDir, err := filepath.Abs(dataDir)
	if err != nil {
		absDir = dataDir
	}
	sum := sha256.Sum256([]byte(absDir))
	tag := hex.EncodeToString(sum[:4])

	if runtime.GOOS == "windows" {
		dir := filepath.Join(tempDir, "gvxd-ext-"+tag)
		if err := EnsureDir(dir); err != nil {
			return "", err
		}
		return dir, nil
	}

	dirName := fmt.Sprintf("gvxd-ext-%d-%s", currentEUID(), tag)
	candidate := filepath.Join(tempDir, dirName)
	sampleSock := filepath.Join(candidate, "d-01234567.sock")
	if len(sampleSock) > MaxUnixSocketPathLen {
		candidate = filepath.Join("/tmp", dirName)
		if len(filepath.Join(candidate, "d-01234567.sock")) > MaxUnixSocketPathLen {
			return "", fmt.Errorf("socket directory path %q exceeds %d-byte sun_path limit", candidate, MaxUnixSocketPathLen)
		}
	}
	if err := ensurePrivateSocketDir(candidate); err != nil {
		return "", err
	}
	return candidate, nil
}
