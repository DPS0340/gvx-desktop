package guest

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/ironpark/gvx-desktop/internal/overlay/guest/srchash"
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
