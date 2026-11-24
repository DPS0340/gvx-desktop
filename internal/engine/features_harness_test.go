//go:build !windows

package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHarnessExtraFeatures verifies image history, export->import round-trip,
// stopped/created container docker cp, --volumes-from, and container size
// reporting against real rungv + docker CLI (GVX_ENGINE_HARNESS=1).
func TestHarnessExtraFeatures(t *testing.T) {
	h := startHarness(t)
	if !h.e.runner.Features().Control {
		t.Skip("extra features harness requires rungv v0.20+ control support")
	}

	// Ensure alpine:3.20 is cached before running subtests.
	if _, errOut, code := h.docker(t, "", "run", "--rm", "alpine:3.20", "true"); code != 0 {
		t.Fatalf("prime alpine:3.20 (%d): %s", code, errOut)
	}

	t.Run("image_history", func(t *testing.T) {
		out, errOut, code := h.docker(t, "", "history", "--no-trunc", "alpine:3.20")
		if code != 0 {
			t.Fatalf("docker history (%d): %s", code, errOut)
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) < 2 {
			t.Fatalf("docker history returned too few lines:\n%s", out)
		}
	})

	t.Run("stopped_and_created_docker_cp", func(t *testing.T) {
		t.Cleanup(func() {
			_, _, _ = h.docker(t, "", "rm", "-f", "gvxd-cp-created", "gvxd-cp-stopped")
		})

		// 1. Copy a file out of a created (never started) container.
		if _, errOut, code := h.docker(t, "", "create", "--name", "gvxd-cp-created", "alpine:3.20", "true"); code != 0 {
			t.Fatalf("docker create (%d): %s", code, errOut)
		}
		dstCreated := filepath.Join(t.TempDir(), "os-release")
		if _, errOut, code := h.docker(t, "", "cp", "gvxd-cp-created:/etc/os-release", dstCreated); code != 0 {
			t.Fatalf("docker cp from created container (%d): %s", code, errOut)
		}
		raw, err := os.ReadFile(dstCreated)
		if err != nil || !strings.Contains(string(raw), "Alpine") {
			t.Fatalf("copied /etc/os-release = %q, err=%v", string(raw), err)
		}

		// 2. Run a container that writes files + symlink, exits, then docker cp file, dir, and via symlink.
		if _, errOut, code := h.docker(t, "", "run", "--name", "gvxd-cp-stopped", "alpine:3.20",
			"sh", "-c", "mkdir -p /app/sub && echo hello-stopped > /app/sub/out.txt && ln -s /app/sub /app/link"); code != 0 {
			t.Fatalf("docker run (%d): %s", code, errOut)
		}

		tmpDir := t.TempDir()
		outFile := filepath.Join(tmpDir, "out.txt")
		if _, errOut, code := h.docker(t, "", "cp", "gvxd-cp-stopped:/app/sub/out.txt", outFile); code != 0 {
			t.Fatalf("docker cp single file from stopped (%d): %s", code, errOut)
		}
		if b, err := os.ReadFile(outFile); err != nil || strings.TrimSpace(string(b)) != "hello-stopped" {
			t.Fatalf("stopped out.txt = %q, err=%v", string(b), err)
		}

		outViaLink := filepath.Join(tmpDir, "via-link.txt")
		if _, errOut, code := h.docker(t, "", "cp", "gvxd-cp-stopped:/app/link/out.txt", outViaLink); code != 0 {
			t.Fatalf("docker cp via symlink from stopped (%d): %s", code, errOut)
		}
		if b, err := os.ReadFile(outViaLink); err != nil || strings.TrimSpace(string(b)) != "hello-stopped" {
			t.Fatalf("stopped via-link.txt = %q, err=%v", string(b), err)
		}

		dirDst := filepath.Join(tmpDir, "copied")
		if _, errOut, code := h.docker(t, "", "cp", "gvxd-cp-stopped:/app", dirDst); code != 0 {
			t.Fatalf("docker cp dir from stopped (%d): %s", code, errOut)
		}
		if b, err := os.ReadFile(filepath.Join(dirDst, "sub", "out.txt")); err != nil || strings.TrimSpace(string(b)) != "hello-stopped" {
			t.Fatalf("stopped dir sub/out.txt = %q, err=%v", string(b), err)
		}
	})

	t.Run("export_and_import_roundtrip", func(t *testing.T) {
		const importedTag = "gvxd-imported-test:v1"
		t.Cleanup(func() {
			_, _, _ = h.docker(t, "", "rm", "-f", "gvxd-exp-src")
			_, _, _ = h.docker(t, "", "rmi", "-f", importedTag)
		})

		if _, errOut, code := h.docker(t, "", "run", "--name", "gvxd-exp-src", "alpine:3.20",
			"sh", "-c", "echo exported-payload > /imported-marker.txt"); code != 0 {
			t.Fatalf("run export src (%d): %s", code, errOut)
		}
		tarPath := filepath.Join(t.TempDir(), "rootfs.tar")
		if _, errOut, code := h.docker(t, "", "export", "-o", tarPath, "gvxd-exp-src"); code != 0 {
			t.Fatalf("docker export (%d): %s", code, errOut)
		}
		out, errOut, code := h.docker(t, "", "import",
			"--change", "ENV FROM_IMPORT=ok",
			"--change", `CMD ["/bin/sh", "-c", "echo $FROM_IMPORT:$(cat /imported-marker.txt)"]`,
			tarPath, importedTag)
		if code != 0 || !strings.Contains(out, "sha256:") {
			t.Fatalf("docker import (%d): out=%q err=%q", code, out, errOut)
		}
		out, errOut, code = h.docker(t, "", "run", "--rm", importedTag)
		if code != 0 || strings.TrimSpace(out) != "ok:exported-payload" {
			t.Fatalf("run imported image (%d): out=%q err=%q", code, out, errOut)
		}
	})

	t.Run("volumes_from_and_sizes", func(t *testing.T) {
		t.Cleanup(func() {
			_, _, _ = h.docker(t, "", "rm", "-f", "gvxd-vf-writer")
			_, _, _ = h.docker(t, "", "volume", "rm", "gvxd-vf-data")
		})

		if _, errOut, code := h.docker(t, "", "run", "--name", "gvxd-vf-writer",
			"-v", "gvxd-vf-data:/shared", "alpine:3.20",
			"sh", "-c", "echo shared-ok > /shared/msg.txt && dd if=/dev/zero of=/rootfs-pad.bin bs=1024 count=64 2>/dev/null"); code != 0 {
			t.Fatalf("run writer (%d): %s", code, errOut)
		}

		// Read via --volumes-from :ro and verify writes fail on read-only mount.
		out, errOut, code := h.docker(t, "", "run", "--rm", "--volumes-from", "gvxd-vf-writer:ro",
			"alpine:3.20", "sh", "-c", "cat /shared/msg.txt && (echo fail > /shared/nope 2>/dev/null && echo WRITABLE || echo READONLY)")
		if code != 0 || !strings.Contains(out, "shared-ok") || !strings.Contains(out, "READONLY") {
			t.Fatalf("volumes-from ro (%d): out=%q err=%q", code, out, errOut)
		}

		// Verify docker ps -a -s reports non-zero SizeRw for gvxd-vf-writer.
		out, errOut, code = h.docker(t, "", "ps", "-a", "-s", "--filter", "name=^/gvxd-vf-writer$", "--format", "{{.Size}}")
		if code != 0 || strings.TrimSpace(out) == "" || strings.HasPrefix(strings.TrimSpace(out), "0B") {
			t.Fatalf("docker ps -s (%d): out=%q err=%q", code, out, errOut)
		}
	})
}
