package rungv

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// Entitlements is the entitlements.plist that gvx ships: Hypervisor.framework
// access for the hvf engine.
const Entitlements = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>com.apple.security.hypervisor</key>
	<true/>
	<key>com.apple.security.cs.allow-jit</key>
	<true/>
</dict>
</plist>
`

// NeedsSigning reports whether info shows a macOS host whose rungv cannot
// reach Hypervisor.framework.
func NeedsSigning(info Info) bool {
	if info.OS != "darwin" {
		return false
	}
	return !slices.Contains(info.AvailableBackends, "hvf")
}

// SignCommand is the command line Sign runs, for display.
func SignCommand(path string) string {
	return "codesign --sign - --entitlements entitlements.plist --force " + Quote(path)
}

// Sign ad-hoc signs the binary at path with Entitlements (macOS only).
func Sign(ctx context.Context, path string) error {
	if runtime.GOOS != "darwin" {
		return errors.New("signing is needed on macOS only")
	}
	dir, err := os.MkdirTemp("", "gvx-desktop-sign-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	plist := filepath.Join(dir, "entitlements.plist")
	if err := os.WriteFile(plist, []byte(Entitlements), 0o600); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, "codesign", "--sign", "-", "--entitlements", plist, "--force", path).CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return errors.New(msg)
		}
		return err
	}
	return nil
}
