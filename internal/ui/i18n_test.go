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

	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// sourceKeys returns every text the code hands tr or T as a literal, with
// where it is.
func sourceKeys(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, filepath.Join("..", "..", "main.go"))
	keys := map[string]string{}
	fset := token.NewFileSet()
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
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				if fn.Name != "tr" {
					return true
				}
			case *ast.SelectorExpr:
				if pkg, ok := fn.X.(*ast.Ident); !ok || pkg.Name != "ui" || fn.Sel.Name != "T" {
					return true
				}
			default:
				return true
			}
			if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if key, err := strconv.Unquote(lit.Value); err == nil {
					keys[key] = fset.Position(lit.Pos()).String()
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

// Every text of the app has a Korean one that takes the same arguments and
// joins what is around it the same way.
func TestKoreanCatalogIsComplete(t *testing.T) {
	keys := sourceKeys(t)
	// The texts that reach tr in a variable.
	for _, s := range []sandbox.Status{sandbox.Starting, sandbox.Running, sandbox.Stopping, sandbox.Exited, sandbox.Failed, -1} {
		keys[s.String()] = "sandbox.Status"
	}
	for _, r := range recipes.All() {
		keys[r.Summary] = "recipe " + r.Key
		for _, h := range r.Highlights {
			keys[h] = "recipe " + r.Key
		}
	}
	for _, k := range []string{"System", "Light", "Dark"} {
		keys[k] = "a theme"
	}
	for key, at := range keys {
		ko, ok := korean[key]
		if !ok {
			t.Errorf("%s: no Korean for %q", at, key)
			continue
		}
		if a, b := verbs(key), verbs(ko); !slices.Equal(a, b) {
			t.Errorf("%s: %q takes %v, its Korean %q takes %v", at, key, a, ko, b)
		}
		kl, kt := edgeSpace(key)
		if l, tr := edgeSpace(ko); l != kl || tr != kt {
			t.Errorf("%s: the Korean of %q does not keep its leading and trailing spaces", at, key)
		}
	}
}

func TestTranslateFollowsTheLanguage(t *testing.T) {
	t.Cleanup(func() { SetLanguage(LangEnglish) })
	SetLanguage(LangKorean)
	if got := tr("Delete %s?", "web"); got != "web을(를) 삭제할까요?" {
		t.Errorf("Korean: %q", got)
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
}

func TestResolveLanguage(t *testing.T) {
	if got := ResolveLanguage(LangKorean); got != LangKorean {
		t.Errorf("ko: %q", got)
	}
	if got := ResolveLanguage(LangEnglish); got != LangEnglish {
		t.Errorf("en: %q", got)
	}
	if got := ResolveLanguage(""); got != LangEnglish && got != LangKorean {
		t.Errorf("system: %q", got)
	}
}
