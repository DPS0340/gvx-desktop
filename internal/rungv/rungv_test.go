package rungv

import (
	"reflect"
	"testing"
)

func TestSplitArgs(t *testing.T) {
	cases := map[string][]string{
		``:                        nil,
		`sh -c "echo hi; ls"`:     {"sh", "-c", "echo hi; ls"},
		`a 'b c' d\ e`:            {"a", "b c", "d e"},
		`--write-kubeconfig=/o/k`: {"--write-kubeconfig=/o/k"},
		`x ""`:                    {"x", ""},
		`  lead   trail  `:        {"lead", "trail"},
		`"it's" 'say "hi"'`:       {"it's", `say "hi"`},
	}
	for in, want := range cases {
		got, err := SplitArgs(in)
		if err != nil {
			t.Fatalf("SplitArgs(%q): %v", in, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("SplitArgs(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := SplitArgs(`"open`); err == nil {
		t.Error("unterminated quote accepted")
	}
}

func TestQuoteRoundTrip(t *testing.T) {
	args := []string{"run", "-e", "DOCKER_TLS_CERTDIR=", "sh", "-c", "echo 'a b'; ls", ""}
	got, err := SplitArgs(FormatArgs(args))
	if err != nil || !reflect.DeepEqual(got, args) {
		t.Fatalf("round trip = %q, %v", got, err)
	}
}

func TestClientArgsCacheDir(t *testing.T) {
	c := &Client{CacheDir: "/c"}
	if got := c.Args("volume", "ls", "--format", "json"); !reflect.DeepEqual(got, []string{"volume", "ls", "-cache-dir", "/c", "--format", "json"}) {
		t.Errorf("volume ls: %q", got)
	}
	if got := c.Args("run", "alpine"); !reflect.DeepEqual(got, []string{"run", "-cache-dir", "/c", "alpine"}) {
		t.Errorf("run: %q", got)
	}
	if got := c.Args("checkpoint", "-control-socket", "s"); !reflect.DeepEqual(got, []string{"checkpoint", "-control-socket", "s"}) {
		t.Errorf("checkpoint: %q", got)
	}
}

func TestParseVersion(t *testing.T) {
	v := ParseVersion("rungv v0.16.1\ncommit a9bfd06 (2026-11-16T03:50:26Z)\ngo go1.27.4 darwin/arm64 GOARM64=v8.0\n")
	if v.Program != "rungv" || v.Version != "v0.16.1" || v.Commit != "a9bfd06" || v.Go != "go1.27.4 darwin/arm64 GOARM64=v8.0" {
		t.Errorf("%+v", v)
	}
	// gvx v0.23 names the program rungvx and adds its build options.
	v = ParseVersion("rungvx v0.23.10022026112300\ncommit 52e9405 (2026-11-25T05:53:27Z)\ngo go1.27.4 darwin/arm64 GOARM64=v8.4\nbuild options: cgo=0 tags=none extensions=none\n")
	if v.Program != "rungvx" || v.Version != "v0.23.10022026112300" || v.Commit != "52e9405" || v.Go != "go1.27.4 darwin/arm64 GOARM64=v8.4" {
		t.Errorf("%+v", v)
	}
}

func TestCutProgram(t *testing.T) {
	for _, c := range []struct {
		line, sep, rest string
		ok              bool
	}{
		{"rungv: platform=hvf", ": ", "platform=hvf", true},
		{"rungvx: platform=hvf", ": ", "platform=hvf", true},
		{"rungv exec: NOT_FOUND: x", " ", "exec: NOT_FOUND: x", true},
		{"rungvx exec: NOT_FOUND: x", " ", "exec: NOT_FOUND: x", true},
		{"rungvx exec: NOT_FOUND: x", ": ", "", false},
		{"gvx: auto-published", ": ", "", false},
		{"rungvy: x", ": ", "", false},
	} {
		if rest, ok := CutProgram(c.line, c.sep); rest != c.rest || ok != c.ok {
			t.Errorf("CutProgram(%q, %q) = %q, %v", c.line, c.sep, rest, ok)
		}
	}
}

func TestDecodeLines(t *testing.T) {
	rows, err := decodeLines[Volume]([]byte(`{"Name":"a","InUse":true}` + "\n" + `{"Name":"b"}` + "\n"))
	if err != nil || len(rows) != 2 || rows[0].Name != "a" || !rows[0].InUse || rows[1].Name != "b" {
		t.Fatalf("%+v %v", rows, err)
	}
}

func TestDiagnose(t *testing.T) {
	if Diagnose("rungv run: listen tcp 127.0.0.1:8080: bind: address already in use", 1) == "" {
		t.Error("port conflict not diagnosed")
	}
	if Diagnose("something", 137) == "" {
		t.Error("137 not diagnosed")
	}
	if Diagnose("all good", 0) != "" {
		t.Error("diagnosed nothing")
	}
}

func TestImageRef(t *testing.T) {
	if r := (Image{Repository: "alpine", Tag: "3.20", ID: "x"}).Ref(); r != "alpine:3.20" {
		t.Error(r)
	}
	if r := (Image{Repository: "<none>", Tag: "<none>", ID: "abc"}).Ref(); r != "abc" {
		t.Error(r)
	}
}
