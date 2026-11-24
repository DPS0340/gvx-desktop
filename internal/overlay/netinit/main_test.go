//go:build linux

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

const (
	relay      = "10.0.2.3"
	relayConf  = "nameserver 10.0.2.3\n"
	debianConf = "nameserver 1.1.1.1\nnameserver 1.0.0.1\n"
)

func TestUseDNS(t *testing.T) {
	for _, c := range []struct {
		name, current, want string
	}{
		{"the image's", debianConf, relayConf},
		{"none", "", relayConf},
		{"rungv's", "nameserver 10.0.2.3\nsearch example.com\n", "nameserver 10.0.2.3\nsearch example.com\n"},
	} {
		dir := t.TempDir()
		conf, spare := filepath.Join(dir, "resolv.conf"), filepath.Join(dir, "spare")
		if c.current != "" {
			writeFile(t, conf, c.current)
		}
		if err := useDNS(conf, spare, relay); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		wantFile(t, conf, c.want)
		for _, p := range []string{conf + ".gvx-netinit", spare} {
			if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s: %s is there (%v)", c.name, p, err)
			}
		}
	}
}

// A linked resolv.conf (systemd's stub) is replaced, not written through.
func TestUseDNSReplacesALink(t *testing.T) {
	dir := t.TempDir()
	conf, stub := filepath.Join(dir, "resolv.conf"), filepath.Join(dir, "stub-resolv.conf")
	writeFile(t, stub, "nameserver 127.0.0.53\n")
	if err := os.Symlink(stub, conf); err != nil {
		t.Fatal(err)
	}
	if err := useDNS(conf, filepath.Join(dir, "spare"), relay); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(conf); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("resolv.conf is %v, %v; want a file", fi.Mode(), err)
	}
	wantFile(t, conf, relayConf)
	wantFile(t, stub, "nameserver 127.0.0.53\n")
}

// On a read-only root (docker run --read-only) resolv.conf cannot be
// replaced, so netinit binds a copy from a writable directory over it. The
// test runs again in a mount namespace of its own, and a user namespace
// unless it is root, where it can make a directory read-only. It skips where
// those cannot be made.
func TestUseDNSOnReadOnlyRoot(t *testing.T) {
	const env = "GVX_NETINIT_TEST_READONLY_DIR"
	if dir := os.Getenv(env); dir != "" {
		useDNSOnReadOnlyDir(t, dir)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestUseDNSOnReadOnlyRoot$", "-test.v")
	cmd.Env = append(os.Environ(), env+"="+t.TempDir())
	cmd.SysProcAttr = &syscall.SysProcAttr{Unshareflags: syscall.CLONE_NEWNS}
	if os.Getuid() != 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
			UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
			GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		}
	}
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err != nil && !errors.As(err, &exit):
		t.Skipf("no namespaces: %v", err)
	case bytes.Contains(out, []byte("--- SKIP")):
		t.Skipf("%s", out)
	case err != nil || !bytes.Contains(out, []byte("--- PASS")):
		t.Fatalf("%v\n%s", err, out)
	}
}

func useDNSOnReadOnlyDir(t *testing.T, dir string) {
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		t.Skipf("mounts are shared: %v", err)
	}
	if err := syscall.Mount("tmpfs", dir, "tmpfs", 0, ""); err != nil {
		t.Skipf("cannot mount: %v", err)
	}
	etc, dev := filepath.Join(dir, "etc"), filepath.Join(dir, "dev")
	for _, d := range []string{etc, dev} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	conf := filepath.Join(etc, "resolv.conf")
	writeFile(t, conf, debianConf)
	if err := syscall.Mount(etc, etc, "", syscall.MS_BIND, ""); err != nil {
		t.Skipf("cannot bind: %v", err)
	}
	if err := syscall.Mount("", etc, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
		t.Skipf("cannot make %s read-only: %v", etc, err)
	}
	if err := os.WriteFile(conf, nil, 0o644); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("writing to the read-only directory: %v, want EROFS", err)
	}

	spare := filepath.Join(dev, ".gvx-resolv.conf")
	if err := useDNS(conf, spare, relay); err != nil {
		t.Fatal(err)
	}
	wantFile(t, conf, relayConf)
	wantFile(t, spare, relayConf)
	// Called again, it finds the relay named and binds nothing more.
	if err := useDNS(conf, spare, relay); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Unmount(conf, 0); err != nil {
		t.Fatal(err)
	}
	wantFile(t, conf, debianConf)
}

func writeFile(t *testing.T, name, data string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func wantFile(t *testing.T, name, want string) {
	t.Helper()
	if b, err := os.ReadFile(name); err != nil || string(b) != want {
		t.Errorf("%s = %q, %v; want %q", name, b, err, want)
	}
}
