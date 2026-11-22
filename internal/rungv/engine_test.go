package rungv

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// makeArgRecorder writes a tiny fake rungv script that prints its argv (one
// arg per line) to stdout and exits 0. On Windows we skip script tests.
func makeArgRecorder(t *testing.T) (*Client, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell recorder test runs on unix")
	}
	dir := t.TempDir()
	logFile := filepath.Join(dir, "args.log")
	bin := filepath.Join(dir, "fake-rungv")
	script := "#!/bin/sh\n" +
		": > \"" + logFile + "\"\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\" >> \"" + logFile + "\"; done\n" +
		"case \"$1\" in\n" +
		"  inspect) printf '[]\\n' ;;\n" +
		"  save) printf 'tar-bytes' ;;\n" +
		"esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Client{Path: bin, CacheDir: "/tmp/test-cache"}, logFile
}

func readRecordedArgs(t *testing.T, logFile string) []string {
	t.Helper()
	b, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.TrimRight(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func TestEngineArchFlags(t *testing.T) {
	c, logFile := makeArgRecorder(t)
	ctx := context.Background()

	if _, err := c.ImagesFullArch(ctx, "amd64"); err != nil {
		t.Fatal(err)
	}
	want := []string{"images", "-cache-dir", "/tmp/test-cache", "-arch", "amd64", "-a", "--no-trunc", "--digests", "--format", "json"}
	if got := readRecordedArgs(t, logFile); !slices.Equal(got, want) {
		t.Errorf("ImagesFullArch args = %v, want %v", got, want)
	}

	if _, err := c.InspectImagesRawArch(ctx, "amd64", "gvxd-amd64:1"); err != nil {
		t.Fatal(err)
	}
	want = []string{"inspect", "-cache-dir", "/tmp/test-cache", "-arch", "amd64", "--", "gvxd-amd64:1"}
	if got := readRecordedArgs(t, logFile); !slices.Equal(got, want) {
		t.Errorf("InspectImagesRawArch args = %v, want %v", got, want)
	}

	if err := c.PullPlatform(ctx, "alpine:3.20", "linux/amd64", nil); err != nil {
		t.Fatal(err)
	}
	want = []string{"pull", "-cache-dir", "/tmp/test-cache", "-arch", "amd64", "-platform", "linux/amd64", "--", "alpine:3.20"}
	if got := readRecordedArgs(t, logFile); !slices.Equal(got, want) {
		t.Errorf("PullPlatform args = %v, want %v", got, want)
	}

	if err := c.RemoveImageSafeArch(ctx, "amd64", "gvxd-amd64:1", true); err != nil {
		t.Fatal(err)
	}
	want = []string{"rmi", "-cache-dir", "/tmp/test-cache", "-arch", "amd64", "-f", "--", "gvxd-amd64:1"}
	if got := readRecordedArgs(t, logFile); !slices.Equal(got, want) {
		t.Errorf("RemoveImageSafeArch args = %v, want %v", got, want)
	}

	if err := c.TagImageArch(ctx, "amd64", "a:1", "b:2"); err != nil {
		t.Fatal(err)
	}
	want = []string{"tag", "-cache-dir", "/tmp/test-cache", "-arch", "amd64", "--", "a:1", "b:2"}
	if got := readRecordedArgs(t, logFile); !slices.Equal(got, want) {
		t.Errorf("TagImageArch args = %v, want %v", got, want)
	}

	if err := c.LoadImagesArch(ctx, "amd64", strings.NewReader("archive"), nil); err != nil {
		t.Fatal(err)
	}
	want = []string{"load", "-cache-dir", "/tmp/test-cache", "-arch", "amd64"}
	if got := readRecordedArgs(t, logFile); !slices.Equal(got, want) {
		t.Errorf("LoadImagesArch args = %v, want %v", got, want)
	}

	var saved bytes.Buffer
	if err := c.SaveImagesArch(ctx, "amd64", &saved, "gvxd-amd64:1"); err != nil {
		t.Fatal(err)
	}
	want = []string{"save", "-cache-dir", "/tmp/test-cache", "-arch", "amd64", "--", "gvxd-amd64:1"}
	if got := readRecordedArgs(t, logFile); !slices.Equal(got, want) {
		t.Errorf("SaveImagesArch args = %v, want %v", got, want)
	}

	if _, err := c.PruneImagesArch(ctx, "amd64", false); err != nil {
		t.Fatal(err)
	}
	want = []string{"image", "prune", "-cache-dir", "/tmp/test-cache", "-f", "-arch", "amd64"}
	if got := readRecordedArgs(t, logFile); !slices.Equal(got, want) {
		t.Errorf("PruneImagesArch args = %v, want %v", got, want)
	}

	// Leading dash in arch must be rejected.
	if _, err := c.ImagesFullArch(ctx, "-bad"); err == nil {
		t.Errorf("ImagesFullArch(-bad) succeeded, want error")
	}
}
