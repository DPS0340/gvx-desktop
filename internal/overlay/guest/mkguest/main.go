// Command mkguest builds netinit for linux/arm64 and linux/amd64 and
// writes the compressed binaries and SOURCE next to package guest. go
// generate runs it in that package's directory.
package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/ironpark/gvx-desktop/internal/overlay/guest/srchash"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mkguest:", err)
		os.Exit(1)
	}
}

func run() error {
	tmp, err := os.MkdirTemp("", "mkguest-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for _, arch := range []string{"arm64", "amd64"} {
		out := filepath.Join(tmp, "netinit-"+arch)
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w -buildid=", "-o", out, "../netinit")
		cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0", "GOFLAGS=-mod=mod")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("build linux/%s: %w", arch, err)
		}
		bin, err := os.ReadFile(out)
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if err != nil {
			return err
		}
		if _, err := zw.Write(bin); err != nil {
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
		if err := os.WriteFile("netinit-linux-"+arch+".gz", buf.Bytes(), 0o644); err != nil {
			return err
		}
	}
	sum, err := srchash.SourceHash(".")
	if err != nil {
		return err
	}
	return os.WriteFile("SOURCE", []byte(sum+"\n"), 0o644)
}
