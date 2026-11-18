package rungv

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// The source of gvx: where it is cloned from, the branch built, and the
// API that tells of it; a test points the last at a server of its own.
const (
	SourceURL    = "https://github.com/gfire-sigs/gvx"
	SourceBranch = "main"
)

var sourceAPI = "https://api.github.com/repos/gfire-sigs/gvx"

// Head is the newest commit of the source's branch.
type Head struct {
	SHA     string
	Subject string // the first line of its message
	// Ahead is how many commits the branch is past the one asked about,
	// or -1 when GitHub could not say, as of a commit it does not know.
	Ahead int
}

// SourceHead asks GitHub for the head of the branch, and how far it is
// past installed, the commit of the rungv in use; installed may be empty.
func SourceHead(ctx context.Context, installed string) (Head, error) {
	var commit struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	if err := getJSON(ctx, sourceAPI+"/commits/"+SourceBranch, &commit); err != nil {
		return Head{}, err
	}
	if commit.SHA == "" {
		return Head{}, errors.New("the branch has no head")
	}
	h := Head{SHA: commit.SHA, Ahead: -1}
	h.Subject, _, _ = strings.Cut(commit.Commit.Message, "\n")
	switch {
	case installed == commit.SHA:
		h.Ahead = 0
	case installed != "":
		var cmp struct {
			Ahead int `json:"ahead_by"`
		}
		if err := getJSON(ctx, sourceAPI+"/compare/"+installed+"..."+SourceBranch, &cmp); err == nil {
			h.Ahead = cmp.Ahead
		}
	}
	return h, nil
}

func getJSON(ctx context.Context, url string, v any) error {
	resp, err := get(ctx, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(v)
}

// ErrNoToolchain reports that git or Go, which a build from source needs,
// was not found.
var ErrNoToolchain = errors.New("building from source needs git and Go")

// FindTool looks for a command where a terminal would find it. An app
// started from the Dock or a launcher has a short PATH, without the
// directories a shell's profile adds, so the usual homes of developer
// tools are looked in too, and at last the login shell is asked.
func FindTool(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "go", "bin"), filepath.Join(home, ".local", "bin"),
			filepath.Join(home, ".nix-profile", "bin"), filepath.Join(home, "sdk", "go", "bin"))
	}
	if runtime.GOOS == "windows" {
		dirs = append(dirs, `C:\Program Files\Go\bin`, `C:\Program Files\Git\cmd`)
	} else {
		dirs = append(dirs, "/opt/homebrew/bin", "/usr/local/bin", "/usr/local/go/bin",
			"/run/current-system/sw/bin", "/nix/var/nix/profiles/default/bin", "/snap/bin")
	}
	for _, d := range dirs {
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	if runtime.GOOS != "windows" {
		if shell := os.Getenv("SHELL"); shell != "" {
			out, err := exec.Command(shell, "-lc", "command -v "+name).Output()
			if p := strings.TrimSpace(lastLine(string(out))); err == nil && filepath.IsAbs(p) {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("%w: %s was not found", ErrNoToolchain, name)
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// BuildFromSource builds rungv from the head of the source's branch and
// puts it at target, replacing what is there. dir is a directory of the
// caller's to keep the clone in between builds: it is fetched into, and
// whatever else is in it is thrown away. phase hears what the build is
// about to do, and line each line git and Go print. It returns the commit
// built. On macOS the caller still signs the binary, with Sign.
func BuildFromSource(ctx context.Context, dir, target string, phase func(Phase), line func(string)) (string, error) {
	git, err := FindTool("git")
	if err != nil {
		return "", err
	}
	goBin, err := FindTool("go")
	if err != nil {
		return "", err
	}
	run := func(in string, env []string, name string, args ...string) error {
		line("$ " + filepath.Base(name) + " " + strings.Join(args, " "))
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = in
		cmd.Env = append(os.Environ(), env...)
		hideWindow(cmd)
		out, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		cmd.Stderr = cmd.Stdout
		if err := cmd.Start(); err != nil {
			return err
		}
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			// git redraws its progress in place with carriage returns.
			text := sc.Text()
			if i := strings.LastIndexByte(strings.TrimRight(text, "\r"), '\r'); i >= 0 {
				text = text[i+1:]
			}
			line(strings.TrimRight(text, "\r"))
		}
		if err := cmd.Wait(); err != nil {
			return fmt.Errorf("%s %s: %w", filepath.Base(name), args[0], err)
		}
		return nil
	}

	phase(PhaseFetch)
	head := "FETCH_HEAD"
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		// Not a clone, or a broken one: start over. The whole history comes
		// along, since Go names the build after the tags it finds in it.
		if err := os.RemoveAll(dir); err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", err
		}
		if err := run(filepath.Dir(dir), nil, git, "clone", "--progress", "--branch", SourceBranch, SourceURL, dir); err != nil {
			return "", err
		}
		head = "origin/" + SourceBranch
	} else if err := run(dir, nil, git, "fetch", "--progress", "--tags", "origin", SourceBranch); err != nil {
		return "", err
	}
	// The clone is this function's own, so that nothing in it is worth
	// keeping: a tree with changes would also be built as +dirty.
	if err := run(dir, nil, git, "checkout", "--force", "--detach", head); err != nil {
		return "", err
	}
	if err := run(dir, nil, git, "clean", "-fdx"); err != nil {
		return "", err
	}
	sha, err := exec.CommandContext(ctx, git, "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(string(sha))

	phase(PhaseBuild)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	fresh := filepath.Join(filepath.Dir(target), ".rungv-built-"+commit[:min(12, len(commit))])
	defer os.Remove(fresh)
	// Go and git are on the PATH of the build whatever this process's is.
	path := "PATH=" + filepath.Dir(goBin) + string(os.PathListSeparator) + filepath.Dir(git) + string(os.PathListSeparator) + os.Getenv("PATH")
	script := filepath.Join(dir, "scripts", "repro-build.sh")
	if _, serr := os.Stat(script); serr == nil && runtime.GOOS != "windows" {
		// gvx's own way to build, which pins what makes a build repeatable.
		err = run(dir, []string{path, "GO=" + goBin}, "/bin/sh", script, fresh, "./cmd/rungv")
	} else {
		err = run(dir, []string{path, "CGO_ENABLED=0", "GOWORK=off", "GOFLAGS="}, goBin, "build",
			"-mod=readonly", "-trimpath", "-buildvcs=true", "-ldflags=-s -w -buildid=", "-o", fresh, "./cmd/rungv")
	}
	if err != nil {
		return "", err
	}
	if err := os.Chmod(fresh, 0o755); err != nil {
		return "", err
	}
	phase(PhaseInstall)
	return commit, replaceFile(fresh, target)
}

// Phase is a step of BuildFromSource.
type Phase int

const (
	PhaseFetch Phase = iota + 1
	PhaseBuild
	PhaseInstall
)
