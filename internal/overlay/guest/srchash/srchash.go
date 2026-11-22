// Package srchash hashes the sources of netinit, so a test can tell that
// the binaries package guest embeds are out of date.
package srchash

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// SourceDirs are the packages netinit is built from, relative to
// package guest's directory.
var SourceDirs = []string{"../netinit", "../guestcfg"}

// SourceHash hashes the non-test Go files of SourceDirs, under dir
// (package guest's directory). The generator records it in SOURCE.
func SourceHash(dir string) (string, error) {
	h := sha256.New()
	for _, d := range SourceDirs {
		root := filepath.Join(dir, d)
		var files []string
		err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if e.IsDir() && p != root {
				return filepath.SkipDir
			}
			if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			return "", err
		}
		slices.Sort(files)
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				return "", err
			}
			rel, _ := filepath.Rel(dir, f)
			h.Write([]byte(filepath.ToSlash(rel) + "\x00"))
			h.Write([]byte(strings.ReplaceAll(string(b), "\r\n", "\n")))
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
