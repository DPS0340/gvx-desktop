package imagefs

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestFormatModeAndHelpers(t *testing.T) {
	if got := FormatMode(0o040755, KindDir); got != "drwxr-xr-x" {
		t.Errorf("dir mode = %q", got)
	}
	if got := FormatMode(0o100644, KindFile); got != "-rw-r--r--" {
		t.Errorf("file mode = %q", got)
	}
	if got := FormatMode(0o104755, KindFile); got != "-rwsr-xr-x" {
		t.Errorf("setuid mode = %q", got)
	}
	if got := FormatMode(0o120777, KindSymlink); got != "lrwxrwxrwx" {
		t.Errorf("symlink mode = %q", got)
	}
	if got := ResolveSymlink("/bin/sh", "busybox"); got != "/bin/busybox" {
		t.Errorf("rel symlink = %q", got)
	}
	if got := ResolveSymlink("/etc/localtime", "/usr/share/zoneinfo/UTC"); got != "/usr/share/zoneinfo/UTC" {
		t.Errorf("abs symlink = %q", got)
	}
	if IsBinary([]byte("hello world\n한글\n")) {
		t.Error("utf8 text flagged as binary")
	}
	if !IsBinary([]byte{0x7f, 'E', 'L', 'F', 0x00, 0x02}) {
		t.Error("ELF header not flagged as binary")
	}
	dump := HexDump([]byte{0x7f, 'E', 'L', 'F', 0x00, 'A'}, 16)
	if !strings.Contains(dump, "7f 45 4c 46") || !strings.Contains(dump, "|.ELF.A|") {
		t.Errorf("hexdump = %q", dump)
	}
	if got := DetectLanguage("/etc/nginx/nginx.conf"); got != "conf" {
		t.Errorf("lang = %q", got)
	}
	if got := CleanCommand("/bin/sh -c #(nop) CMD [\"nginx\"]"); got != `CMD ["nginx"]` {
		t.Errorf("clean cmd = %q", got)
	}
}

func TestOpenDockerAndReadFile(t *testing.T) {
	makeTar := func(files map[string]string, symlinks map[string]string) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for name, body := range files {
			_ = tw.WriteHeader(&tar.Header{
				Name:     name,
				Mode:     0o644,
				Size:     int64(len(body)),
				Typeflag: tar.TypeReg,
				ModTime:  time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
			})
			_, _ = tw.Write([]byte(body))
		}
		for name, target := range symlinks {
			_ = tw.WriteHeader(&tar.Header{
				Name:     name,
				Mode:     0o777,
				Typeflag: tar.TypeSymlink,
				Linkname: target,
			})
		}
		_ = tw.Close()
		return buf.Bytes()
	}

	rootArchive := makeTar(map[string]string{
		"etc/nginx/nginx.conf": "worker_processes auto;\n",
		"app/bin":              "\x7fELF\x00\x01",
	}, map[string]string{
		"bin/sh": "busybox",
	})

	removed := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/history"):
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"Id": "sha256:top", "Created": 1700000100, "CreatedBy": "/bin/sh -c apk add nginx", "Size": 2048},
				{"Id": "sha256:base", "Created": 1700000000, "CreatedBy": "ADD alpine.tar.gz /", "Size": 8192},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/containers/create":
			_, _ = w.Write([]byte(`{"Id":"tmp-cid"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/containers/tmp-cid":
			removed++
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/containers/tmp-cid/archive":
			p := r.URL.Query().Get("path")
			if p == "/" {
				_, _ = w.Write(rootArchive)
			} else if p == "/etc/nginx/nginx.conf" {
				_, _ = w.Write(makeTar(map[string]string{"nginx.conf": "worker_processes auto;\n"}, nil))
			} else if p == "/app/bin" {
				_, _ = w.Write(makeTar(map[string]string{"bin": "\x7fELF\x00\x01"}, nil))
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	host := "tcp://" + strings.TrimPrefix(srv.URL, "http://")
	ctx := context.Background()
	fs, err := OpenDocker(ctx, host, "sha256:1234", nil)
	if err != nil {
		t.Fatalf("OpenDocker: %v", err)
	}
	defer fs.Close()

	layers := fs.Layers()
	if len(layers) != 2 || layers[0].Digest != "sha256:base" || layers[1].Command != "apk add nginx" {
		t.Fatalf("layers = %+v", layers)
	}
	root, err := fs.ReadDir(ctx, "/", 0)
	if err != nil || len(root) != 3 {
		t.Fatalf("root = %+v, %v", root, err)
	}
	if root[0].Name != "app" || root[1].Name != "bin" || root[2].Name != "etc" {
		t.Errorf("unexpected root order: %+v", root)
	}
	etcNginx, err := fs.ReadDir(ctx, "/etc/nginx", 0)
	if err != nil || len(etcNginx) != 1 || etcNginx[0].Name != "nginx.conf" {
		t.Fatalf("/etc/nginx = %+v, %v", etcNginx, err)
	}
	fc, err := fs.ReadFile(ctx, "/etc/nginx/nginx.conf", 0, 1024)
	if err != nil || string(fc.Data) != "worker_processes auto;\n" || fc.Binary {
		t.Fatalf("ReadFile text: %+v, %v", fc, err)
	}
	bin, err := fs.ReadFile(ctx, "/app/bin", 0, 1024)
	if err != nil || !bin.Binary {
		t.Fatalf("ReadFile binary: %+v, %v", bin, err)
	}
	if removed != 3 {
		t.Errorf("temp container removed %d times, want 3", removed)
	}
}

func TestOpenGVXRealCacheIfPresent(t *testing.T) {
	// Exercise our pure-Go EROFS reader against a real gvx layer cache when
	// available on the developer's machine.
	cacheDir := "/Users/ironpark/.mako/home/agy/Library/Caches/gvx/layers"
	if _, err := os.Stat(cacheDir); err != nil {
		t.Skip("no local gvx layer cache")
	}
	fs, err := OpenGVX(cacheDir, "0759823471bf", "sha256:fed7e14be83cbd2661a75cdc5b872d936cf155d46c327084d30b8df19ae88d72")
	if err != nil {
		t.Skipf("nginx:alpine not in cache: %v", err)
	}
	defer fs.Close()

	layers := fs.Layers()
	if len(layers) == 0 {
		t.Fatal("expected layers for nginx:alpine")
	}
	ctx := context.Background()
	root, err := fs.ReadDir(ctx, "/", 0)
	if err != nil || len(root) == 0 {
		t.Fatalf("ReadDir(/): %+v, %v", root, err)
	}
	etcNginx, err := fs.ReadDir(ctx, "/etc/nginx", 0)
	if err != nil || len(etcNginx) == 0 {
		t.Fatalf("ReadDir(/etc/nginx): %+v, %v", etcNginx, err)
	}
	fc, err := fs.ReadFile(ctx, "/etc/nginx/nginx.conf", 0, 64*1024)
	if err != nil || !strings.Contains(string(fc.Data), "worker_processes") || fc.Binary {
		t.Fatalf("ReadFile(/etc/nginx/nginx.conf): err=%v binary=%v len=%d", err, fc.Binary, len(fc.Data))
	}
}
