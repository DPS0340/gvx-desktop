package ui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// A key is a text the code hands tr, T or trn as a literal.
type key struct {
	at     string // where the code has it
	plural bool   // trn's: the Korean is a plural, under the singular
	bare   bool   // some call passes no arguments, formatting it later
}

// sourceKeys returns every text the code hands tr, T or trn as a literal.
func sourceKeys(t *testing.T) map[string]key {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, filepath.Join("..", "..", "main.go"))
	keys := map[string]key{}
	fset := token.NewFileSet()
	literal := func(e ast.Expr) (string, bool) {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(lit.Value)
		return s, err == nil
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			fn := ""
			switch f := call.Fun.(type) {
			case *ast.Ident:
				fn = f.Name
			case *ast.SelectorExpr:
				if pkg, ok := f.X.(*ast.Ident); ok && pkg.Name == "ui" {
					switch f.Sel.Name {
					case "T":
						fn = "T"
					case "TN":
						fn = "trn"
					}
				}
			}
			switch fn {
			case "tr", "T":
				if s, ok := literal(call.Args[0]); ok {
					k := keys[s]
					k.at = fset.Position(call.Args[0].Pos()).String()
					k.bare = k.bare || len(call.Args) == 1
					keys[s] = k
				}
			case "trn":
				if len(call.Args) < 3 {
					return true
				}
				one, ok1 := literal(call.Args[1])
				other, ok2 := literal(call.Args[2])
				if ok1 && ok2 {
					if a, b := verbs(one), verbs(other); !slices.Equal(a, b) {
						t.Errorf("%s: %q takes %v, its plural %q takes %v", fset.Position(call.Pos()), one, a, other, b)
					}
					keys[one] = key{at: fset.Position(call.Args[1].Pos()).String(), plural: true}
				}
			}
			return true
		})
	}
	if len(keys) < 100 {
		t.Fatalf("found %d texts in the source, which is too few to be all of them", len(keys))
	}
	return keys
}

var formatVerb = regexp.MustCompile(`%(\[\d+\])?[a-zA-Z]`)

// verbs lists the verbs of a format in the order of their arguments, so a
// translation that reorders them with %[2]s compares equal.
func verbs(format string) []string {
	var out []string
	for i, m := range formatVerb.FindAllStringSubmatch(format, -1) {
		arg := i + 1
		if m[1] != "" {
			arg, _ = strconv.Atoi(m[1][1 : len(m[1])-1])
		}
		out = append(out, strconv.Itoa(arg)+m[0][len(m[0])-1:])
	}
	slices.Sort(out)
	return out
}

func edgeSpace(s string) (lead, trail string) {
	return s[:len(s)-len(strings.TrimLeft(s, " "))], s[len(strings.TrimRight(s, " ")):]
}

// particlePair matches a Korean particle choice, "{을/를}".
var particlePair = regexp.MustCompile(`\{[^{}/%\s]+/[^{}/%\s]+\}`)

// appKeys is every text the app words: those tr and trn are given as
// literals, and those that reach them in a variable.
func appKeys(t *testing.T) map[string]key {
	t.Helper()
	keys := sourceKeys(t)
	for _, s := range []sandbox.Status{sandbox.Starting, sandbox.Running, sandbox.Stopping, sandbox.Exited, sandbox.Failed, -1} {
		keys[s.String()] = key{at: "sandbox.Status"}
	}
	for _, r := range recipes.All() {
		keys[r.Summary] = key{at: "recipe " + r.Key}
		for _, h := range r.Highlights {
			keys[h] = key{at: "recipe " + r.Key}
		}
	}
	for _, k := range []string{"System", "Light", "Dark"} {
		keys[k] = key{at: "a theme"}
	}
	for _, k := range []string{
		"Software Update",
		"Application",
		"Check whether a newer version of %s is available.",
		"Check for Updates",
		"Checking for updates…",
		"Looking for the latest version of %s.",
		"%s is currently the newest version available.",
		"%s %s is currently the newest version available.",
		"You're up to date!",
		"OK",
		"A new version of %s is available!",
		"Critical",
		"%s %s is now available — you have %s.",
		"%s %s is now available.",
		"No release notes were provided for this version.",
		"View full release notes",
		"Automatically check for updates",
		"Skip This Version",
		"Remind Me Later",
		"Open release page",
		"Install Update",
		"Downloading %s…",
		"Downloading %s %s…",
		"Ready to Install",
		"Verified",
		"%s %s has been downloaded and verified. Click below to install and relaunch.",
		"%s %s has been downloaded. Click below to install and relaunch.",
		"The update for %s has been downloaded and verified. Click below to install and relaunch.",
		"The update for %s has been downloaded. Click below to install and relaunch.",
		"Later",
		"Install and Relaunch",
		"Installing update…",
		"%s will restart shortly.",
		"An unexpected error occurred while updating.",
		"Update Failed",
		"Starting download…",
		"Signature verified.",
		"Download complete.",
		"Verifying signature…",
		"Downloaded %s",
		"Downloading…",
	} {
		keys[k] = key{at: "ggui/updater"}
	}
	return keys
}

// Every text of the app has one in each language, which takes the same
// arguments and joins what is around it the same way. In Korean, particle
// choices are where the formatter makes them. Run one language with
// -run 'TestCatalogsAreComplete/ja'.
func TestCatalogsAreComplete(t *testing.T) {
	keys := appKeys(t)
	for _, lang := range languages {
		if lang.catalog == nil {
			continue
		}
		t.Run(lang.tag, func(t *testing.T) {
			if lang.catalog.Lang != lang.tag {
				t.Errorf("the catalog says it is %q", lang.catalog.Lang)
			}
			if lang.strings.Close == "" {
				t.Errorf("no words for ggui's controls")
			}
			for text, k := range keys {
				var words []string
				if k.plural {
					forms, ok := lang.catalog.Plurals[text]
					if !ok || forms.Other == "" {
						t.Errorf("%s: no plural for %q", k.at, text)
						continue
					}
					words = []string{forms.Other}
				} else {
					s, ok := lang.catalog.Messages[text]
					if !ok || s == "" {
						t.Errorf("%s: no text for %q", k.at, text)
						continue
					}
					words = []string{s}
				}
				for _, w := range words {
					if a, b := verbs(text), verbs(w); !slices.Equal(a, b) {
						t.Errorf("%s: %q takes %v, its translation %q takes %v", k.at, text, a, w, b)
					}
					kl, kt := edgeSpace(text)
					if l, tr := edgeSpace(w); l != kl || tr != kt {
						t.Errorf("%s: the translation of %q does not keep its leading and trailing spaces", k.at, text)
					}
					if lang.tag != LangKorean {
						continue
					}
					for _, at := range particlePair.FindAllStringIndex(w, -1) {
						if !endsInVerb(w[:at[0]]) {
							t.Errorf("%s: the Korean of %q has %s after no format verb, where it shows as written", k.at, text, w[at[0]:at[1]])
						}
						if k.bare {
							t.Errorf("%s: %q is formatted after tr, which leaves %s in its Korean as written: pass tr the arguments", k.at, text, w[at[0]:at[1]])
						}
					}
				}
			}
		})
	}
}

// endsInVerb reports whether s ends in a format verb.
func endsInVerb(s string) bool {
	locs := formatVerb.FindAllStringIndex(s, -1)
	return len(locs) > 0 && locs[len(locs)-1][1] == len(s)
}

func TestTranslateFollowsTheLanguage(t *testing.T) {
	t.Cleanup(func() { SetLanguage(LangEnglish) })
	SetLanguage(LangKorean)
	// The particle fits the word: a Latin name takes the form after a
	// vowel, and a number the one Korean reads it with.
	for _, c := range []struct{ got, want string }{
		{tr("Delete %s?", "web"), "web를 삭제할까요?"},
		{tr("Delete %s?", "postgres:16"), "postgres:16을 삭제할까요?"},
		{tr("Exited with code %d", 1), "코드 1로 종료됨"},
		{tr("Exited with code %d", 3), "코드 3으로 종료됨"},
		{tr("The bundle %s (%s) is removed from disk.", "web-10", "1 GB"), "번들 web-10이 디스크에서 삭제됩니다(1 GB)."},
	} {
		if c.got != c.want {
			t.Errorf("Korean: %q, want %q", c.got, c.want)
		}
	}
	if got := quitText(2); got != "샌드박스 2개가 실행 중입니다. gvx에는 데몬이 없어서 샌드박스는 GVX Desktop이 실행되는 동안에만 유지됩니다." {
		t.Errorf("a plural: %q", got)
	}
	if got := tr("%d of %d sandboxes running", 1, 4); got != "샌드박스 4개 중 1개 실행 중" {
		t.Errorf("reordered arguments: %q", got)
	}
	if got := tr("a text with no translation"); got != "a text with no translation" {
		t.Errorf("a missing text: %q", got)
	}
	SetLanguage(LangEnglish)
	if got := tr("Delete %s?", "web"); got != "Delete web?" {
		t.Errorf("English: %q", got)
	}
	if got := quitText(1); !strings.HasPrefix(got, "1 sandbox is running.") {
		t.Errorf("one: %q", got)
	}
	if got := quitText(2); !strings.HasPrefix(got, "2 sandboxes are running.") {
		t.Errorf("two: %q", got)
	}
}

// Choosing a language in the settings words the window in it at the next
// frame, and the text tr gives anywhere else too.
func TestTheWindowFollowsTheLanguageSetting(t *testing.T) {
	t.Cleanup(func() { SetLanguage(LangEnglish) })
	dir := t.TempDir()
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return Build(m) }, ggui.Sz(1100, 760))
	p.Setup(func() {
		m = NewModel(store.Store{Dir: dir}, dir)
		m.Offline = true
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()
	shows := func(lang string, want, not string) {
		t.Helper()
		m.UpdateSettings(func(s *store.Settings) { s.Language = lang })
		p.Frame()
		if _, ok := p.Find(want); !ok {
			t.Errorf("%s: no %q in the window", lang, want)
		}
		if _, ok := p.Find(not); ok {
			t.Errorf("%s: %q is still in the window", lang, not)
		}
		if got := tr("Settings"); got != want {
			t.Errorf("%s: tr says %q", lang, got)
		}
	}
	shows(LangKorean, "설정", "Settings")
	shows(LangEnglish, "Settings", "설정")
	shows(LangKorean, "설정", "Settings")
}

func TestResolveLanguage(t *testing.T) {
	if got := ResolveLanguage(LangKorean); got != LangKorean {
		t.Errorf("ko: %q", got)
	}
	if got := ResolveLanguage(LangEnglish); got != LangEnglish {
		t.Errorf("en: %q", got)
	}
	if got := ResolveLanguage(""); !slices.Contains(LanguageTags(), got) {
		t.Errorf("system: %q", got)
	}
}
