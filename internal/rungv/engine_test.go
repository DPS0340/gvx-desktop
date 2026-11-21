package rungv

// Owner: workstream C.

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestEngineClientArgs(t *testing.T) {
	c := &Client{Path: "/usr/bin/rungv", CacheDir: "/tmp/gvxd-test-cache"}
	cases := []struct {
		in   []string
		want []string
	}{
		{
			[]string{"images", "-a", "--no-trunc", "--format", "json"},
			[]string{"images", "-cache-dir", "/tmp/gvxd-test-cache", "-a", "--no-trunc", "--format", "json"},
		},
		{
			[]string{"inspect", "alpine:3.20"},
			[]string{"inspect", "-cache-dir", "/tmp/gvxd-test-cache", "alpine:3.20"},
		},
		{
			[]string{"pull", "-platform", "linux/arm64", "alpine:3.20"},
			[]string{"pull", "-cache-dir", "/tmp/gvxd-test-cache", "-platform", "linux/arm64", "alpine:3.20"},
		},
		{
			[]string{"tag", "alpine:3.20", "gvxd-tag:v1"},
			[]string{"tag", "-cache-dir", "/tmp/gvxd-test-cache", "alpine:3.20", "gvxd-tag:v1"},
		},
		{
			[]string{"load"},
			[]string{"load", "-cache-dir", "/tmp/gvxd-test-cache"},
		},
		{
			[]string{"save", "alpine:3.20"},
			[]string{"save", "-cache-dir", "/tmp/gvxd-test-cache", "alpine:3.20"},
		},
		{
			[]string{"image", "prune", "-f"},
			[]string{"image", "prune", "-cache-dir", "/tmp/gvxd-test-cache", "-f"},
		},
		{
			[]string{"volume", "create", "-d", "ext4lite", "gvxd-v1"},
			[]string{"volume", "create", "-cache-dir", "/tmp/gvxd-test-cache", "-d", "ext4lite", "gvxd-v1"},
		},
	}
	for _, tc := range cases {
		got := c.Args(tc.in...)
		if !slices.Equal(got, tc.want) {
			t.Errorf("Args(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestEngineRejectsLeadingDashArgs(t *testing.T) {
	c := &Client{Path: "/nonexistent/rungv", CacheDir: "/tmp/gvxd-test-cache"}
	ctx := context.Background()

	if _, err := c.InspectImagesRaw(ctx, "-cache-dir"); err == nil || !strings.Contains(err.Error(), "must not start with '-'") {
		t.Errorf("InspectImagesRaw(-cache-dir) = %v, want dash error", err)
	}
	if err := c.PullPlatform(ctx, "-bad", "", nil); err == nil || !strings.Contains(err.Error(), "must not start with '-'") {
		t.Errorf("PullPlatform(-bad) = %v, want dash error", err)
	}
	if err := c.PullPlatform(ctx, "alpine:3.20", "-bad", nil); err == nil || !strings.Contains(err.Error(), "must not start with '-'") {
		t.Errorf("PullPlatform(platform=-bad) = %v, want dash error", err)
	}
	if err := c.RemoveImageSafe(ctx, "-bad", false); err == nil || !strings.Contains(err.Error(), "must not start with '-'") {
		t.Errorf("RemoveImageSafe(-bad) = %v, want dash error", err)
	}
	if err := c.TagImage(ctx, "-src", "dst:1"); err == nil || !strings.Contains(err.Error(), "must not start with '-'") {
		t.Errorf("TagImage(-src) = %v, want dash error", err)
	}
	if err := c.TagImage(ctx, "src:1", "-dst"); err == nil || !strings.Contains(err.Error(), "must not start with '-'") {
		t.Errorf("TagImage(-dst) = %v, want dash error", err)
	}
	if err := c.SaveImages(ctx, &bytes.Buffer{}, "-bad"); err == nil || !strings.Contains(err.Error(), "must not start with '-'") {
		t.Errorf("SaveImages(-bad) = %v, want dash error", err)
	}
	if err := c.CreateVolumeSafe(ctx, "-bad", "ext4lite"); err == nil || !strings.Contains(err.Error(), "must not start with '-'") {
		t.Errorf("CreateVolumeSafe(-bad) = %v, want dash error", err)
	}
	if err := c.RemoveVolumeSafe(ctx, "-bad"); err == nil || !strings.Contains(err.Error(), "must not start with '-'") {
		t.Errorf("RemoveVolumeSafe(-bad) = %v, want dash error", err)
	}
}

func TestEngineHelpersIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping rungv integration test under -short")
	}
	path, err := Locate("")
	if err != nil {
		t.Skip("rungv not found")
	}
	cache := t.TempDir()
	c := &Client{Path: path, CacheDir: cache}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var lines []string
	if err := c.PullPlatform(ctx, "alpine:3.20", "", func(s string) {
		lines = append(lines, s)
	}); err != nil {
		t.Fatalf("PullPlatform: %v", err)
	}

	imgs, err := c.ImagesFull(ctx)
	if err != nil {
		t.Fatalf("ImagesFull: %v", err)
	}
	if len(imgs) == 0 {
		t.Fatal("expected at least one image after pull")
	}
	if !strings.HasPrefix(imgs[0].ID, "sha256:") || len(imgs[0].ID) != len("sha256:")+64 {
		t.Fatalf("expected full sha256 ID, got %q", imgs[0].ID)
	}

	raw, err := c.InspectImagesRaw(ctx, "alpine:3.20")
	if err != nil {
		t.Fatalf("InspectImagesRaw: %v", err)
	}
	var inspected []struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(raw, &inspected); err != nil || len(inspected) != 1 {
		t.Fatalf("InspectImagesRaw decode failed: err=%v len=%d", err, len(inspected))
	}

	if err := c.TagImage(ctx, "alpine:3.20", "gvxd-test-tag:v1"); err != nil {
		t.Fatalf("TagImage: %v", err)
	}

	var tarBuf bytes.Buffer
	if err := c.SaveImages(ctx, &tarBuf, "alpine:3.20"); err != nil {
		t.Fatalf("SaveImages: %v", err)
	}
	if tarBuf.Len() == 0 {
		t.Fatal("SaveImages wrote 0 bytes")
	}

	// Load into a second isolated temp cache.
	c2 := &Client{Path: path, CacheDir: t.TempDir()}
	var loadLines []string
	if err := c2.LoadImages(ctx, bytes.NewReader(tarBuf.Bytes()), func(s string) {
		loadLines = append(loadLines, s)
	}); err != nil {
		t.Fatalf("LoadImages: %v", err)
	}
	loaded, err := c2.ImagesFull(ctx)
	if err != nil || len(loaded) == 0 {
		t.Fatalf("ImagesFull in second cache: err=%v len=%d", err, len(loaded))
	}
}
