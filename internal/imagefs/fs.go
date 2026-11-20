// Package imagefs provides read-only filesystem and layer exploration for both
// gvx cached EROFS images and Docker Engine images.
package imagefs

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Kind is the file type of an entry inside an image.
type Kind uint8

const (
	KindFile Kind = iota
	KindDir
	KindSymlink
	KindOther
)

// Entry is one file, directory or symlink inside an image.
type Entry struct {
	Name    string
	Path    string
	Kind    Kind
	Mode    uint32
	Size    int64
	UID     uint32
	GID     uint32
	ModTime time.Time
	Target  string // symlink target
	Layer   int    // 1-based layer number when known, 0 otherwise
}

// Equal makes Entry comparable for signals.
func (e Entry) Equal(o Entry) bool {
	return e.Name == o.Name && e.Path == o.Path && e.Kind == o.Kind &&
		e.Mode == o.Mode && e.Size == o.Size && e.UID == o.UID && e.GID == o.GID &&
		e.ModTime.Equal(o.ModTime) && e.Target == o.Target && e.Layer == o.Layer
}

// Layer describes one layer of an image.
type Layer struct {
	Index    int
	Digest   string
	Size     int64
	Created  time.Time
	Command  string
	Empty    bool
	HasFiles bool
}

// Equal makes Layer comparable for signals.
func (l Layer) Equal(o Layer) bool {
	return l.Index == o.Index && l.Digest == o.Digest && l.Size == o.Size &&
		l.Created.Equal(o.Created) && l.Command == o.Command &&
		l.Empty == o.Empty && l.HasFiles == o.HasFiles
}

// FileContent is a preview of a file's contents.
type FileContent struct {
	Data      []byte
	TotalSize int64
	Truncated bool
	Binary    bool
}

// FS explores one image's layers and filesystem.
type FS interface {
	Layers() []Layer
	// ReadDir lists entries in dir (e.g. "/etc"). layer == 0 reads the merged
	// overlay; layer > 0 reads that single layer when supported.
	ReadDir(ctx context.Context, dir string, layer int) ([]Entry, error)
	// ReadFile reads up to limit bytes of the file at filePath.
	ReadFile(ctx context.Context, filePath string, layer int, limit int64) (FileContent, error)
	Close() error
}

// CleanPath normalizes a POSIX path inside an image to start with "/".
func CleanPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return path.Clean(p)
}

// ResolveSymlink resolves target relative to the directory containing a symlink.
func ResolveSymlink(symlinkPath, target string) string {
	if strings.HasPrefix(target, "/") {
		return path.Clean(target)
	}
	return path.Clean(path.Join(path.Dir(CleanPath(symlinkPath)), target))
}

// SortEntries puts directories first, then sorts alphabetically by name.
func SortEntries(entries []Entry) {
	slices.SortFunc(entries, func(a, b Entry) int {
		ad, bd := a.Kind == KindDir, b.Kind == KindDir
		if ad != bd {
			if ad {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
}

// FormatMode formats POSIX mode bits like ls -l (e.g. "drwxr-xr-x").
func FormatMode(mode uint32, kind Kind) string {
	var b [10]byte
	switch kind {
	case KindDir:
		b[0] = 'd'
	case KindSymlink:
		b[0] = 'l'
	case KindOther:
		b[0] = '?'
		switch mode & 0o170000 {
		case 0o020000:
			b[0] = 'c'
		case 0o060000:
			b[0] = 'b'
		case 0o010000:
			b[0] = 'p'
		case 0o140000:
			b[0] = 's'
		}
	default:
		b[0] = '-'
	}
	const rwx = "rwx"
	for i := range 9 {
		if mode&(1<<uint(8-i)) != 0 {
			b[i+1] = rwx[i%3]
		} else {
			b[i+1] = '-'
		}
	}
	if mode&0o4000 != 0 {
		if b[3] == 'x' {
			b[3] = 's'
		} else {
			b[3] = 'S'
		}
	}
	if mode&0o2000 != 0 {
		if b[6] == 'x' {
			b[6] = 's'
		} else {
			b[6] = 'S'
		}
	}
	if mode&0o1000 != 0 {
		if b[9] == 'x' {
			b[9] = 't'
		} else {
			b[9] = 'T'
		}
	}
	return string(b[:])
}

// IsBinary reports whether data looks like binary rather than UTF-8 text.
func IsBinary(data []byte) bool {
	sample := data
	if len(sample) > 8192 {
		sample = sample[:8192]
	}
	if len(sample) == 0 {
		return false
	}
	if bytes.IndexByte(sample, 0) >= 0 {
		return true
	}
	// Truncating at 8192 might split a multi-byte UTF-8 rune at the tail; trim
	// up to 3 trailing continuation/lead bytes before checking utf8.Valid.
	for i := 0; i < 3 && len(sample) > 0 && !utf8.Valid(sample); i++ {
		sample = sample[:len(sample)-1]
	}
	if !utf8.Valid(sample) {
		return true
	}
	ctrl := 0
	for _, c := range sample {
		if c < 0x20 && c != '\n' && c != '\r' && c != '\t' && c != '\f' {
			ctrl++
		}
	}
	return ctrl*10 > len(sample)
}

// HexDump formats up to maxBytes of data as a classic 16-byte-per-line hex dump.
func HexDump(data []byte, maxBytes int) string {
	if maxBytes > 0 && len(data) > maxBytes {
		data = data[:maxBytes]
	}
	var b strings.Builder
	for off := 0; off < len(data); off += 16 {
		chunk := data[off:min(off+16, len(data))]
		fmt.Fprintf(&b, "%08x  ", off)
		for i := range 16 {
			if i == 8 {
				b.WriteByte(' ')
			}
			if i < len(chunk) {
				fmt.Fprintf(&b, "%02x ", chunk[i])
			} else {
				b.WriteString("   ")
			}
		}
		b.WriteString(" |")
		for _, c := range chunk {
			if c >= 0x20 && c <= 0x7e {
				b.WriteByte(c)
			} else {
				b.WriteByte('.')
			}
		}
		b.WriteString("|\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// DetectLanguage guesses a short display language label from a filename.
func DetectLanguage(name string) string {
	base := strings.ToLower(path.Base(name))
	switch base {
	case "dockerfile":
		return "dockerfile"
	case "makefile", "gnumakefile":
		return "makefile"
	case "passwd", "group", "hosts", "resolv.conf", "fstab", "os-release":
		return "conf"
	}
	switch strings.ToLower(path.Ext(base)) {
	case ".json":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".toml":
		return "toml"
	case ".sh", ".bash", ".zsh", ".ash":
		return "sh"
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".js", ".mjs", ".cjs":
		return "javascript"
	case ".ts", ".mts":
		return "typescript"
	case ".html", ".htm":
		return "html"
	case ".css":
		return "css"
	case ".xml", ".svg", ".plist":
		return "xml"
	case ".md", ".markdown":
		return "markdown"
	case ".sql":
		return "sql"
	case ".conf", ".cfg", ".ini", ".cnf", ".env", ".properties":
		return "conf"
	case ".c", ".h":
		return "c"
	case ".rs":
		return "rust"
	case ".rb":
		return "ruby"
	case ".txt", ".log":
		return "text"
	}
	return ""
}

// ShortDigest trims a sha256: prefix and returns the first 12 characters.
func ShortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if d == "" || d == "<missing>" {
		return d
	}
	return d[:min(12, len(d))]
}

// CleanCommand trims the shell wrapper prefix docker history adds to commands.
func CleanCommand(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	for _, prefix := range []string{"/bin/sh -c #(nop) ", "/bin/sh -c ", "sh -c "} {
		if rest, ok := strings.CutPrefix(cmd, prefix); ok {
			return strings.TrimSpace(rest)
		}
	}
	return cmd
}
