package dockerapi

import (
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// engine serves handler as a dockerd on TCP would, and connects to it.
func engine(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// frame is a run of bytes of a multiplexed log stream behind its header.
func frame(stream byte, s string) []byte {
	h := make([]byte, 8, 8+len(s))
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	return append(h, s...)
}

func TestParseHost(t *testing.T) {
	unixFromSlash := func(s string) string { return s }
	winFromSlash := func(s string) string { return strings.ReplaceAll(s, "/", `\`) }

	for _, tc := range []struct {
		name      string
		host      string
		fromSlash func(string) string
		wantNet   string
		wantAddr  string
		wantBase  string
		wantErr   bool
	}{
		{
			name:      "unix posix socket",
			host:      "unix:///var/run/docker.sock",
			fromSlash: unixFromSlash,
			wantNet:   "unix",
			wantAddr:  "/var/run/docker.sock",
			wantBase:  "http://docker",
		},
		{
			name:      "unix macOS home socket",
			host:      "unix:///Users/me/.gvx/engine/dockerd.sock",
			fromSlash: unixFromSlash,
			wantNet:   "unix",
			wantAddr:  "/Users/me/.gvx/engine/dockerd.sock",
			wantBase:  "http://docker",
		},
		{
			name:      "unix windows drive two slashes",
			host:      "unix://C:/Users/a/.gvx/engine/dockerd.sock",
			fromSlash: winFromSlash,
			wantNet:   "unix",
			wantAddr:  `C:\Users\a\.gvx\engine\dockerd.sock`,
			wantBase:  "http://docker",
		},
		{
			name:      "unix windows drive three slashes",
			host:      "unix:///C:/Users/a/.gvx/engine/dockerd.sock",
			fromSlash: winFromSlash,
			wantNet:   "unix",
			wantAddr:  `C:\Users\a\.gvx\engine\dockerd.sock`,
			wantBase:  "http://docker",
		},
		{
			name:      "unix windows lowercase drive",
			host:      "unix://d:/gvx/dockerd.sock",
			fromSlash: winFromSlash,
			wantNet:   "unix",
			wantAddr:  `d:\gvx\dockerd.sock`,
			wantBase:  "http://docker",
		},
		{
			name:      "unix opaque form",
			host:      "unix:/var/run/docker.sock",
			fromSlash: unixFromSlash,
			wantNet:   "unix",
			wantAddr:  "/var/run/docker.sock",
			wantBase:  "http://docker",
		},
		{
			name:      "tcp loopback",
			host:      "tcp://127.0.0.1:2375",
			fromSlash: unixFromSlash,
			wantNet:   "tcp",
			wantAddr:  "127.0.0.1:2375",
			wantBase:  "http://127.0.0.1:2375",
		},
		{
			name:      "http loopback",
			host:      "http://127.0.0.1:23750",
			fromSlash: unixFromSlash,
			wantNet:   "tcp",
			wantAddr:  "127.0.0.1:23750",
			wantBase:  "http://127.0.0.1:23750",
		},
		{
			name:      "empty unix path",
			host:      "unix://",
			fromSlash: unixFromSlash,
			wantErr:   true,
		},
		{
			name:      "unsupported scheme",
			host:      "ssh://user@host",
			fromSlash: unixFromSlash,
			wantErr:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			netw, addr, base, err := parseHostFor(tc.host, tc.fromSlash)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got (%q, %q, %q)", tc.host, netw, addr, base)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.host, err)
			}
			if netw != tc.wantNet || addr != tc.wantAddr || base != tc.wantBase {
				t.Fatalf("parseHostFor(%q) = (%q, %q, %q), want (%q, %q, %q)",
					tc.host, netw, addr, base, tc.wantNet, tc.wantAddr, tc.wantBase)
			}
		})
	}
}

func TestContainersAndActions(t *testing.T) {
	var calls []string
	c := engine(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		switch {
		case r.URL.Path == "/containers/json":
			_, _ = w.Write([]byte(`[{"Id":"3f2a9c1b7d4e5f60aa","Names":["/web"],"Image":"nginx","State":"running","Status":"Up 2 hours",
				"Ports":[{"PrivatePort":80,"PublicPort":9000,"Type":"tcp"},{"PrivatePort":80,"PublicPort":9000,"Type":"tcp"}],
				"Mounts":[{"Type":"bind","Source":"/Users/me/site","Destination":"/usr/share/nginx/html","Mode":"","RW":true,"Propagation":"rprivate"},
					{"Type":"volume","Name":"cache","Source":"/var/lib/docker/volumes/cache/_data","Destination":"/cache","Driver":"local","RW":true}]}]`))
		case r.URL.Path == "/containers/gone/start":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"No such container: gone"}`))
		case r.URL.Path == "/containers/web/stop":
			w.WriteHeader(http.StatusNotModified) // already stopped
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	ctx := context.Background()
	list, err := c.Containers(ctx, true)
	if err != nil || len(list) != 1 {
		t.Fatalf("Containers: %v, %v", list, err)
	}
	if ctr := list[0]; ctr.Name() != "web" || ctr.ShortID() != "3f2a9c1b7d4e" || ctr.PortsText() != "9000→80/tcp" {
		t.Errorf("container %q %q %q", ctr.Name(), ctr.ShortID(), ctr.PortsText())
	}
	if b := list[0].Binds(); len(b) != 1 || b[0] != (Mount{Type: "bind", Source: "/Users/me/site", Destination: "/usr/share/nginx/html", RW: true}) {
		t.Errorf("binds %+v", b)
	}
	if err := c.Stop(ctx, "web"); err != nil {
		t.Errorf("a 304 is no error: %v", err)
	}
	if err := c.Start(ctx, "gone"); err == nil || err.Error() != "No such container: gone" {
		t.Errorf("the engine's message: %v", err)
	}
	if err := c.Remove(ctx, "web", true); err != nil {
		t.Error(err)
	}
	want := []string{"GET /containers/json?all=1", "POST /containers/web/stop?t=10", "POST /containers/gone/start", "DELETE /containers/web?force=1"}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests\n%s\nwant\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}

func TestLogsOfAContainerWithoutATerminal(t *testing.T) {
	var query string
	c := engine(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Path + "?" + r.URL.RawQuery
		var b []byte
		b = append(b, frame(1, "2026-11-16T06:24:33.000000001Z ready\n")...)
		b = append(b, frame(2, "2026-11-16T06:24:34.5Z warn: slow")...) // a line in two runs
		b = append(b, frame(2, " disk\n")...)
		b = append(b, frame(1, "2026-11-16T06:24:35Z \n")...) // an empty line
		b = append(b, frame(1, "2026-11-16T06:24:36Z last, unterminated")...)
		_, _ = w.Write(b)
	})
	lines, err := c.Logs(context.Background(), "web", 200)
	if err != nil {
		t.Fatal(err)
	}
	if query != "/containers/web/logs?stdout=1&stderr=1&timestamps=1&tail=200" {
		t.Errorf("asked for %s", query)
	}
	want := []LogLine{
		{Time: time.Date(2026, 11, 16, 6, 24, 33, 1, time.UTC), Text: "ready"},
		{Stderr: true, Time: time.Date(2026, 11, 16, 6, 24, 34, 5e8, time.UTC), Text: "warn: slow disk"},
		{Time: time.Date(2026, 11, 16, 6, 24, 35, 0, time.UTC), Text: ""},
		{Time: time.Date(2026, 11, 16, 6, 24, 36, 0, time.UTC), Text: "last, unterminated"},
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines: %+v", len(lines), lines)
	}
	for i := range want {
		if g, w := lines[i], want[i]; g.Stderr != w.Stderr || !g.Time.Equal(w.Time) || g.Text != w.Text {
			t.Errorf("line %d: %+v, want %+v", i, g, w)
		}
	}
}

func TestLogsOfAContainerWithATerminal(t *testing.T) {
	c := engine(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("2026-11-16T06:24:33Z $ ls\r\n2026-11-16T06:24:33Z bin  etc\r\nno time here\n"))
	})
	lines, err := c.Logs(context.Background(), "tty", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 || lines[0].Text != "$ ls" || lines[1].Text != "bin  etc" || lines[1].Stderr {
		t.Fatalf("lines %+v", lines)
	}
	if lines[2].Text != "no time here" || !lines[2].Time.IsZero() {
		t.Errorf("a line with no time: %+v", lines[2])
	}
}

func TestLogsOfAMissingContainer(t *testing.T) {
	c := engine(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"No such container: x"}`))
	})
	if _, err := c.Logs(context.Background(), "x", 10); err == nil || !strings.Contains(err.Error(), "No such container") {
		t.Fatalf("err %v", err)
	}
}

func TestExitCode(t *testing.T) {
	for _, tc := range []struct {
		state, status string
		code          int
		ok            bool
	}{
		{"exited", "Exited (0) 2 hours ago", 0, true},
		{"exited", "Exited (137) About a minute ago", 137, true},
		{"running", "Up 2 hours", 0, false},
		{"created", "Created", 0, false},
		{"exited", "something else", 0, false},
	} {
		code, ok := Container{State: tc.state, Status: tc.status}.ExitCode()
		if code != tc.code || ok != tc.ok {
			t.Errorf("%s %q: %d %v", tc.state, tc.status, code, ok)
		}
	}
}

func TestImagesInspectRemovePruneAndPull(t *testing.T) {
	var calls []string
	c := engine(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/images/json":
			_, _ = w.Write([]byte(`[{"Id":"sha256:abcdef1234567890","RepoTags":["ghcr.io/org/app:1.0"],"Size":1048576,"Created":1700000000}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/images/ghcr.io/org/app:1.0/json":
			_, _ = w.Write([]byte(`{"Id":"sha256:abcdef1234567890","Architecture":"arm64"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/images/ghcr.io/org/app:1.0":
			_, _ = w.Write([]byte(`[{"Untagged":"ghcr.io/org/app:1.0"}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/images/prune":
			_, _ = w.Write([]byte(`{"ImagesDeleted":[{"Deleted":"sha256:111"},{"Untagged":"old:tag"}],"SpaceReclaimed":4096}`))
		case r.Method == http.MethodPost && r.URL.Path == "/images/create":
			_, _ = w.Write([]byte("{\"status\":\"Pulling from library/nginx\",\"id\":\"alpine\"}\n{\"status\":\"Downloading\",\"id\":\"a1b2\",\"progress\":\"[===>] 1MB/2MB\"}\n{\"status\":\"Status: Downloaded newer image for nginx:alpine\"}\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ctx := context.Background()
	imgs, err := c.Images(ctx)
	if err != nil || len(imgs) != 1 || imgs[0].ShortID() != "abcdef123456" {
		t.Fatalf("Images: %+v, %v", imgs, err)
	}
	inspected, err := c.InspectImage(ctx, "ghcr.io/org/app:1.0")
	if err != nil || !strings.Contains(inspected, `"Architecture": "arm64"`) {
		t.Fatalf("InspectImage: %q, %v", inspected, err)
	}
	if err := c.RemoveImage(ctx, "ghcr.io/org/app:1.0", true); err != nil {
		t.Fatalf("RemoveImage: %v", err)
	}
	pruned, err := c.PruneImages(ctx, false)
	if err != nil || pruned.Deleted != 1 || pruned.SpaceReclaimed != 4096 {
		t.Fatalf("PruneImages: %+v, %v", pruned, err)
	}
	var lines []string
	if err := c.PullImage(ctx, "nginx:alpine", func(s string) { lines = append(lines, s) }); err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	if len(lines) != 3 || !strings.Contains(lines[1], "a1b2: Downloading") {
		t.Errorf("pull lines: %v", lines)
	}
	want := []string{
		"GET /images/json",
		"GET /images/ghcr.io/org/app:1.0/json",
		"DELETE /images/ghcr.io/org/app:1.0?force=1",
		"POST /images/prune",
		"POST /images/create?fromImage=nginx&tag=alpine",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests\n%s\nwant\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}

func TestImageManifestsArchitecture(t *testing.T) {
	c := engine(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
			{
				"Id": "sha256:1111222233334444",
				"RepoTags": ["gvxd-amd64:1"],
				"Size": 1024,
				"Created": 1700000000,
				"Manifests": [
					{"ID": "sha256:aaa", "Available": false, "Kind": "image", "ImageData": {"Platform": {"architecture": "arm64", "os": "linux"}}},
					{"ID": "sha256:bbb", "Available": true, "Kind": "attestation", "ImageData": {"Platform": {"architecture": "unknown", "os": "unknown"}}},
					{"ID": "sha256:ccc", "Available": true, "Kind": "image", "ImageData": {"Platform": {"architecture": "amd64", "os": "linux"}}}
				]
			},
			{
				"Id": "sha256:5555666677778888",
				"RepoTags": ["alpine:3.20"],
				"Size": 2048,
				"Created": 1700000001
			}
		]`))
	})
	imgs, err := c.Images(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 2 {
		t.Fatalf("got %d images, want 2", len(imgs))
	}
	if got := imgs[0].Architecture(); got != "amd64" {
		t.Errorf("imgs[0].Architecture() = %q, want amd64", got)
	}
	if got := imgs[1].Architecture(); got != "" {
		t.Errorf("imgs[1].Architecture() = %q, want empty", got)
	}
	other := imgs[0]
	other.Manifests = []ImageManifest{{ID: "sha256:ccc", Available: true, Kind: "image", ImageData: &ImageManifestData{Platform: ImagePlatform{Architecture: "arm64", OS: "linux"}}}}
	if imgs[0].Equal(other) {
		t.Error("Image.Equal returned true for different Manifests")
	}
}

func TestImagePlatformScopedEndpoints(t *testing.T) {
	var calls []string
	c := engine(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"Id":"sha256:amd64","Architecture":"amd64"}`))
		case http.MethodDelete:
			_, _ = w.Write([]byte(`[{"Untagged":"app:1"}]`))
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
		}
	})
	ctx := context.Background()
	if _, err := c.InspectImagePlatform(ctx, "app:1", "linux/amd64"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveImagePlatform(ctx, "app:1", true, "linux/amd64"); err != nil {
		t.Fatal(err)
	}
	if err := c.TagPlatform(ctx, "app:1", "localhost:5001/app", "1", "linux/amd64"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET /images/app:1/json?platform=linux%2Famd64",
		"DELETE /images/app:1?force=1&platform=linux%2Famd64",
		"POST /images/app:1/tag?platform=linux%2Famd64&repo=localhost%3A5001%2Fapp&tag=1",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests\n%s\nwant\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}

func TestSplitImageRef(t *testing.T) {
	for _, tc := range []struct {
		ref, from, tag string
	}{
		{"nginx", "nginx", "latest"},
		{"nginx:alpine", "nginx", "alpine"},
		{"localhost:5000/app", "localhost:5000/app", "latest"},
		{"localhost:5000/app:v2", "localhost:5000/app", "v2"},
		{"nginx@sha256:abc", "nginx@sha256:abc", ""},
	} {
		from, tag := splitImageRef(tc.ref)
		if from != tc.from || tag != tc.tag {
			t.Errorf("%q -> (%q, %q), want (%q, %q)", tc.ref, from, tag, tc.from, tc.tag)
		}
	}
}

func TestPullImageReportsStreamError(t *testing.T) {
	c := engine(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errorDetail":{"message":"pull access denied for private/app"},"error":"pull access denied for private/app"}` + "\n"))
	})
	if err := c.PullImage(context.Background(), "private/app", nil); err == nil || !strings.Contains(err.Error(), "pull access denied") {
		t.Fatalf("err = %v", err)
	}
}

// A tag and a push go where the API has them, the push with the empty
// credentials dockerd wants, and its progress and its error come back.
func TestTagAndPush(t *testing.T) {
	var calls []string
	var auth string
	c := engine(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		switch {
		case strings.HasSuffix(r.URL.Path, "/tag"):
			w.WriteHeader(http.StatusCreated)
		case strings.Contains(r.URL.Path, "/gone/"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"No such image: host.docker.internal:5001/gone:1"}`))
		case strings.Contains(r.URL.Path, "/denied/"):
			_, _ = w.Write([]byte(`{"status":"The push refers to repository [host.docker.internal:5001/denied]"}` + "\n" +
				`{"errorDetail":{"message":"Get \"https://host.docker.internal:5001/v2/\": http: server gave HTTP response to HTTPS client"},"error":"x"}` + "\n"))
		default:
			auth = r.Header.Get("X-Registry-Auth")
			_, _ = w.Write([]byte(`{"status":"The push refers to repository [host.docker.internal:5001/acme/app]"}` + "\n" +
				`{"status":"Pushed","progressDetail":{},"id":"3f26bc2dec0b"}` + "\n" +
				`{"status":"dev: digest: sha256:45e0 size: 1026"}` + "\n"))
		}
	})
	ctx := context.Background()
	if err := c.Tag(ctx, "ghcr.io/acme/app:dev", "host.docker.internal:5001/acme/app", "dev"); err != nil {
		t.Fatal(err)
	}
	var lines []string
	if err := c.PushImage(ctx, "host.docker.internal:5001/acme/app:dev", func(l string) { lines = append(lines, l) }); err != nil {
		t.Fatal(err)
	}
	if auth != "e30=" {
		t.Errorf("X-Registry-Auth %q", auth)
	}
	if len(lines) != 3 || lines[1] != "3f26bc2dec0b: Pushed" {
		t.Errorf("progress %q", lines)
	}
	if err := c.PushImage(ctx, "host.docker.internal:5001/denied:1", nil); err == nil || !strings.Contains(err.Error(), "HTTP response to HTTPS client") {
		t.Errorf("the stream's error: %v", err)
	}
	if err := c.PushImage(ctx, "host.docker.internal:5001/gone:1", nil); err == nil || err.Error() != "No such image: host.docker.internal:5001/gone:1" {
		t.Errorf("the engine's message: %v", err)
	}
	want := []string{
		"POST /images/ghcr.io/acme/app:dev/tag?repo=host.docker.internal%3A5001%2Facme%2Fapp&tag=dev",
		"POST /images/host.docker.internal:5001/acme/app/push?tag=dev",
		"POST /images/host.docker.internal:5001/denied/push?tag=1",
		"POST /images/host.docker.internal:5001/gone/push?tag=1",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests\n%s\nwant\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}

// TestPushToARegistry pushes through a real dockerd to a real registry, as
// the Images page does: set GVXD_PUSH_DOCKER to the engine's DOCKER_HOST
// and GVXD_PUSH_REGISTRY to the registry as the engine reaches it, such as
// host.docker.internal:15001. The engine must have alpine:3.20.
func TestPushToARegistry(t *testing.T) {
	host, registry := os.Getenv("GVXD_PUSH_DOCKER"), os.Getenv("GVXD_PUSH_REGISTRY")
	if testing.Short() || host == "" || registry == "" {
		t.Skip("set GVXD_PUSH_DOCKER and GVXD_PUSH_REGISTRY")
	}
	c, err := New(host)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	repo := registry + "/gvxd-test/pushed"
	if err := c.Tag(ctx, "alpine:3.20", repo, "api"); err != nil {
		t.Fatal(err)
	}
	defer c.RemoveImage(context.Background(), repo+":api", false)
	if err := c.PushImage(ctx, repo+":api", func(l string) { t.Log(l) }); err != nil {
		t.Fatal(err)
	}
}

func TestNetworks(t *testing.T) {
	var calls []string
	c := engine(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		switch {
		case r.URL.Path == "/networks" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[{"Name":"bridge","Id":"aa","Driver":"bridge","IPAM":{"Config":[{"Subnet":"10.0.2.0/24","Gateway":"10.0.2.2"}]},"Containers":{}},
				{"Name":"shop_default","Id":"bb11223344556677","Created":"2026-11-16T13:18:00.5Z","Driver":"bridge","Internal":true,
				 "IPAM":{"Config":[{"Subnet":"10.89.1.0/24","Gateway":"10.89.1.1"}]},
				 "Containers":{"c1":{"Name":"shop-web-1","IPv4Address":"10.89.1.2/24"}},
				 "Labels":{"com.docker.compose.project":"shop"}}]`))
		case r.URL.Path == "/networks/prune":
			_, _ = w.Write([]byte(`{"NetworksDeleted":["old"]}`))
		case r.URL.Path == "/containers/json":
			_, _ = w.Write([]byte(`[{"Id":"c1","Names":["/shop-web-1"],"NetworkSettings":{"Networks":{
				"shop_default":{"NetworkID":"bb","IPAddress":"10.89.1.2","IPPrefixLen":24,"Aliases":["web"],"DNSNames":["shop-web-1","web","c1"]},
				"a_front":{"IPAddress":"10.89.2.2"}}}}]`))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	ctx := context.Background()
	nets, err := c.Networks(ctx)
	if err != nil || len(nets) != 2 {
		t.Fatalf("Networks: %v, %v", nets, err)
	}
	if !nets[0].Builtin() || nets[1].Builtin() {
		t.Error("builtin")
	}
	n := nets[1]
	if sub, gw := n.Subnet(); sub != "10.89.1.0/24" || gw != "10.89.1.1" || n.Project() != "shop" || !n.Internal || n.ShortID() != "bb1122334455" {
		t.Errorf("network %+v", n)
	}
	if n.CreatedAt().IsZero() || n.Containers["c1"].IPv4Address != "10.89.1.2/24" {
		t.Errorf("created %v, members %+v", n.CreatedAt(), n.Containers)
	}
	if !n.Equal(n) || n.Equal(nets[0]) {
		t.Error("Equal")
	}
	if err := c.RemoveNetwork(ctx, "shop_default"); err != nil {
		t.Error(err)
	}
	if del, err := c.PruneNetworks(ctx); err != nil || len(del) != 1 || del[0] != "old" {
		t.Errorf("prune %v %v", del, err)
	}

	list, err := c.Containers(ctx, true)
	if err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	got := list[0].Networks()
	if len(got) != 2 || got[0].Name != "a_front" || got[1].IPAddress != "10.89.1.2" || got[1].Aliases[0] != "web" {
		t.Errorf("container networks %+v", got)
	}
	other := list[0]
	other.NetworkSettings.Networks = map[string]Endpoint{"shop_default": {IPAddress: "10.89.1.9"}}
	if list[0].Equal(other) {
		t.Error("Equal ignores networks")
	}
	want := []string{"GET /networks", "DELETE /networks/shop_default", "POST /networks/prune", "GET /containers/json?all=1"}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests\n%s", strings.Join(calls, "\n"))
	}
}
