package guest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ironpark/gvx-desktop/internal/overlay/guest/srchash"
	"github.com/ironpark/gvx-desktop/internal/overlay/guestcfg"
)

func TestBinariesAreUpToDate(t *testing.T) {
	want, err := os.ReadFile("SOURCE")
	if err != nil {
		t.Fatal(err)
	}
	got, err := srchash.SourceHash(".")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(want)) != got {
		t.Fatal("netinit changed since its binaries were built: run go generate ./internal/overlay/guest")
	}
}

func TestNetinitIsALinuxExecutable(t *testing.T) {
	b, err := Netinit()
	if err == ErrUnavailable {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(b, []byte("\x7fELF")) {
		t.Fatal("netinit is not an ELF file")
	}
}

func TestInstallWritesBinaryAndHostfsMeta(t *testing.T) {
	base := t.TempDir()
	bin := []byte("\x7fELF-fake-netinit-v1")

	mountDir, err := Install(base, bin)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if want := filepath.Join(base, installDirName); mountDir != want {
		t.Fatalf("mountDir = %q, want %q", mountDir, want)
	}

	binPath := filepath.Join(mountDir, guestcfg.BinaryName)
	gotBin, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read netinit: %v", err)
	}
	if !bytes.Equal(gotBin, bin) {
		t.Fatalf("netinit bytes = %q, want %q", gotBin, bin)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(binPath)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o755 {
			t.Fatalf("netinit mode = %04o, want 0755", fi.Mode().Perm())
		}
		di, err := os.Stat(mountDir)
		if err != nil {
			t.Fatal(err)
		}
		if di.Mode().Perm() != 0o755 {
			t.Fatalf("mountDir mode = %04o, want 0755", di.Mode().Perm())
		}
	}

	metaPath := filepath.Join(mountDir, hostfsMetaFile)
	rawMeta, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read %s: %v", hostfsMetaFile, err)
	}
	var parsed struct {
		Version int `json:"version"`
		Entries map[string]struct {
			Mode uint32 `json:"mode"`
			UID  uint32 `json:"uid"`
			GID  uint32 `json:"gid"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(rawMeta, &parsed); err != nil {
		t.Fatalf("unmarshal %s: %v (%q)", hostfsMetaFile, err, rawMeta)
	}
	const wantMode = 0o100755 // S_IFREG | 0o755
	entry, ok := parsed.Entries[guestcfg.BinaryName]
	if parsed.Version != 2 || !ok || entry.Mode != wantMode || entry.UID != 0 || entry.GID != 0 {
		t.Fatalf("meta = %+v, want v2 entry %q with mode %#o uid 0 gid 0", parsed, guestcfg.BinaryName, wantMode)
	}

	// Repeat install is a no-op when both files match.
	binInfo1, _ := os.Stat(binPath)
	metaInfo1, _ := os.Stat(metaPath)
	mountDir2, err := Install(base, bin)
	if err != nil || mountDir2 != mountDir {
		t.Fatalf("repeat Install = (%q, %v)", mountDir2, err)
	}
	binInfo2, _ := os.Stat(binPath)
	metaInfo2, _ := os.Stat(metaPath)
	if !os.SameFile(binInfo1, binInfo2) || !os.SameFile(metaInfo1, metaInfo2) {
		t.Fatal("repeat Install rewrote unchanged files")
	}

	// Corrupted or missing .gvx_meta is repaired without rewriting netinit.
	if err := os.WriteFile(metaPath, []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(base, bin); err != nil {
		t.Fatalf("repair Install: %v", err)
	}
	if repaired, err := os.ReadFile(metaPath); err != nil || !bytes.Equal(repaired, hostfsMetaBytes) {
		t.Fatalf("repaired meta = %q, %v", repaired, err)
	}
	if binInfo3, _ := os.Stat(binPath); !os.SameFile(binInfo1, binInfo3) {
		t.Fatal("repairing .gvx_meta unexpectedly rewrote netinit")
	}

	// Binary updates replace netinit while keeping .gvx_meta intact.
	binV2 := []byte("\x7fELF-fake-netinit-v2")
	if _, err := Install(base, binV2); err != nil {
		t.Fatalf("upgrade Install: %v", err)
	}
	if got, err := os.ReadFile(binPath); err != nil || !bytes.Equal(got, binV2) {
		t.Fatalf("upgraded netinit = %q, %v", got, err)
	}

	// On Unix, missing execute permission on netinit is also repaired.
	if runtime.GOOS != "windows" {
		if err := os.Chmod(binPath, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Install(base, binV2); err != nil {
			t.Fatalf("chmod repair Install: %v", err)
		}
		if fi, err := os.Stat(binPath); err != nil || fi.Mode().Perm() != 0o755 {
			t.Fatalf("repaired netinit mode = %04o, %v", fi.Mode().Perm(), err)
		}
	}
}
