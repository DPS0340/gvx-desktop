package engine

// Owner: workstream C.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestNetworksBuiltinsCRUDAndWarningSeam(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeRunner()
	fake.addImage("alpine:3.20", ImageConfig{Cmd: []string{"/bin/sh"}})
	te := reopenTestEngine(t, dir, fake)

	// Builtins bridge, host, none always exist.
	st, body := te.do(t, http.MethodGet, "/networks", nil)
	if st != http.StatusOK {
		t.Fatalf("GET /networks = %d %s", st, body)
	}
	var nets []dockerNetwork
	if err := json.Unmarshal(body, &nets); err != nil {
		t.Fatal(err)
	}
	if len(nets) != 3 {
		t.Fatalf("expected 3 builtins, got %d", len(nets))
	}

	// Deleting a builtin → 403.
	if st, _ := te.do(t, http.MethodDelete, "/networks/bridge", nil); st != http.StatusForbidden {
		t.Fatalf("delete builtin = %d, want 403", st)
	}

	// Create custom network.
	st, body = te.do(t, http.MethodPost, "/networks/create", map[string]any{
		"Name":   "gvxd-net",
		"Driver": "bridge",
		"Labels": map[string]string{"com.docker.compose.project": "demo"},
	})
	if st != http.StatusCreated {
		t.Fatalf("create network = %d %s", st, body)
	}
	var created struct {
		ID      string `json:"Id"`
		Warning string `json:"Warning"`
	}
	_ = json.Unmarshal(body, &created)
	if len(created.ID) != 64 {
		t.Fatalf("expected 64-hex network ID, got %q", created.ID)
	}

	// Duplicate name → 409.
	if st, _ := te.do(t, http.MethodPost, "/networks/create", map[string]any{"Name": "gvxd-net"}); st != http.StatusConflict {
		t.Fatalf("duplicate create = %d, want 409", st)
	}

	// Inspect by prefix.
	st, body = te.do(t, http.MethodGet, "/networks/"+created.ID[:12], nil)
	if st != http.StatusOK {
		t.Fatalf("inspect by prefix = %d %s", st, body)
	}

	// Filter type=custom.
	q := url.QueryEscape(`{"type":{"custom":true}}`)
	_, body = te.do(t, http.MethodGet, "/networks?filters="+q, nil)
	nets = nil
	_ = json.Unmarshal(body, &nets)
	if len(nets) != 1 || nets[0].Name != "gvxd-net" {
		t.Fatalf("custom filter got %+v", nets)
	}

	// Connect two containers via seam: first has no warning, second produces warning.
	w1, err := te.connectNetwork("gvxd-net", strings.Repeat("1", 64), []string{"web"})
	if err != nil || w1 != "" {
		t.Fatalf("first connect = (%q, %v), want no warning", w1, err)
	}
	w2, err := te.connectNetwork("gvxd-net", strings.Repeat("2", 64), []string{"db"})
	if err != nil || !strings.Contains(w2, `Containers on the network "gvxd-net" cannot reach each other yet`) {
		t.Fatalf("second connect = (%q, %v), want inter-container warning", w2, err)
	}

	// Delete while containers are connected → 403.
	st, body = te.do(t, http.MethodDelete, "/networks/gvxd-net", nil)
	if st != http.StatusForbidden || !strings.Contains(string(body), "active endpoints") {
		t.Fatalf("delete active network = %d %s, want 403", st, body)
	}

	// Persistence across reopen.
	te.srv.Close()
	_ = te.Close(time.Second)
	te2 := reopenTestEngine(t, dir, fake)
	n, err := te2.network("gvxd-net")
	if err != nil || len(n.Containers) != 2 {
		t.Fatalf("network after reopen = %+v, %v", n, err)
	}

	// Disconnect both via seam, then prune removes gvxd-net and keeps builtins.
	te2.disconnectNetworks(strings.Repeat("1", 64))
	te2.disconnectNetworks(strings.Repeat("2", 64))

	st, body = te2.do(t, http.MethodPost, "/networks/prune", nil)
	if st != http.StatusOK {
		t.Fatalf("prune = %d %s", st, body)
	}
	var pr struct {
		NetworksDeleted []string `json:"NetworksDeleted"`
	}
	_ = json.Unmarshal(body, &pr)
	if len(pr.NetworksDeleted) != 1 || pr.NetworksDeleted[0] != "gvxd-net" {
		t.Fatalf("prune deleted %v, want [gvxd-net]", pr.NetworksDeleted)
	}
	if _, err := te2.network("bridge"); err != nil {
		t.Fatalf("bridge builtin missing after prune: %v", err)
	}
}
