package engine

// Owner: frozen (architect).

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMatchSegments(t *testing.T) {
	split := func(s string) []string { return strings.Split(strings.Trim(s, "/"), "/") }
	cases := []struct {
		pattern, path string
		ok            bool
		vars          map[string]string
	}{
		{"/containers/{id}/json", "/containers/abc/json", true, map[string]string{"id": "abc"}},
		{"/containers/{id}/json", "/containers/json", false, nil},
		{"/containers/{id}", "/containers/abc", true, map[string]string{"id": "abc"}},
		{"/images/{name...}/json", "/images/ghcr.io/org/app:v1/json", true, map[string]string{"name": "ghcr.io/org/app:v1"}},
		{"/images/{name...}/json", "/images/json", false, nil},
		{"/images/{name...}", "/images/library/alpine:3.20", true, map[string]string{"name": "library/alpine:3.20"}},
		{"/_ping", "/_ping", true, nil},
		{"/_ping", "/_ping/x", false, nil},
	}
	for _, c := range cases {
		vars, ok := matchSegments(split(c.pattern), split(c.path))
		if ok != c.ok {
			t.Errorf("%s ~ %s: ok %v, want %v", c.pattern, c.path, ok, c.ok)
			continue
		}
		for k, v := range c.vars {
			if vars[k] != v {
				t.Errorf("%s ~ %s: %s = %q, want %q", c.pattern, c.path, k, vars[k], v)
			}
		}
	}
}

func TestStripVersion(t *testing.T) {
	for in, want := range map[string]string{
		"/v1.52/containers/json": "/containers/json",
		"/v1.24/_ping":           "/_ping",
		"/v1.57":                 "/",
		"/containers/json":       "/containers/json",
		"/vx.1/containers":       "/vx.1/containers",
		"/volumes":               "/volumes",
	} {
		if got := stripVersion(in); got != want {
			t.Errorf("stripVersion(%q) = %q, want %q", in, got, want)
		}
	}
	if !versionAtLeast("1.52", "1.42") || versionAtLeast("1.41", "1.42") || !versionAtLeast("1.100", "1.52") {
		t.Error("versionAtLeast compares wrongly")
	}
}

func TestUnsupportedEndpoints(t *testing.T) {
	te := newTestEngine(t)
	for _, c := range []struct{ method, path, want string }{
		{"POST", "/v1.52/containers/abc/exec", notSupportedMessage("exec")},
		{"POST", "/v1.52/exec/abc/start", notSupportedMessage("exec")},
		{"POST", "/v1.52/grpc", buildDriverNotSupportedMessage()},
		{"POST", "/session", buildDriverNotSupportedMessage()},
		{"GET", "/v1.52/swarm", notSupportedMessage("Swarm")},
		{"GET", "/v1.52/plugins/x/json", notSupportedMessage("Plugins")},
	} {
		status, body := te.do(t, c.method, c.path, nil)
		var e struct{ Message string }
		_ = json.Unmarshal(body, &e)
		if status != http.StatusNotImplemented || e.Message != c.want {
			t.Errorf("%s %s: %d %q, want %q", c.method, c.path, status, body, c.want)
		}
	}
	if status, _ := te.do(t, "GET", "/v1.52/nothing/here", nil); status != http.StatusNotFound {
		t.Errorf("an unknown path answered %d", status)
	}
}

func TestFilters(t *testing.T) {
	for _, raw := range []string{
		`{"label":{"com.docker.compose.project=demo":true,"x":true}}`,
		`{"label":["com.docker.compose.project=demo","x"]}`,
	} {
		f, err := parseFilters(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !f.matchLabels(map[string]string{"com.docker.compose.project": "demo", "x": ""}) {
			t.Errorf("%s: labels that match did not", raw)
		}
		if f.matchLabels(map[string]string{"com.docker.compose.project": "other", "x": ""}) {
			t.Errorf("%s: labels that do not match did", raw)
		}
	}
}
