package rungv

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourceNormalization(t *testing.T) {
	for _, tc := range []struct {
		src       Source
		cloneURL  string
		branch    string
		slug      string
		isGitHub  bool
		isDefault bool
	}{
		{Source{}, SourceURL, SourceBranch, "gfire-sigs/gvx", true, true},
		{Source{Repo: "gfire-sigs/gvx", Branch: "main"}, SourceURL, "main", "gfire-sigs/gvx", true, true},
		{Source{Repo: "ironpark/gvx", Branch: "feat/test"}, "https://github.com/ironpark/gvx", "feat/test", "ironpark/gvx", true, false},
		{Source{Repo: "https://github.com/ironpark/gvx.git", Branch: "dev"}, "https://github.com/ironpark/gvx.git", "dev", "ironpark/gvx", true, false},
		{Source{Repo: "git@github.com:ironpark/gvx.git", Branch: "dev"}, "git@github.com:ironpark/gvx.git", "dev", "ironpark/gvx", true, false},
		{Source{Repo: "https://gitlab.com/org/gvx.git", Branch: "v2"}, "https://gitlab.com/org/gvx.git", "v2", "", false, false},
		{Source{Repo: "/tmp/local-gvx", Branch: "wip"}, "/tmp/local-gvx", "wip", "", false, false},
	} {
		if got := tc.src.CloneURL(); got != tc.cloneURL {
			t.Errorf("%+v CloneURL = %q, want %q", tc.src, got, tc.cloneURL)
		}
		if got := tc.src.BranchName(); got != tc.branch {
			t.Errorf("%+v BranchName = %q, want %q", tc.src, got, tc.branch)
		}
		slug, ok := tc.src.GitHubSlug()
		if ok != tc.isGitHub || slug != tc.slug {
			t.Errorf("%+v GitHubSlug = (%q, %v), want (%q, %v)", tc.src, slug, ok, tc.slug, tc.isGitHub)
		}
		if got := tc.src.IsDefault(); got != tc.isDefault {
			t.Errorf("%+v IsDefault = %v, want %v", tc.src, got, tc.isDefault)
		}
	}
}

func TestSourceHeadSaysHowFarBranchIsPast(t *testing.T) {
	const head = "e410633f24cf34fddfde473e48dde172593dc5af"
	compared := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/gfire-sigs/gvx/commits/main":
			w.Write([]byte(`{"sha":"` + head + `","commit":{"message":"fix: a thing\n\nand why"}}`))
		case "/repos/gfire-sigs/gvx/compare/aaaa...main":
			compared++
			w.Write([]byte(`{"ahead_by":3}`))
		case "/repos/ironpark/gvx/commits/feat/test":
			w.Write([]byte(`{"sha":"` + head + `","commit":{"message":"feat: branch test"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := sourceAPIBase
	sourceAPIBase = srv.URL
	defer func() { sourceAPIBase = old }()

	for _, c := range []struct {
		installed string
		ahead     int
	}{{"", -1}, {head, 0}, {"aaaa", 3}, {"unknown", -1}} {
		h, err := SourceHead(context.Background(), Source{}, c.installed)
		if err != nil {
			t.Fatal(err)
		}
		if h.SHA != head || h.Subject != "fix: a thing" || h.Ahead != c.ahead {
			t.Errorf("installed %q: %+v, want ahead %d", c.installed, h, c.ahead)
		}
	}
	if compared != 1 {
		t.Errorf("compared %d times, want once: the head itself needs no comparing", compared)
	}

	h, err := SourceHead(context.Background(), Source{Repo: "ironpark/gvx", Branch: "feat/test"}, head)
	if err != nil {
		t.Fatal(err)
	}
	if h.SHA != head || h.Subject != "feat: branch test" || h.Ahead != 0 {
		t.Errorf("custom repo/branch: %+v", h)
	}
}

func TestSourceHeadLocalGitRepo(t *testing.T) {
	git, err := FindTool("git")
	if err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-b", "test-branch")
	if err := os.WriteFile(filepath.Join(repo, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README")
	run("commit", "-m", "init")
	want := run("rev-parse", "HEAD")

	h, err := SourceHead(context.Background(), Source{Repo: repo, Branch: "test-branch"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if h.SHA != want || h.Ahead != -1 {
		t.Errorf("got %+v, want SHA %s Ahead -1", h, want)
	}
	h2, err := SourceHead(context.Background(), Source{Repo: repo, Branch: "test-branch"}, want)
	if err != nil || h2.Ahead != 0 {
		t.Errorf("same commit: %+v, %v", h2, err)
	}
}
