//go:build unix

package ext4lite

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// TestOpenInUse: a writable mount holds the volume's flock exclusively.
func TestOpenInUse(t *testing.T) {
	path := fixture(t, "layer.ext4")
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if v, err := Open(path); !errors.Is(err, ErrInUse) {
		if err == nil {
			v.Close()
		}
		t.Fatalf("Open while a writer holds it = %v, want ErrInUse", err)
	}
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)

	v := open(t, path)
	// And while the reader holds it, a writer cannot start.
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
		t.Error("a writer took the lock while the volume was open")
	}
	v.Close()
}
