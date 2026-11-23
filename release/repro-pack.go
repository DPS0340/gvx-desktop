//go:build ignore

// Command repro-pack writes deterministic zip and tar.gz archives with fixed
// timestamps, modes, and root ownership so archives match bit for bit across
// Linux, macOS, and Windows hosts.
//
// Usage:
//
//	go run ./release/repro-pack.go zip EPOCH OUT.zip DIR_OR_FILE [FILE...]
//	go run ./release/repro-pack.go tar EPOCH OUT.tar.gz TREE_DIR
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 5 {
		fmt.Fprintf(os.Stderr, "usage: repro-pack <zip|tar> EPOCH OUT SRC...\n")
		os.Exit(2)
	}
	mode := os.Args[1]
	epoch, err := strconv.ParseInt(os.Args[2], 10, 64)
	if err != nil {
		fatalf("invalid SOURCE_DATE_EPOCH %q: %v", os.Args[2], err)
	}
	mtime := time.Unix(epoch, 0).UTC()
	outPath := os.Args[3]
	srcArgs := os.Args[4:]

	switch mode {
	case "zip":
		err = packZip(outPath, srcArgs, mtime)
	case "tar":
		if len(srcArgs) != 1 {
			fmt.Fprintf(os.Stderr, "usage: repro-pack tar EPOCH OUT.tar.gz TREE_DIR\n")
			os.Exit(2)
		}
		err = packTarGz(outPath, srcArgs[0], mtime)
	default:
		fatalf("unknown mode %q (expected zip or tar)", mode)
	}
	if err != nil {
		fatalf("%v", err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "repro-pack: "+format+"\n", args...)
	os.Exit(1)
}

func packZip(outPath string, srcFiles []string, mtime time.Time) (err error) {
	type zipEntry struct {
		name string
		src  string
		mode fs.FileMode
	}
	var entries []zipEntry
	seen := make(map[string]bool)
	addEntry := func(name, src string) error {
		name = filepath.ToSlash(name)
		if seen[name] {
			return fmt.Errorf("duplicate zip entry %q", name)
		}
		seen[name] = true
		mode := fs.FileMode(0o644)
		if strings.EqualFold(filepath.Ext(name), ".exe") {
			mode = 0o755
		}
		entries = append(entries, zipEntry{name: name, src: src, mode: mode})
		return nil
	}
	for _, src := range srcFiles {
		fi, err := os.Stat(src)
		if err != nil {
			return err
		}
		if fi.IsDir() {
			err = filepath.WalkDir(src, func(p string, d fs.DirEntry, werr error) error {
				if werr != nil {
					return werr
				}
				if d.IsDir() {
					return nil
				}
				if !d.Type().IsRegular() {
					return fmt.Errorf("unsupported non-regular zip entry %s", p)
				}
				rel, err := filepath.Rel(src, p)
				if err != nil {
					return err
				}
				return addEntry(rel, p)
			})
			if err != nil {
				return err
			}
			continue
		}
		if err := addEntry(filepath.Base(src), src); err != nil {
			return err
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(outPath)
		}
	}()

	zw := zip.NewWriter(f)
	for _, e := range entries {
		h := &zip.FileHeader{
			Name:     e.name,
			Method:   zip.Deflate,
			Modified: mtime,
		}
		h.SetMode(e.mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		if err := copyFile(w, e.src); err != nil {
			return err
		}
	}
	return zw.Close()
}

type tarEntry struct {
	rel  string
	src  string
	dir  bool
	mode fs.FileMode
}

func modeFor(rel string) fs.FileMode {
	dir := path.Dir(rel)
	if dir == "usr/bin" ||
		(strings.HasSuffix(dir, "/bin") && strings.HasPrefix(dir, "usr/lib/")) ||
		(strings.HasSuffix(dir, "/cli-plugins") && strings.HasPrefix(dir, "usr/lib/")) {
		return 0o755
	}
	return 0o644
}

func packTarGz(outPath, treeDir string, mtime time.Time) (err error) {
	usrDir := filepath.Join(treeDir, "usr")
	var entries []tarEntry
	err = filepath.WalkDir(usrDir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, err := filepath.Rel(treeDir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			entries = append(entries, tarEntry{rel: rel, src: p, dir: true, mode: 0o755})
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("unsupported non-regular entry %s", rel)
		}
		entries = append(entries, tarEntry{rel: rel, src: p, mode: modeFor(rel)})
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })

	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(outPath)
		}
	}()

	gz, err := gzip.NewWriterLevel(f, gzip.BestCompression)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{
			Format:  tar.FormatUSTAR,
			Name:    e.rel,
			Mode:    int64(e.mode),
			Uid:     0,
			Gid:     0,
			Uname:   "root",
			Gname:   "root",
			ModTime: mtime,
		}
		if e.dir {
			h.Typeflag = tar.TypeDir
			h.Name += "/"
		} else {
			fi, err := os.Stat(e.src)
			if err != nil {
				return err
			}
			h.Typeflag = tar.TypeReg
			h.Size = fi.Size()
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if !e.dir {
			if err := copyFile(tw, e.src); err != nil {
				return err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func copyFile(w io.Writer, src string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}
