package appupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/ironpark/gvx-desktop/internal/ghrelease"
)

// BundleID is the macOS app's identifier, which a new bundle must have.
const BundleID = "io.github.ironpark.gvx-desktop"

// maxDownload bounds what an update downloads, and what it unpacks of
// that: several times a release's size.
const maxDownload = 512 << 20

// stagePrefix starts the name of the folder a new macOS bundle waits in,
// beside the one it replaces.
const stagePrefix = ".gvx-desktop-update-"

// downloadPrefix starts the name of a program's archive while it
// downloads, beside the program.
const downloadPrefix = ".gvx-desktop-download-"

// ErrNoBuild reports a release with no build of the app for this machine.
var ErrNoBuild = errors.New("the release has no build of GVX Desktop for this system")

// ErrNotSelf reports an install the app does not update itself.
var ErrNotSelf = errors.New("this copy of GVX Desktop does not update itself")

// Staged is an update downloaded, checked against its checksum, and put
// beside the app it replaces, for Prepare and Finish to put in place.
type Staged struct {
	Tag    string // the release's
	Method Method // Bundle or Program
	Target string // the bundle or program the update replaces

	fresh   string // the new bundle or program, beside Target
	old     string // a program: where the one it replaces steps aside to
	stage   string // a bundle: the folder fresh is in
	script  string // a bundle: the helper Prepare wrote
	goos    string
	args    []string
	swapped bool // a program: fresh is at Target, and the old one at old
}

// Stage downloads the build of rel that installs the app as in has it,
// checks it, and puts it beside the app. Nothing of the app changes yet.
// progress, when not nil, hears how much of the download has arrived.
func Stage(ctx context.Context, rel Release, in Install, progress func(done, total int64)) (*Staged, error) {
	switch in.Method {
	case Bundle:
		return stageBundle(ctx, rel, in.Path, progress)
	case Program:
		return stageProgram(ctx, rel, in.Path, runtime.GOOS, runtime.GOARCH, progress)
	}
	return nil, ErrNotSelf
}

// fetch downloads the file name of rel to w, checked against the checksum
// the release gives it in name.sha256. A file the release has no checksum
// for is not installed.
func fetch(ctx context.Context, rel Release, name string, w io.Writer, progress func(done, total int64)) error {
	a, ok := rel.Find(name)
	if !ok {
		return fmt.Errorf("%w: %s is not in release %s", ErrNoBuild, name, rel.Tag)
	}
	sums, ok := rel.Find(name + ".sha256")
	if !ok {
		return fmt.Errorf("the release has no checksum for %s, so it is not installed", name)
	}
	want, err := ghrelease.Checksum(ctx, sums.URL, name)
	if err != nil {
		return err
	}
	return ghrelease.Download(ctx, a, want, w, maxDownload, progress)
}

// programPaths are where a new program waits beside target, and where
// target steps aside to when the new one takes its place.
func programPaths(target, goos string) (fresh, old string) {
	if goos == "windows" {
		base := strings.TrimSuffix(target, filepath.Ext(target))
		return base + ".new.exe", base + ".old.exe"
	}
	return target + ".new", target + ".old"
}

// stageProgram downloads the archive of rel for goos and goarch beside
// target, and unpacks its program there, at programPaths' fresh.
func stageProgram(ctx context.Context, rel Release, target, goos, goarch string, progress func(done, total int64)) (*Staged, error) {
	name, ok := AssetName(rel.Tag, goos, goarch, Program)
	if !ok {
		return nil, fmt.Errorf("%w (%s/%s)", ErrNoBuild, goos, goarch)
	}
	archive, err := os.CreateTemp(filepath.Dir(target), downloadPrefix+"*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	if err := fetch(ctx, rel, name, archive, progress); err != nil {
		return nil, err
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	fresh, old := programPaths(target, goos)
	out, err := os.OpenFile(fresh, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return nil, err
	}
	if goos == "windows" {
		err = unzipProgram(archive, out)
	} else {
		err = untarProgram(archive, out)
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = checkProgram(fresh, goos)
	}
	if err == nil {
		err = os.Chmod(fresh, 0o755)
	}
	if err != nil {
		os.Remove(fresh)
		return nil, err
	}
	return &Staged{Tag: rel.Tag, Method: Program, Target: target, fresh: fresh, old: old, goos: goos}, nil
}

// unzipProgram copies the program out of a Windows release's zip, which
// holds the .exe alone.
func unzipProgram(archive *os.File, out io.Writer) error {
	st, err := archive.Stat()
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(archive, st.Size())
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !strings.EqualFold(path.Ext(f.Name), ".exe") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		return copyCapped(out, rc)
	}
	return errors.New("the archive holds no program")
}

// untarProgram copies the program out of a Linux release's tarball, which
// lays out /usr: usr/bin/gvx-desktop.
func untarProgram(archive io.Reader, out io.Writer) error {
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return errors.New("the archive holds no program")
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg && path.Clean(strings.TrimPrefix(h.Name, "./")) == "usr/bin/"+Package {
			return copyCapped(out, tr)
		}
	}
}

func copyCapped(out io.Writer, in io.Reader) error {
	n, err := io.Copy(out, io.LimitReader(in, maxDownload+1))
	if err == nil && n > maxDownload {
		err = ghrelease.ErrTooLarge
	}
	return err
}

// checkProgram makes sure what was unpacked is a program for goos at all:
// a Windows .exe starts MZ, a Linux one is ELF.
func checkProgram(p, goos string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, 4)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	magic := []byte("\x7fELF")
	if goos == "windows" {
		magic = []byte("MZ")
	}
	if !bytes.HasPrefix(head, magic) {
		return fmt.Errorf("the release's program is not one for %s", goos)
	}
	return nil
}

// swap puts fresh in target's place, moving target to old first: a
// running program's file may be renamed, though Windows lets it be neither
// written over nor removed. When fresh does not go in, target comes back.
func swap(target, fresh, old string) error {
	if err := os.Remove(old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("an earlier update left %s, which cannot be removed: %w", old, err)
	}
	if err := os.Rename(target, old); err != nil {
		return err
	}
	if err := os.Rename(fresh, target); err != nil {
		if rerr := os.Rename(old, target); rerr != nil {
			return fmt.Errorf("%w; putting the old program back failed too, and it is at %s: %v", err, old, rerr)
		}
		return err
	}
	return nil
}

// unswap undoes swap: the old program goes back in target's place, and
// the new one back to fresh.
func unswap(target, fresh, old string) error {
	if err := os.Rename(target, fresh); err != nil {
		return err
	}
	if err := os.Rename(old, target); err != nil {
		os.Rename(fresh, target) // the new one at least, rather than none
		return err
	}
	return nil
}

// Prepare readies the update to go in when the app quits, which starts it
// with args. A program goes in place now, the one running stepping aside,
// and back should the new one not go in. A bundle stays where it waits:
// the helper Finish starts moves it once the app has quit.
func (s *Staged) Prepare(args []string) error {
	s.args = args
	switch s.Method {
	case Program:
		if s.swapped {
			return nil
		}
		if err := swap(s.Target, s.fresh, s.old); err != nil {
			return err
		}
		s.swapped = true
		return nil
	case Bundle:
		s.script = filepath.Join(s.stage, "finish.sh")
		return os.WriteFile(s.script, []byte(helperScript(os.Getpid(), s.Target, s.fresh, s.stage, "/usr/bin/open", args)), 0o700)
	}
	return ErrNotSelf
}

// Finish starts the updated app. Call it last, once the app has ended all
// it runs. A program is started from its place; should it not start, the
// old one goes back and starts instead. For a bundle, the helper starts,
// and waits for this process to end before it moves anything.
func (s *Staged) Finish() error {
	switch s.Method {
	case Program:
		if !s.swapped {
			return errors.New("the update was not put in place")
		}
		if err := start(s.Target, s.args); err != nil {
			if uerr := unswap(s.Target, s.fresh, s.old); uerr != nil {
				return fmt.Errorf("the new program did not start: %w; putting the old one back failed: %v", err, uerr)
			}
			if serr := start(s.Target, s.args); serr != nil {
				return fmt.Errorf("the new program did not start: %w; the old one, put back, did not either: %v", err, serr)
			}
			return fmt.Errorf("the new program did not start, and the old one was put back: %w", err)
		}
		if s.goos != "windows" {
			os.Remove(s.old) // a Unix program's file may go while it runs
		}
		return nil
	case Bundle:
		if s.script == "" {
			return errors.New("the update was not prepared")
		}
		cmd := exec.Command("/bin/sh", s.script)
		if log, err := os.Create(filepath.Join(os.TempDir(), "gvx-desktop-update.log")); err == nil {
			cmd.Stdout, cmd.Stderr = log, log
			defer log.Close()
		}
		detach(cmd)
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}
	return ErrNotSelf
}

// Discard removes an update that is not to go in.
func (s *Staged) Discard() error {
	switch s.Method {
	case Program:
		if s.swapped {
			return nil // it is in place; the old one goes at the next start
		}
		return os.Remove(s.fresh)
	case Bundle:
		return os.RemoveAll(s.stage)
	}
	return nil
}

// start runs program with args, apart from this process, which is about
// to end.
func start(program string, args []string) error {
	cmd := exec.Command(program, args...)
	cmd.Dir = filepath.Dir(program)
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// Cleanup removes what an update left beside the app as in has it: the
// program it replaced, which Windows does not let go of while it runs, a
// new program that never went in, a download cut short, or a bundle's
// folder. Call it when the app starts, before any update is staged.
func Cleanup(in Install) error { return cleanup(in, runtime.GOOS) }

func cleanup(in Install, goos string) error {
	var errs []error
	remove := func(p string, all bool) {
		rm := os.Remove
		if all {
			rm = os.RemoveAll
		}
		if err := rm(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	switch in.Method {
	case Program:
		fresh, old := programPaths(in.Path, goos)
		remove(fresh, false)
		remove(old, false)
		left, _ := filepath.Glob(filepath.Join(filepath.Dir(in.Path), downloadPrefix+"*"))
		for _, p := range left {
			remove(p, false)
		}
	case Bundle:
		left, _ := filepath.Glob(filepath.Join(filepath.Dir(in.Path), stagePrefix+"*"))
		for _, p := range left {
			remove(p, true)
		}
	}
	return errors.Join(errs...)
}

// RemoveReplaced removes the program an update replaced, which Windows
// lets go of once the old app has ended: it reports whether it is gone.
func RemoveReplaced(in Install) bool {
	if in.Method != Program {
		return true
	}
	_, old := programPaths(in.Path, runtime.GOOS)
	err := os.Remove(old)
	return err == nil || errors.Is(err, fs.ErrNotExist)
}

// stageBundle downloads the disk image of rel, copies the app out of it
// into a folder beside bundle, on the same volume, and checks its
// signature and identifier.
func stageBundle(ctx context.Context, rel Release, bundle string, progress func(done, total int64)) (*Staged, error) {
	name, ok := AssetName(rel.Tag, "darwin", runtime.GOARCH, Bundle)
	if !ok {
		return nil, fmt.Errorf("%w (darwin/%s)", ErrNoBuild, runtime.GOARCH)
	}
	tmp, err := os.MkdirTemp("", "gvx-desktop-update-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	dmg := filepath.Join(tmp, name)
	f, err := os.Create(dmg)
	if err != nil {
		return nil, err
	}
	err = fetch(ctx, rel, name, f, progress)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp(filepath.Dir(bundle), stagePrefix)
	if err != nil {
		return nil, err
	}
	fresh := filepath.Join(stage, filepath.Base(bundle))
	if err := copyFromImage(ctx, dmg, filepath.Join(tmp, "mnt"), fresh); err != nil {
		os.RemoveAll(stage)
		return nil, err
	}
	if err := verifyBundle(ctx, fresh); err != nil {
		os.RemoveAll(stage)
		return nil, err
	}
	return &Staged{Tag: rel.Tag, Method: Bundle, Target: bundle, fresh: fresh, stage: stage, goos: "darwin"}, nil
}

// copyFromImage mounts the disk image dmg at mnt, read-only and out of the
// Finder's sight, copies the app in it to dst, and unmounts it.
func copyFromImage(ctx context.Context, dmg, mnt, dst string) error {
	if err := os.MkdirAll(mnt, 0o700); err != nil {
		return err
	}
	if _, err := run(ctx, "/usr/bin/hdiutil", "attach", "-nobrowse", "-readonly", "-noautoopen", "-mountpoint", mnt, dmg); err != nil {
		return err
	}
	defer func() {
		if _, err := run(context.Background(), "/usr/bin/hdiutil", "detach", mnt); err != nil {
			run(context.Background(), "/usr/bin/hdiutil", "detach", "-force", mnt)
		}
	}()
	app, err := findApp(mnt)
	if err != nil {
		return err
	}
	_, err = run(ctx, "/usr/bin/ditto", app, dst)
	return err
}

// findApp is the one app bundle at the top of a mounted disk image, which
// also holds a link to /Applications.
func findApp(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var apps []string
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), ".app") {
			apps = append(apps, filepath.Join(dir, e.Name()))
		}
	}
	if len(apps) != 1 {
		return "", fmt.Errorf("the disk image holds %d apps, not one", len(apps))
	}
	return apps[0], nil
}

// verifyBundle checks that app is sealed by its signature, an ad hoc one
// or a Developer ID's, and is this app.
func verifyBundle(ctx context.Context, app string) error {
	if _, err := run(ctx, "/usr/bin/codesign", "--verify", "--deep", "--strict", app); err != nil {
		return fmt.Errorf("the new app's signature does not hold: %w", err)
	}
	id, err := run(ctx, "/usr/bin/plutil", "-extract", "CFBundleIdentifier", "raw", "-o", "-", filepath.Join(app, "Contents", "Info.plist"))
	if err != nil {
		return err
	}
	if id = strings.TrimSpace(id); id != BundleID {
		return fmt.Errorf("the new app is %s, not %s", id, BundleID)
	}
	return nil
}

// run runs a command and returns what it printed, which an error carries.
func run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(out.String()); msg != "" {
			return "", fmt.Errorf("%s: %w: %s", filepath.Base(name), err, msg)
		}
		return "", fmt.Errorf("%s: %w", filepath.Base(name), err)
	}
	return out.String(), nil
}

// helperWait is how long the helper waits for the app to end, in half
// seconds: five minutes, more than stopping its sandboxes takes.
const helperWait = 600

// helperScript is the shell script that finishes a bundle's update: it
// waits for the process pid, the app, to end; moves the bundle app into
// the stage and the new bundle fresh into its place, the old one going
// back if the new one does not; removes the stage, the old bundle with it;
// and opens the app with open, the new one or the old, with args.
func helperScript(pid int, app, fresh, stage, open string, args []string) string {
	launch := shellQuote(open) + ` "$app"`
	if len(args) > 0 {
		launch += " --args"
		for _, a := range args {
			launch += " " + shellQuote(a)
		}
	}
	return `#!/bin/sh
# GVX Desktop wrote this to finish its update once it has quit: the new app
# goes in place of the old, which stays until the new one is in, and opens.
pid=` + strconv.Itoa(pid) + `
app=` + shellQuote(app) + `
new=` + shellQuote(fresh) + `
stage=` + shellQuote(stage) + `
old="$stage/previous.app"
waited=0
while kill -0 "$pid" 2>/dev/null; do
	if [ "$waited" -ge ` + strconv.Itoa(helperWait) + ` ]; then
		echo "the app did not quit; the update is left in $stage" >&2
		exit 1
	fi
	sleep 0.5
	waited=$((waited + 1))
done
if mv "$app" "$old"; then
	if ! mv "$new" "$app"; then
		echo "the new app did not go in place; the old one goes back" >&2
		mv "$old" "$app"
	fi
else
	echo "the old app could not be moved aside; it stays" >&2
fi
if [ -d "$app" ]; then
	rm -rf "$stage"
fi
` + launch + `
`
}
