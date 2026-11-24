package engine

// Owner: workstream C.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestPingHeadersAndHEAD(t *testing.T) {
	te := newTestEngine(t)

	// GET /_ping
	req, err := http.NewRequest(http.MethodGet, te.srv.URL+"/_ping", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := te.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /_ping status = %d, want 200", res.StatusCode)
	}
	for k, want := range map[string]string{
		"Api-Version":         APIVersion,
		"Ostype":              "linux",
		"Docker-Experimental": "false",
		"Builder-Version":     "1",
		"Swarm":               "inactive",
		"Cache-Control":       "no-cache, no-store, must-revalidate",
		"Pragma":              "no-cache",
		"Content-Type":        "text/plain; charset=utf-8",
	} {
		if got := res.Header.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	status, body := te.do(t, http.MethodGet, "/v1.52/_ping", nil)
	if status != http.StatusOK || string(body) != "OK" {
		t.Fatalf("GET /v1.52/_ping = %d %q, want 200 OK", status, body)
	}

	// HEAD /_ping
	hstatus, hbody := te.do(t, http.MethodHead, "/_ping", nil)
	if hstatus != http.StatusOK || len(hbody) != 0 {
		t.Fatalf("HEAD /_ping = %d %q, want 200 empty body", hstatus, hbody)
	}
}

func TestVersionInfoAndSystemDF(t *testing.T) {
	te := newTestEngine(t)
	te.fake.addImage("alpine:3.20", ImageConfig{Cmd: []string{"/bin/sh"}})

	// /version
	status, body := te.do(t, http.MethodGet, "/v1.52/version", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /version = %d %s", status, body)
	}
	var ver struct {
		Platform struct {
			Name string `json:"Name"`
		} `json:"Platform"`
		Components []struct {
			Name    string            `json:"Name"`
			Version string            `json:"Version"`
			Details map[string]string `json:"Details"`
		} `json:"Components"`
		Version       string `json:"Version"`
		APIVersion    string `json:"ApiVersion"`
		MinAPIVersion string `json:"MinAPIVersion"`
		Os            string `json:"Os"`
		Arch          string `json:"Arch"`
		KernelVersion string `json:"KernelVersion"`
		Experimental  bool   `json:"Experimental"`
	}
	if err := json.Unmarshal(body, &ver); err != nil {
		t.Fatal(err)
	}
	if ver.Version != "gvx-desktop-0.0.0-test" || ver.APIVersion != APIVersion || ver.MinAPIVersion != MinAPIVersion {
		t.Errorf("unexpected version: %+v", ver)
	}
	if ver.Os != "linux" || ver.KernelVersion != "gvx" || !ver.Experimental {
		t.Errorf("unexpected platform fields: %+v", ver)
	}
	if len(ver.Components) < 2 || ver.Components[0].Name != "Engine" || ver.Components[1].Name != "rungv" {
		t.Errorf("unexpected components: %+v", ver.Components)
	}

	// Create a volume and a container so /info and /system/df reflect them.
	if st, b := te.do(t, http.MethodPost, "/volumes/create", map[string]any{"Name": "gvxd-dfvol"}); st != http.StatusCreated {
		t.Fatalf("volume create = %d %s", st, b)
	}
	if st, b := te.do(t, http.MethodPost, "/containers/create?name=gvxd-dfctr", map[string]any{
		"Image": "alpine:3.20",
		"HostConfig": map[string]any{
			"Binds": []string{"gvxd-dfvol:/data"},
		},
	}); st != http.StatusCreated {
		t.Fatalf("container create = %d %s", st, b)
	}

	// /info
	status, body = te.do(t, http.MethodGet, "/v1.52/info", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /info = %d %s", status, body)
	}
	var info struct {
		ID                string              `json:"ID"`
		ServerVersion     string              `json:"ServerVersion"`
		Containers        int                 `json:"Containers"`
		ContainersRunning int                 `json:"ContainersRunning"`
		ContainersStopped int                 `json:"ContainersStopped"`
		Images            int                 `json:"Images"`
		Driver            string              `json:"Driver"`
		OSType            string              `json:"OSType"`
		DockerRootDir     string              `json:"DockerRootDir"`
		Plugins           map[string][]string `json:"Plugins"`
		Warnings          []string            `json:"Warnings"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatal(err)
	}
	if info.Driver != "gvx" || info.OSType != "linux" || info.ServerVersion != "gvx-desktop-0.0.0-test" {
		t.Errorf("unexpected info: %+v", info)
	}
	if info.Containers != 1 || info.ContainersStopped != 1 || info.Images != 1 {
		t.Errorf("counts wrong: %+v", info)
	}
	if info.ID == "" || len(info.Warnings) == 0 {
		t.Errorf("missing ID or Warnings: %+v", info)
	}
	// Stable ID across calls.
	_, body2 := te.do(t, http.MethodGet, "/info", nil)
	var info2 struct{ ID string }
	_ = json.Unmarshal(body2, &info2)
	if info2.ID != info.ID {
		t.Errorf("info ID not stable: %q vs %q", info.ID, info2.ID)
	}

	// /system/df
	status, body = te.do(t, http.MethodGet, "/v1.52/system/df", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /system/df = %d %s", status, body)
	}
	var df struct {
		LayersSize int64 `json:"LayersSize"`
		Images     []struct {
			ID         string `json:"Id"`
			Containers int64  `json:"Containers"`
		} `json:"Images"`
		Containers []struct {
			ID     string `json:"Id"`
			SizeRw int64  `json:"SizeRw"`
		} `json:"Containers"`
		Volumes []struct {
			Name      string `json:"Name"`
			UsageData struct {
				Size     int64 `json:"Size"`
				RefCount int64 `json:"RefCount"`
			} `json:"UsageData"`
		} `json:"Volumes"`
		BuildCache []any `json:"BuildCache"`
	}
	if err := json.Unmarshal(body, &df); err != nil {
		t.Fatal(err)
	}
	if df.LayersSize != 1<<20 || len(df.Images) != 1 || df.Images[0].Containers != 1 {
		t.Errorf("unexpected df images: %+v", df)
	}
	if len(df.Containers) != 1 || df.Containers[0].SizeRw != 0 {
		t.Errorf("unexpected df containers: %+v", df.Containers)
	}
	if len(df.Volumes) != 1 || df.Volumes[0].Name != "gvxd-dfvol" || df.Volumes[0].UsageData.RefCount != 1 {
		t.Errorf("unexpected df volumes: %+v", df.Volumes)
	}
}

func TestUnsupportedTableFull(t *testing.T) {
	te := newTestEngine(t)
	cases := []struct{ method, path, substr string }{
		{"POST", "/auth", "Logging in"},
		{"GET", "/containers/abc/top", "processes"},
		{"GET", "/containers/abc/changes", "changes"},
		{"GET", "/containers/abc/export", "Exporting"},
		{"POST", "/containers/abc/update", "Updating"},
		{"POST", "/containers/abc/pause", "Pausing"},
		{"POST", "/containers/abc/unpause", "Pausing"},
		{"GET", "/containers/abc/archive", "Copying"},
		{"PUT", "/containers/abc/archive", "Copying"},
		{"POST", "/commit", "Committing"},
		{"POST", "/images/alpine:3.20/push", "Pushing"},
		{"GET", "/distribution/alpine:3.20/json", "manifest"},
		{"PUT", "/volumes/foo", "cluster volume"},
		{"GET", "/nodes", "Swarm"},
		{"GET", "/services", "Swarm"},
		{"GET", "/tasks", "Swarm"},
		{"GET", "/secrets", "Secrets"},
		{"GET", "/configs", "Configs"},
	}
	for _, c := range cases {
		st, body := te.do(t, c.method, "/v1.52"+c.path, nil)
		if st != http.StatusNotImplemented || !strings.Contains(string(body), c.substr) ||
			!strings.Contains(string(body), "not supported by GVX Desktop's experimental sandbox engine yet") {
			t.Errorf("%s %s: %d %s", c.method, c.path, st, body)
		}
	}
}

func TestParseHumanSize(t *testing.T) {
	cases := map[string]int64{
		"0B":           0,
		"512B":         512,
		"1kB":          1000,
		"1KiB":         1024,
		"9.155MB":      9155000,
		"2MiB":         2 * 1024 * 1024,
		"1.5GB":        1500000000,
		"12.5MB (50%)": 12500000,
	}
	for in, want := range cases {
		if got := parseHumanSize(in); got != want {
			t.Errorf("parseHumanSize(%q) = %d, want %d", in, got, want)
		}
	}
}
