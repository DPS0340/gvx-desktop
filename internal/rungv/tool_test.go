package rungv

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A command in a directory the PATH leaves out is found all the same: the
// home directory's tool directories are looked in.
func TestFindToolLooksPastAShortPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the directories differ")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("SHELL", "") // the shell would read the real profile
	bin := filepath.Join(home, ".docker", "bin")
	os.MkdirAll(bin, 0o755)
	tool := filepath.Join(bin, "gvxd-test-tool")
	os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755)

	got, err := FindTool("gvxd-test-tool")
	if err != nil || got != tool {
		t.Errorf("FindTool = %q, %v; want %q", got, err, tool)
	}
	if _, err := FindTool("gvxd-no-such-tool"); !errors.Is(err, ErrToolNotFound) {
		t.Errorf("a missing tool: %v", err)
	}
}
