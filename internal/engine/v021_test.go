package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

func TestImagePushUnsupportedBeforeV021(t *testing.T) {
	te := newTestEngine(t)
	te.fake.addImage("localhost:5000/app:v1", ImageConfig{Cmd: []string{"/bin/sh"}})

	status, body := te.do(t, http.MethodPost, "/v1.52/images/localhost:5000/app/push?tag=v1", nil)
	if status != http.StatusNotImplemented {
		t.Fatalf("status = %d %s, want 501", status, body)
	}
	if !strings.Contains(string(body), "Pushing an image (it needs rungv 0.21)") {
		t.Fatalf("unexpected 501 message: %s", body)
	}
}

func TestImagePushV021ProgressAuthAndRegistryURL(t *testing.T) {
	te := newTestEngine(t)
	te.fake.features = rungv.Features{KeepBlobs: true}
	imgID := te.fake.addImage("ghcr.io/org/app:v1", ImageConfig{Cmd: []string{"/bin/sh"}})
	te.fake.addImage("host.docker.internal:5001/demo/app:latest", ImageConfig{Cmd: []string{"/bin/sh"}})
	te.fake.addImage("HOST.DOCKER.INTERNAL:5001/demo/upper:v2", ImageConfig{Cmd: []string{"/bin/sh"}})

	foreign := foreignImageArch()
	te.fake.addForeignImage(foreign, "ghcr.io/org/amd64-only:1", ImageConfig{Cmd: []string{"/bin/sh"}})
	te.fake.addImage("ghcr.io/org/multi:1", ImageConfig{Cmd: []string{"/bin/sh"}})
	te.fake.addForeignImage(foreign, "ghcr.io/org/multi:1", ImageConfig{Cmd: []string{"/bin/sh"}})

	// 1. Missing image returns 404 before streaming.
	st404, b404 := te.do(t, http.MethodPost, "/v1.52/images/ghcr.io/org/missing/push?tag=v1", nil)
	if st404 != http.StatusNotFound {
		t.Fatalf("missing image status = %d %s, want 404", st404, b404)
	}

	// 2. Progress streaming + aux digest (without size -> Size: 0) + non-empty X-Registry-Auth.
	te.fake.pushFunc = func(_ context.Context, _ rungv.PushOptions, line func(string)) error {
		line("The push refers to repository [ghcr.io/org/app]")
		line("aaa111222333: Layer already exists")
		line("bbb444555666: Mounted from org/base")
		line("ccc777888999: Pushed")
		line("v1: digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
		return nil
	}
	authJSON := `{"username":"alice","password":"secret"}`
	req, err := http.NewRequest(http.MethodPost, te.srv.URL+"/v1.52/images/ghcr.io/org/app/push?tag=v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Registry-Auth", base64.URLEncoding.EncodeToString([]byte(authJSON)))
	resp, err := te.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rawBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("push status = %d %s, want 200", resp.StatusCode, rawBody)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	lines := strings.Split(strings.TrimSpace(string(rawBody)), "\n")
	if len(lines) != 6 {
		t.Fatalf("got %d JSON frames, want 6:\n%s", len(lines), rawBody)
	}
	var f0, f1, f2, f3, f4, f5 map[string]any
	for i, dst := range []*map[string]any{&f0, &f1, &f2, &f3, &f4, &f5} {
		if err := json.Unmarshal([]byte(lines[i]), dst); err != nil {
			t.Fatalf("frame %d invalid JSON %q: %v", i, lines[i], err)
		}
	}
	if f0["status"] != "The push refers to repository [ghcr.io/org/app]" {
		t.Errorf("frame 0 = %v", f0)
	}
	if f1["id"] != "aaa111222333" || f1["status"] != "Layer already exists" {
		t.Errorf("frame 1 = %v", f1)
	}
	if f2["id"] != "bbb444555666" || f2["status"] != "Mounted from org/base" {
		t.Errorf("frame 2 = %v", f2)
	}
	if f3["id"] != "ccc777888999" || f3["status"] != "Pushed" {
		t.Errorf("frame 3 = %v", f3)
	}
	if f4["status"] != "v1: digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
		t.Errorf("frame 4 = %v", f4)
	}
	auxMap, _ := f5["aux"].(map[string]any)
	if auxMap["Tag"] != "v1" || auxMap["Digest"] != "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" || auxMap["Size"] != float64(0) {
		t.Errorf("frame 5 aux = %v", f5)
	}

	te.fake.mu.Lock()
	if len(te.fake.pushCalls) != 1 {
		te.fake.mu.Unlock()
		t.Fatalf("pushCalls = %d, want 1", len(te.fake.pushCalls))
	}
	call := te.fake.pushCalls[0]
	te.fake.mu.Unlock()
	if call.Ref != "ghcr.io/org/app:v1" || call.AuthPayload != authJSON || call.RegistryURL != "" || call.Arch != "" {
		t.Errorf("unexpected PushOptions: %+v", call)
	}

	// Verify image push event was recorded.
	assertImagePushEvent(t, te.Engine, imgID, "ghcr.io/org/app:v1")

	// 3. Empty {} X-Registry-Auth (as sent by docker CLI / dockerapi.PushImage) leaves AuthPayload empty,
	//    and host.docker.internal:5001 maps RegistryURL to http://127.0.0.1:5001.
	te.fake.pushFunc = nil
	for _, tc := range []struct {
		name     string
		path     string
		authHdr  string
		wantURL  string
		wantAuth string
	}{
		{
			name:     "empty_url_base64_padded",
			path:     "/v1.52/images/host.docker.internal:5001/demo/app/push?tag=latest",
			authHdr:  base64.URLEncoding.EncodeToString([]byte("{}")),
			wantURL:  "http://127.0.0.1:5001",
			wantAuth: "",
		},
		{
			name:     "empty_raw_json_case_insensitive_host",
			path:     "/v1.52/images/HOST.DOCKER.INTERNAL:5001/demo/upper/push?tag=v2",
			authHdr:  `{}`,
			wantURL:  "http://127.0.0.1:5001",
			wantAuth: "",
		},
		{
			name:     "std_raw_base64_identity_token",
			path:     "/v1.52/images/ghcr.io/org/app/push?tag=v1",
			authHdr:  base64.RawStdEncoding.EncodeToString([]byte(`{"identitytoken":"tok-123"}`)),
			wantURL:  "",
			wantAuth: `{"identitytoken":"tok-123"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(te.fake.pushCalls)
			r, err := http.NewRequest(http.MethodPost, te.srv.URL+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set("X-Registry-Auth", tc.authHdr)
			res, err := te.srv.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("status = %d %s", res.StatusCode, body)
			}
			te.fake.mu.Lock()
			got := te.fake.pushCalls[before]
			te.fake.mu.Unlock()
			if got.RegistryURL != tc.wantURL {
				t.Errorf("RegistryURL = %q, want %q", got.RegistryURL, tc.wantURL)
			}
			if got.AuthPayload != tc.wantAuth {
				t.Errorf("AuthPayload = %q, want %q", got.AuthPayload, tc.wantAuth)
			}
		})
	}

	// 4. Foreign-arch push: both implicit (only exists in foreign ns) and explicit ?platform=.
	before := len(te.fake.pushCalls)
	stF1, bF1 := te.do(t, http.MethodPost, "/v1.52/images/ghcr.io/org/amd64-only/push?tag=1", nil)
	if stF1 != http.StatusOK {
		t.Fatalf("foreign implicit push = %d %s", stF1, bF1)
	}
	te.fake.mu.Lock()
	gotF1 := te.fake.pushCalls[before]
	te.fake.mu.Unlock()
	if gotF1.Arch != foreign || gotF1.Ref != "ghcr.io/org/amd64-only:1" {
		t.Errorf("foreign implicit PushOptions = %+v, want Arch=%q", gotF1, foreign)
	}

	before = len(te.fake.pushCalls)
	stF2, bF2 := te.do(t, http.MethodPost, "/v1.52/images/ghcr.io/org/multi/push?tag=1&platform=linux/"+foreign, nil)
	if stF2 != http.StatusOK {
		t.Fatalf("foreign explicit platform push = %d %s", stF2, bF2)
	}
	te.fake.mu.Lock()
	gotF2 := te.fake.pushCalls[before]
	te.fake.mu.Unlock()
	if gotF2.Arch != foreign || gotF2.Platform != "linux/"+foreign {
		t.Errorf("foreign explicit PushOptions = %+v, want Arch=%q Platform=linux/%s", gotF2, foreign, foreign)
	}

	// 5. Error mid-stream produces Docker errorDetail JSON frame.
	te.fake.pushFunc = func(_ context.Context, _ rungv.PushOptions, line func(string)) error {
		line("The push refers to repository [ghcr.io/org/app]")
		return errors.New("unauthorized: authentication required")
	}
	stErr, bErr := te.do(t, http.MethodPost, "/v1.52/images/ghcr.io/org/app/push?tag=v1", nil)
	if stErr != http.StatusOK {
		t.Fatalf("mid-stream error status = %d %s, want 200 stream", stErr, bErr)
	}
	errLines := strings.Split(strings.TrimSpace(string(bErr)), "\n")
	if len(errLines) != 2 {
		t.Fatalf("mid-stream error frames = %d, want 2:\n%s", len(errLines), bErr)
	}
	var errFrame struct {
		Error       string `json:"error"`
		ErrorDetail struct {
			Message string `json:"message"`
		} `json:"errorDetail"`
	}
	if err := json.Unmarshal([]byte(errLines[1]), &errFrame); err != nil {
		t.Fatalf("unmarshal error frame: %v", err)
	}
	if !strings.Contains(errFrame.Error, "unauthorized") || errFrame.ErrorDetail.Message != errFrame.Error {
		t.Errorf("unexpected error frame: %+v", errFrame)
	}

	// 6. Invalid X-Registry-Auth header returns 400.
	badReq, _ := http.NewRequest(http.MethodPost, te.srv.URL+"/v1.52/images/ghcr.io/org/app/push?tag=v1", nil)
	badReq.Header.Set("X-Registry-Auth", "!!!not-base64!!!")
	badRes, err := te.srv.Client().Do(badReq)
	if err != nil {
		t.Fatal(err)
	}
	badRes.Body.Close()
	if badRes.StatusCode != http.StatusBadRequest {
		t.Errorf("bad auth header status = %d, want 400", badRes.StatusCode)
	}
}

func assertImagePushEvent(t *testing.T, eng *Engine, wantID, wantName string) {
	t.Helper()
	eng.evt.mu.Lock()
	defer eng.evt.mu.Unlock()
	for i := len(eng.evt.ring) - 1; i >= 0; i-- {
		ev := eng.evt.ring[i]
		if ev.Type == "image" && ev.Action == "push" {
			if ev.ID != wantID || ev.Attributes["name"] != wantName {
				t.Errorf("push event = %+v, want ID=%q name=%q", ev, wantID, wantName)
			}
			return
		}
	}
	t.Errorf("no image push event emitted")
}

func TestParseRegistryAuthHeader(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty", "", "", false},
		{"whitespace", "   ", "", false},
		{"raw_empty_obj", "{}", "", false},
		{"raw_server_only", `{"serveraddress":"https://index.docker.io/v1/"}`, "", false},
		{"raw_null", "null", "", false},
		{"url_b64_empty_obj", base64.URLEncoding.EncodeToString([]byte("{}")), "", false},
		{"raw_url_b64_empty_obj", base64.RawURLEncoding.EncodeToString([]byte("{}")), "", false},
		{"std_b64_empty_obj", base64.StdEncoding.EncodeToString([]byte("{}")), "", false},
		{"raw_std_b64_empty_obj", base64.RawStdEncoding.EncodeToString([]byte("{}")), "", false},
		{"url_b64_user_pass", base64.URLEncoding.EncodeToString([]byte(`{"username":"u","password":"p"}`)), `{"username":"u","password":"p"}`, false},
		{"raw_url_b64_auth", base64.RawURLEncoding.EncodeToString([]byte(`{"auth":"dTpw"}`)), `{"auth":"dTpw"}`, false},
		{"std_b64_token", base64.StdEncoding.EncodeToString([]byte(`{"identitytoken":"abc"}`)), `{"identitytoken":"abc"}`, false},
		{"raw_json_creds", `{"username":"u","password":"p"}`, `{"username":"u","password":"p"}`, false},
		{"invalid", "not-valid-b64!!!", "", true},
		{"invalid_b64_json", base64.StdEncoding.EncodeToString([]byte("not json")), "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRegistryAuthHeader(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr=%v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLegacyBuildUnixForwardVsTCPBridge(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses unix backend socket in fake buildkit")
	}
	for _, tc := range []struct {
		name        string
		unixForward bool
	}{
		{"v021_unix_forward", true},
		{"v020_tcp_bridge", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := os.MkdirTemp("", "gvxd-uf-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })

			fake := newFakeRunner()
			fake.features = rungv.Features{UnixForward: tc.unixForward}
			fake.addImage(BuilderImage, ImageConfig{})

			var capturedSpec sandbox.Spec
			fake.Script = func(p *fakeProc) int {
				if p.Spec.ID == builderSandboxID {
					var sockPath string
					for _, port := range p.Spec.Ports {
						if rest, ok := strings.CutPrefix(port, "unix:"); ok {
							if i := strings.LastIndexByte(rest, ':'); i > 0 {
								sockPath = rest[:i]
							}
						}
					}
					_ = os.Remove(sockPath)
					ln, err := net.Listen("unix", sockPath)
					if err != nil {
						return 1
					}
					defer func() {
						_ = ln.Close()
						_ = os.Remove(sockPath)
					}()
					go func() {
						for {
							c, err := ln.Accept()
							if err != nil {
								return
							}
							_ = c.Close()
						}
					}()
					code := <-p.end
					select {
					case p.end <- code:
					default:
					}
					return code
				}
				if strings.HasPrefix(p.Spec.ID, builderBuildctlPrefix) {
					capturedSpec = p.Spec.Clone()
					for _, m := range p.Spec.Mounts {
						if m.Target == "/out" {
							_ = os.WriteFile(filepath.Join(m.Source, "image.tar"), []byte("ufapp:1\n"), 0o600)
						}
					}
					return 0
				}
				return 0
			}

			eng := New(Config{Dir: dir, Runner: fake, Version: "0.0.0-test", BuilderSocketDir: dir})
			if err := eng.Open(context.Background()); err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(eng.Handler())
			t.Cleanup(func() {
				srv.Close()
				_ = eng.Close(time.Second)
			})

			ctxArchive := makeTestTar(t, []struct {
				hdr  tar.Header
				body string
			}{{hdr: tar.Header{Name: "Dockerfile"}, body: "FROM alpine:3.20\n"}})

			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1.52/build?t=ufapp:1", bytes.NewReader(ctxArchive))
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("POST /build = %d %s", resp.StatusCode, body)
			}

			args, _ := rungv.SplitArgs(capturedSpec.Command)
			backendSock := filepath.Join(dir, "buildkitd.sock")
			if tc.unixForward {
				wantUF := []string{"/run/buildkit/buildkitd.sock=" + backendSock}
				if !slices.Equal(capturedSpec.UnixForwards, wantUF) {
					t.Errorf("UnixForwards = %v, want %v", capturedSpec.UnixForwards, wantUF)
				}
				if !slices.Contains(args, "unix:///run/buildkit/buildkitd.sock") {
					t.Errorf("buildctl args = %v, want unix:///run/buildkit/buildkitd.sock", args)
				}
			} else {
				if len(capturedSpec.UnixForwards) != 0 {
					t.Errorf("UnixForwards = %v, want empty on v0.20", capturedSpec.UnixForwards)
				}
				idx := slices.Index(args, "--addr")
				if idx < 0 || idx+1 >= len(args) || !strings.HasPrefix(args[idx+1], "tcp://10.0.2.2:") {
					t.Errorf("buildctl --addr = %v, want tcp://10.0.2.2:<port>", args)
				}
			}
		})
	}
}

func TestContainerUnixSocketBindMounts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix domain socket mode check is POSIX-specific")
	}
	sockDir, err := os.MkdirTemp("", "gvxd-us-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })

	// Create two real Unix sockets and one regular directory on host.
	dockerSock := filepath.Join(sockDir, "docker.sock")
	lnDocker, err := net.Listen("unix", dockerSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lnDocker.Close() })

	customSock := filepath.Join(sockDir, "custom.sock")
	lnCustom, err := net.Listen("unix", customSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lnCustom.Close() })

	symlinkSock := filepath.Join(sockDir, "link.sock")
	if err := os.Symlink(customSock, symlinkSock); err != nil {
		t.Fatal(err)
	}

	regularFile := filepath.Join(sockDir, "not-a-socket")
	if err := os.WriteFile(regularFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	hostDir := filepath.Join(sockDir, "data")
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		t.Fatal(err)
	}

	newEnv := func(unixForward bool, cfgDockerSock string) *testEngine {
		dir := t.TempDir()
		fake := newFakeRunner()
		fake.features = rungv.Features{UnixForward: unixForward}
		fake.addImage("alpine:3.20", ImageConfig{Cmd: []string{"/bin/sh"}})
		eng := New(Config{
			Dir:          dir,
			Runner:       fake,
			Version:      "0.0.0-test",
			DockerSocket: cfgDockerSock,
		})
		if err := eng.Open(context.Background()); err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(eng.Handler())
		te := &testEngine{Engine: eng, fake: fake, srv: srv, dir: dir}
		t.Cleanup(func() {
			srv.Close()
			_ = eng.Close(time.Second)
		})
		return te
	}

	// 1. v0.21 (UnixForward=true): Binds with /var/run/docker.sock + custom Unix socket + regular dir.
	t.Run("v021_binds", func(t *testing.T) {
		te := newEnv(true, dockerSock)
		st, body := te.do(t, http.MethodPost, "/v1.52/containers/create?name=uf-binds", map[string]any{
			"Image": "alpine:3.20",
			"HostConfig": map[string]any{
				"Binds": []string{
					"/var/run/docker.sock:/var/run/docker.sock",
					customSock + ":/run/custom.sock",
					symlinkSock + ":/run/link.sock",
					hostDir + ":/data:ro",
				},
			},
		})
		if st != http.StatusCreated {
			t.Fatalf("create = %d %s", st, body)
		}
		var created struct{ ID string }
		_ = json.Unmarshal(body, &created)

		stStart, bStart := te.do(t, http.MethodPost, "/v1.52/containers/"+created.ID+"/start", nil)
		if stStart != http.StatusNoContent {
			t.Fatalf("start = %d %s", stStart, bStart)
		}
		started := te.fake.Started()
		if len(started) != 1 {
			t.Fatalf("started = %d, want 1", len(started))
		}
		spec := started[0]
		wantUF := []string{
			"/var/run/docker.sock=" + dockerSock,
			"/run/custom.sock=" + customSock,
			"/run/link.sock=" + symlinkSock,
		}
		if !slices.Equal(spec.UnixForwards, wantUF) {
			t.Errorf("spec.UnixForwards = %v, want %v", spec.UnixForwards, wantUF)
		}
		// Only the regular directory bind should remain in spec.Mounts.
		if len(spec.Mounts) != 1 || spec.Mounts[0].Type != sandbox.MountBind || spec.Mounts[0].Target != "/data" {
			t.Errorf("spec.Mounts = %+v, want only /data bind", spec.Mounts)
		}

		// docker inspect still reports all 4 binds in Mounts.
		stIns, bIns := te.do(t, http.MethodGet, "/v1.52/containers/"+created.ID+"/json", nil)
		if stIns != http.StatusOK {
			t.Fatalf("inspect = %d %s", stIns, bIns)
		}
		var ins struct {
			Mounts []MountPoint `json:"Mounts"`
		}
		_ = json.Unmarshal(bIns, &ins)
		if len(ins.Mounts) != 4 {
			t.Fatalf("inspect Mounts = %+v, want 4 entries", ins.Mounts)
		}
		if ins.Mounts[0].Type != "bind" || ins.Mounts[0].Source != "/var/run/docker.sock" || ins.Mounts[0].Destination != "/var/run/docker.sock" {
			t.Errorf("inspect Mounts[0] = %+v", ins.Mounts[0])
		}
	})

	// 2. v0.21 (UnixForward=true): HostConfig.Mounts type=bind stats Config.DockerSocket for /var/run/docker.sock.
	t.Run("v021_mounts_bind", func(t *testing.T) {
		te := newEnv(true, dockerSock)
		st, body := te.do(t, http.MethodPost, "/v1.52/containers/create?name=uf-mounts", map[string]any{
			"Image": "alpine:3.20",
			"HostConfig": map[string]any{
				"Mounts": []map[string]any{
					{"Type": "bind", "Source": "/var/run/docker.sock", "Target": "/run/docker.sock"},
					{"Type": "bind", "Source": customSock, "Target": "/run/custom.sock"},
				},
			},
		})
		if st != http.StatusCreated {
			t.Fatalf("create with Mounts = %d %s", st, body)
		}
		var created struct{ ID string }
		_ = json.Unmarshal(body, &created)
		c, err := te.lookup(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		wantUF := []string{
			"/run/docker.sock=" + dockerSock,
			"/run/custom.sock=" + customSock,
		}
		if !slices.Equal(c.Spec.UnixForwards, wantUF) {
			t.Errorf("Spec.UnixForwards = %v, want %v", c.Spec.UnixForwards, wantUF)
		}
		if len(c.Spec.Mounts) != 0 {
			t.Errorf("Spec.Mounts = %+v, want empty", c.Spec.Mounts)
		}
		if len(c.Mounts) != 2 {
			t.Errorf("Container.Mounts = %+v, want 2 entries", c.Mounts)
		}

		// Missing DockerSocket (e.g. Docker API configured on TCP) and non-socket
		// DockerSocket must NOT rewrite /var/run/docker.sock to DockerSocket.
		for _, badCase := range []struct {
			name string
			path string
		}{
			{"missing", filepath.Join(sockDir, "does-not-exist.sock")},
			{"regular_file", regularFile},
		} {
			t.Run("tcp_or_nonsocket_"+badCase.name, func(t *testing.T) {
				teBad := newEnv(true, badCase.path)

				// HostConfig.Binds: must never emit UnixForwards pointing at badCase.path.
				stB, bB := teBad.do(t, http.MethodPost, "/v1.52/containers/create?name=uf-bad-bind", map[string]any{
					"Image": "alpine:3.20",
					"HostConfig": map[string]any{
						"Binds": []string{"/var/run/docker.sock:/var/run/docker.sock"},
					},
				})
				if stB != http.StatusCreated {
					t.Fatalf("create Binds = %d %s", stB, bB)
				}
				var cB struct{ ID string }
				_ = json.Unmarshal(bB, &cB)
				rec, err := teBad.lookup(cB.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, uf := range rec.Spec.UnixForwards {
					if strings.Contains(uf, badCase.path) {
						t.Errorf("Binds forwarded to non-socket/missing DockerSocket: %v", rec.Spec.UnixForwards)
					}
				}
				if !isUnixSocket("/var/run/docker.sock") {
					if len(rec.Spec.UnixForwards) != 0 {
						t.Errorf("Binds UnixForwards = %v, want empty when neither DockerSocket nor /var/run/docker.sock is a socket", rec.Spec.UnixForwards)
					}
					if len(rec.Spec.Mounts) != 1 || rec.Spec.Mounts[0].Type != sandbox.MountBind {
						t.Errorf("Binds Mounts = %+v, want fallback MountBind", rec.Spec.Mounts)
					}
				}

				// HostConfig.Mounts type=bind: falls back to statting /var/run/docker.sock
				// (400 when absent on host; never rewrites to badCase.path).
				stM, bM := teBad.do(t, http.MethodPost, "/v1.52/containers/create?name=uf-bad-mount", map[string]any{
					"Image": "alpine:3.20",
					"HostConfig": map[string]any{
						"Mounts": []map[string]any{
							{"Type": "bind", "Source": "/var/run/docker.sock", "Target": "/var/run/docker.sock"},
						},
					},
				})
				if _, err := os.Stat("/var/run/docker.sock"); err != nil {
					if stM != http.StatusBadRequest {
						t.Errorf("Mounts bind without host /var/run/docker.sock = %d %s, want 400", stM, bM)
					}
				} else if stM == http.StatusCreated {
					var cM struct{ ID string }
					_ = json.Unmarshal(bM, &cM)
					recM, _ := teBad.lookup(cM.ID)
					for _, uf := range recM.Spec.UnixForwards {
						if strings.Contains(uf, badCase.path) {
							t.Errorf("Mounts forwarded to non-socket/missing DockerSocket: %v", recM.Spec.UnixForwards)
						}
					}
				}
			})
		}
	})

	// 3. v0.20 fallback (UnixForward=false): keep MountBind behavior unchanged.
	t.Run("v020_fallback", func(t *testing.T) {
		te := newEnv(false, dockerSock)
		st, body := te.do(t, http.MethodPost, "/v1.52/containers/create?name=v20-binds", map[string]any{
			"Image": "alpine:3.20",
			"HostConfig": map[string]any{
				"Binds": []string{
					"/var/run/docker.sock:/var/run/docker.sock",
					customSock + ":/run/custom.sock",
				},
			},
		})
		if st != http.StatusCreated {
			t.Fatalf("create = %d %s", st, body)
		}
		var created struct{ ID string }
		_ = json.Unmarshal(body, &created)
		c, err := te.lookup(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(c.Spec.UnixForwards) != 0 {
			t.Errorf("v0.20 Spec.UnixForwards = %v, want empty", c.Spec.UnixForwards)
		}
		if len(c.Spec.Mounts) != 2 || c.Spec.Mounts[0].Type != sandbox.MountBind || c.Spec.Mounts[1].Type != sandbox.MountBind {
			t.Errorf("v0.20 Spec.Mounts = %+v, want 2 MountBinds", c.Spec.Mounts)
		}
	})
}

func TestStreamStatsBlkioBytesAndOps(t *testing.T) {
	c := Container{
		ID:   fakeHex("blkio-ctr"),
		Name: "blkio-ctr",
	}
	gs := rungv.Stats{
		Timestamp: time.Now().UTC(),
		BlockIO: []struct {
			Disk         string `json:"disk"`
			ReadOps      uint64 `json:"read_ops"`
			WriteOps     uint64 `json:"write_ops"`
			ReadSectors  uint64 `json:"read_sectors"`
			WriteSectors uint64 `json:"write_sectors"`
		}{
			{Disk: "vda", ReadOps: 12, WriteOps: 34, ReadSectors: 100, WriteSectors: 200},
		},
	}

	payload := statsPayloadFromGuest(c, gs, streamCPUStats{}, time.Time{})
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		BlkioStats struct {
			Bytes []streamBlkioEntry `json:"io_service_bytes_recursive"`
			Ops   []streamBlkioEntry `json:"io_serviced_recursive"`
		} `json:"blkio_stats"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}

	wantBytes := []streamBlkioEntry{
		{Major: 254, Minor: 0, Op: "read", Value: 100 * 512},
		{Major: 254, Minor: 0, Op: "write", Value: 200 * 512},
	}
	wantOps := []streamBlkioEntry{
		{Major: 254, Minor: 0, Op: "read", Value: 12},
		{Major: 254, Minor: 0, Op: "write", Value: 34},
	}
	if !slices.Equal(decoded.BlkioStats.Bytes, wantBytes) {
		t.Errorf("io_service_bytes_recursive = %+v, want %+v", decoded.BlkioStats.Bytes, wantBytes)
	}
	if !slices.Equal(decoded.BlkioStats.Ops, wantOps) {
		t.Errorf("io_serviced_recursive = %+v, want %+v", decoded.BlkioStats.Ops, wantOps)
	}

	// Procstat fallback initializes both slices as empty JSON arrays, not null.
	te := newTestEngine(t)
	procPayload := te.streamSampleStatsProcstat(c, streamCPUStats{}, time.Time{})
	procRaw, err := json.Marshal(procPayload)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(procRaw), `"io_service_bytes_recursive":[]`) ||
		!strings.Contains(string(procRaw), `"io_serviced_recursive":[]`) {
		t.Errorf("procstat blkio_stats missing empty arrays: %s", procRaw)
	}
}

func TestAllowLANPortsDefaultAndOptIn(t *testing.T) {
	newPortEnv := func(allowFn func() bool) *testEngine {
		dir := t.TempDir()
		fake := newFakeRunner()
		fake.addImage("nginx:alpine", ImageConfig{
			Cmd:          []string{"nginx"},
			ExposedPorts: map[string]struct{}{"80/tcp": {}, "443/tcp": {}},
		})
		eng := New(Config{
			Dir:           dir,
			Runner:        fake,
			Version:       "0.0.0-test",
			AllowLANPorts: allowFn,
		})
		if err := eng.Open(context.Background()); err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(eng.Handler())
		te := &testEngine{Engine: eng, fake: fake, srv: srv, dir: dir}
		t.Cleanup(func() {
			srv.Close()
			_ = eng.Close(time.Second)
		})
		return te
	}

	// 1. Default (nil callback and false callback) forces 127.0.0.1 and keeps the
	// exact warning for explicit non-wildcard, non-loopback IPs.
	for _, tc := range []struct {
		name string
		fn   func() bool
	}{
		{"nil_callback", nil},
		{"false_callback", func() bool { return false }},
	} {
		t.Run("default_"+tc.name, func(t *testing.T) {
			te := newPortEnv(tc.fn)
			st, created, raw := createTestContainer(t, te, "lan-off", map[string]any{
				"Image": "nginx:alpine",
				"HostConfig": map[string]any{
					"PortBindings": map[string][]PortBinding{
						"80/tcp":   {{HostIP: "192.168.1.5", HostPort: ""}},
						"443/tcp":  {{HostIP: "0.0.0.0", HostPort: ""}},
						"8080/tcp": {{HostIP: "", HostPort: ""}},
						"8443/tcp": {{HostIP: "::", HostPort: ""}},
					},
				},
			})
			if st != http.StatusCreated {
				t.Fatalf("create = %d %s", st, raw)
			}
			if len(created.Warnings) != 1 || created.Warnings[0] != "Port 80/tcp is published on 127.0.0.1 only, not on 192.168.1.5" {
				t.Fatalf("warnings = %v, want single 127.0.0.1 warning for 192.168.1.5", created.Warnings)
			}
			if stStart, bStart := te.do(t, http.MethodPost, "/v1.52/containers/"+created.ID+"/start", nil); stStart != http.StatusNoContent {
				t.Fatalf("start = %d %s", stStart, bStart)
			}
			started := te.fake.Started()
			if len(started) != 1 || len(started[0].Ports) != 4 {
				t.Fatalf("started specs = %+v", started)
			}
			for _, sp := range started[0].Ports {
				if err := sandbox.ValidatePort(sp); err != nil {
					t.Errorf("ValidatePort(%q): %v", sp, err)
				}
				if !strings.HasPrefix(sp, "127.0.0.1:") {
					t.Errorf("spec port %q should be forced to 127.0.0.1", sp)
				}
			}
			rec, _ := te.lookup(created.ID)
			for _, key := range []string{"80/tcp", "443/tcp", "8080/tcp", "8443/tcp"} {
				if len(rec.Ports[key]) != 1 || rec.Ports[key][0].HostIP != "127.0.0.1" {
					t.Errorf("Ports[%s] = %+v, want 127.0.0.1", key, rec.Ports[key])
				}
			}
		})
	}

	// 2. AllowLANPorts=true honors empty -> 0.0.0.0, explicit 0.0.0.0, LAN IP,
	// IPv6 :: / ::1, and keeps explicit 127.0.0.1 loopback with no false warning.
	t.Run("opt_in_enabled", func(t *testing.T) {
		var allowLAN atomic.Bool
		allowLAN.Store(true)
		te := newPortEnv(allowLAN.Load)

		// ValidatePortBindings preserves LAN IP, 0.0.0.0, ::, ::1, 127.0.0.1,
		// and maps empty HostIP to 0.0.0.0 with zero warnings.
		st, cVal, raw := createTestContainer(t, te, "lan-validate", map[string]any{
			"Image": "nginx:alpine",
			"HostConfig": map[string]any{
				"PortBindings": map[string][]PortBinding{
					"80/tcp": {{HostIP: "", HostPort: "18090"}},
					"81/tcp": {{HostIP: "0.0.0.0", HostPort: "18091"}},
					"82/tcp": {{HostIP: "127.0.0.1", HostPort: "18092"}},
					"83/tcp": {{HostIP: "192.168.1.5", HostPort: "18093-18094"}},
					"84/tcp": {{HostIP: "[::]", HostPort: ""}},
					"85/tcp": {{HostIP: "::1", HostPort: ""}},
				},
			},
		})
		if st != http.StatusCreated {
			t.Fatalf("create lan-validate = %d %s", st, raw)
		}
		if len(cVal.Warnings) != 0 {
			t.Fatalf("lan-validate warnings = %v, want none", cVal.Warnings)
		}
		recVal, err := te.lookup(cVal.ID)
		if err != nil {
			t.Fatal(err)
		}
		wantHC := map[string][]PortBinding{
			"80/tcp": {{HostIP: "0.0.0.0", HostPort: "18090"}},
			"81/tcp": {{HostIP: "0.0.0.0", HostPort: "18091"}},
			"82/tcp": {{HostIP: "127.0.0.1", HostPort: "18092"}},
			"83/tcp": {{HostIP: "192.168.1.5", HostPort: "18093"}, {HostIP: "192.168.1.5", HostPort: "18094"}},
			"84/tcp": {{HostIP: "::", HostPort: ""}},
			"85/tcp": {{HostIP: "::1", HostPort: ""}},
		}
		for k, want := range wantHC {
			if !slices.Equal(recVal.HostConfig.PortBindings[k], want) {
				t.Errorf("HostConfig.PortBindings[%s] = %+v, want %+v", k, recVal.HostConfig.PortBindings[k], want)
			}
		}

		// Invalid HostIP literals return 400 when AllowLANPorts is enabled.
		for _, badIP := range []string{"not-an-ip", "fe80::1%en0", "[127.0.0.1]"} {
			stBad, _, bBad := createTestContainer(t, te, "", map[string]any{
				"Image": "nginx:alpine",
				"HostConfig": map[string]any{
					"PortBindings": map[string][]PortBinding{
						"80/tcp": {{HostIP: badIP, HostPort: ""}},
					},
				},
			})
			if stBad != http.StatusBadRequest {
				t.Errorf("bad HostIP %q status = %d %s, want 400", badIP, stBad, bBad)
			}
		}

		// Start with "", "0.0.0.0", and explicit "127.0.0.1" (fixed + dynamic)
		// and verify Spec.Ports, NetworkSettings.Ports, and GET /containers/json.
		lnProbe, err := net.Listen("tcp", "0.0.0.0:0")
		if err != nil {
			t.Fatal(err)
		}
		fixedPort := strconv.Itoa(lnProbe.Addr().(*net.TCPAddr).Port)
		_ = lnProbe.Close()

		st, cRun, raw := createTestContainer(t, te, "lan-run", map[string]any{
			"Image": "nginx:alpine",
			"HostConfig": map[string]any{
				"PortBindings": map[string][]PortBinding{
					"80/tcp":  {{HostIP: "", HostPort: fixedPort}},
					"81/tcp":  {{HostIP: "0.0.0.0", HostPort: ""}},
					"443/tcp": {{HostIP: "127.0.0.1", HostPort: ""}},
				},
			},
		})
		if st != http.StatusCreated {
			t.Fatalf("create lan-run = %d %s", st, raw)
		}
		if len(cRun.Warnings) != 0 {
			t.Fatalf("lan-run warnings = %v, want none", cRun.Warnings)
		}
		if stStart, bStart := te.do(t, http.MethodPost, "/v1.52/containers/"+cRun.ID+"/start", nil); stStart != http.StatusNoContent {
			t.Fatalf("start lan-run = %d %s", stStart, bStart)
		}
		started := te.fake.Started()
		spec := started[len(started)-1]
		for _, sp := range spec.Ports {
			if err := sandbox.ValidatePort(sp); err != nil {
				t.Errorf("ValidatePort(%q): %v", sp, err)
			}
		}

		// Inspect NetworkSettings.Ports.
		stIns, bIns := te.do(t, http.MethodGet, "/v1.52/containers/"+cRun.ID+"/json", nil)
		if stIns != http.StatusOK {
			t.Fatalf("inspect = %d %s", stIns, bIns)
		}
		var ins containerInspectResponse
		if err := json.Unmarshal(bIns, &ins); err != nil {
			t.Fatal(err)
		}
		if got := ins.NetworkSettings.Ports["80/tcp"]; len(got) != 1 || got[0].HostIP != "0.0.0.0" || got[0].HostPort != fixedPort {
			t.Errorf("inspect 80/tcp = %+v, want 0.0.0.0:%s", got, fixedPort)
		}
		if got := ins.NetworkSettings.Ports["81/tcp"]; len(got) != 1 || got[0].HostIP != "0.0.0.0" || got[0].HostPort == "" {
			t.Errorf("inspect 81/tcp = %+v, want 0.0.0.0:<ephemeral>", got)
		}
		if got := ins.NetworkSettings.Ports["443/tcp"]; len(got) != 1 || got[0].HostIP != "127.0.0.1" || got[0].HostPort == "" {
			t.Errorf("inspect 443/tcp = %+v, want 127.0.0.1:<ephemeral>", got)
		}
		wantSpecPorts := []string{
			"127.0.0.1:" + ins.NetworkSettings.Ports["443/tcp"][0].HostPort + ":443/tcp",
			"0.0.0.0:" + fixedPort + ":80/tcp",
			"0.0.0.0:" + ins.NetworkSettings.Ports["81/tcp"][0].HostPort + ":81/tcp",
		}
		if !slices.Equal(spec.Ports, wantSpecPorts) {
			t.Errorf("spec.Ports = %v, want %v", spec.Ports, wantSpecPorts)
		}

		// GET /containers/json reports exact HostIP values.
		stList, bList := te.do(t, http.MethodGet, "/v1.52/containers/json", nil)
		if stList != http.StatusOK {
			t.Fatalf("list = %d %s", stList, bList)
		}
		var rows []containerListItem
		_ = json.Unmarshal(bList, &rows)
		if len(rows) != 1 || len(rows[0].Ports) != 3 {
			t.Fatalf("list rows = %+v", rows)
		}
		ipByPriv := map[uint16]string{}
		for _, p := range rows[0].Ports {
			ipByPriv[p.PrivatePort] = p.IP
		}
		if ipByPriv[80] != "0.0.0.0" || ipByPriv[81] != "0.0.0.0" || ipByPriv[443] != "127.0.0.1" {
			t.Errorf("list Ports IPs = %v, want 80->0.0.0.0 81->0.0.0.0 443->127.0.0.1", ipByPriv)
		}

		// Conflict: another container trying to bind 127.0.0.1:<fixedPort> while
		// lan-run holds 0.0.0.0:<fixedPort> fails cleanly with 500.
		_, cConflict, _ := createTestContainer(t, te, "lan-conflict", map[string]any{
			"Image": "nginx:alpine",
			"HostConfig": map[string]any{
				"PortBindings": map[string][]PortBinding{
					"80/tcp": {{HostIP: "127.0.0.1", HostPort: fixedPort}},
				},
			},
		})
		stConf, bConf := te.do(t, http.MethodPost, "/v1.52/containers/"+cConflict.ID+"/start", nil)
		wantConf := fmt.Sprintf("Bind for 127.0.0.1:%s failed: port is already allocated", fixedPort)
		if stConf != http.StatusInternalServerError || !strings.Contains(string(bConf), wantConf) {
			t.Errorf("conflict start = %d %s, want 500 containing %q", stConf, bConf, wantConf)
		}

		// PublishAllPorts binds 0.0.0.0 when AllowLANPorts is true.
		stAll, cAll, bAll := createTestContainer(t, te, "lan-allports", map[string]any{
			"Image":      "nginx:alpine",
			"HostConfig": map[string]any{"PublishAllPorts": true},
		})
		if stAll != http.StatusCreated {
			t.Fatalf("create PublishAllPorts = %d %s", stAll, bAll)
		}
		if stStart, bStart := te.do(t, http.MethodPost, "/v1.52/containers/"+cAll.ID+"/start", nil); stStart != http.StatusNoContent {
			t.Fatalf("start PublishAllPorts = %d %s", stStart, bStart)
		}
		recAll, _ := te.lookup(cAll.ID)
		for _, key := range []string{"80/tcp", "443/tcp"} {
			if len(recAll.Ports[key]) != 1 || recAll.Ports[key][0].HostIP != "0.0.0.0" {
				t.Errorf("PublishAllPorts Ports[%s] = %+v, want 0.0.0.0", key, recAll.Ports[key])
			}
		}

		// Non-local IP literal fails start with bind error mentioning that IP (never 127.0.0.1).
		_, cBadAddr, _ := createTestContainer(t, te, "lan-unassigned", map[string]any{
			"Image": "nginx:alpine",
			"HostConfig": map[string]any{
				"PortBindings": map[string][]PortBinding{
					"80/tcp": {{HostIP: "198.51.100.1", HostPort: "18099"}},
				},
			},
		})
		stUn, bUn := te.do(t, http.MethodPost, "/v1.52/containers/"+cBadAddr.ID+"/start", nil)
		if stUn != http.StatusInternalServerError || !strings.Contains(string(bUn), "198.51.100.1:18099") {
			t.Errorf("unassigned IP start = %d %s, want 500 mentioning 198.51.100.1:18099", stUn, bUn)
		}

		// Turning AllowLANPorts back off before restarting lan-run forces it back
		// to 127.0.0.1 (fail-closed).
		if stStop, bStop := te.do(t, http.MethodPost, "/v1.52/containers/"+cRun.ID+"/stop?t=1", nil); stStop != http.StatusNoContent {
			t.Fatalf("stop lan-run = %d %s", stStop, bStop)
		}
		allowLAN.Store(false)
		if stStart, bStart := te.do(t, http.MethodPost, "/v1.52/containers/"+cRun.ID+"/start", nil); stStart != http.StatusNoContent {
			t.Fatalf("restart lan-run with LAN off = %d %s", stStart, bStart)
		}
		recOff, _ := te.lookup(cRun.ID)
		for _, key := range []string{"80/tcp", "81/tcp", "443/tcp"} {
			if len(recOff.Ports[key]) != 1 || recOff.Ports[key][0].HostIP != "127.0.0.1" {
				t.Errorf("after toggling AllowLANPorts off, Ports[%s] = %+v, want 127.0.0.1", key, recOff.Ports[key])
			}
		}
		allowLAN.Store(true)

		// IPv6 wildcard (::) and loopback (::1) produce bracketed rungv -p specs
		// when IPv6 sockets are available on this host.
		if ln6, err := net.Listen("tcp", "[::1]:0"); err == nil {
			_ = ln6.Close()
			_, cV6, rawV6 := createTestContainer(t, te, "lan-ipv6", map[string]any{
				"Image": "nginx:alpine",
				"HostConfig": map[string]any{
					"PortBindings": map[string][]PortBinding{
						"80/tcp":  {{HostIP: "::", HostPort: ""}},
						"443/tcp": {{HostIP: "::1", HostPort: ""}},
					},
				},
			})
			if stV6, bV6 := te.do(t, http.MethodPost, "/v1.52/containers/"+cV6.ID+"/start", nil); stV6 != http.StatusNoContent {
				t.Fatalf("start ipv6 = %d %s (create=%s)", stV6, bV6, rawV6)
			}
			sp6 := te.fake.Started()[len(te.fake.Started())-1]
			rec6, _ := te.lookup(cV6.ID)
			if got := rec6.Ports["80/tcp"]; len(got) != 1 || got[0].HostIP != "::" {
				t.Errorf("ipv6 80/tcp = %+v, want ::", got)
			}
			if got := rec6.Ports["443/tcp"]; len(got) != 1 || got[0].HostIP != "::1" {
				t.Errorf("ipv6 443/tcp = %+v, want ::1", got)
			}
			want6 := []string{
				"[::1]:" + rec6.Ports["443/tcp"][0].HostPort + ":443/tcp",
				"[::]:" + rec6.Ports["80/tcp"][0].HostPort + ":80/tcp",
			}
			if !slices.Equal(sp6.Ports, want6) {
				t.Errorf("ipv6 spec.Ports = %v, want %v", sp6.Ports, want6)
			}
			for _, p := range sp6.Ports {
				if err := sandbox.ValidatePort(p); err != nil {
					t.Errorf("ValidatePort(%q): %v", p, err)
				}
			}
		}

		// Real local non-loopback IPv4 (if the host has one) binds and formats cleanly.
		if lanIP := findLocalNonLoopbackIPv4(); lanIP != "" {
			stLAN, cLAN, bLAN := createTestContainer(t, te, "lan-host-ip", map[string]any{
				"Image": "nginx:alpine",
				"HostConfig": map[string]any{
					"PortBindings": map[string][]PortBinding{
						"80/tcp": {{HostIP: lanIP, HostPort: ""}},
					},
				},
			})
			if stLAN != http.StatusCreated {
				t.Fatalf("create lan-host-ip = %d %s", stLAN, bLAN)
			}
			if len(cLAN.Warnings) != 0 {
				t.Errorf("lan-host-ip warnings = %v, want none", cLAN.Warnings)
			}
			if stStart, bStart := te.do(t, http.MethodPost, "/v1.52/containers/"+cLAN.ID+"/start", nil); stStart != http.StatusNoContent {
				t.Fatalf("start lan-host-ip (%s) = %d %s", lanIP, stStart, bStart)
			}
			recLAN, _ := te.lookup(cLAN.ID)
			spLAN := te.fake.Started()[len(te.fake.Started())-1]
			if got := recLAN.Ports["80/tcp"]; len(got) != 1 || got[0].HostIP != lanIP || got[0].HostPort == "" {
				t.Errorf("lan-host-ip Ports = %+v, want %s:<port>", got, lanIP)
			} else {
				wantSpec := lanIP + ":" + got[0].HostPort + ":80/tcp"
				if !slices.Equal(spLAN.Ports, []string{wantSpec}) {
					t.Errorf("lan-host-ip spec.Ports = %v, want [%s]", spLAN.Ports, wantSpec)
				}
				if err := sandbox.ValidatePort(wantSpec); err != nil {
					t.Errorf("ValidatePort(%q): %v", wantSpec, err)
				}
			}
		}
	})

	// 3. Port overlap matrix across IPv4/IPv6 unicast and wildcard bindings.
	t.Run("port_conflict_matrix", func(t *testing.T) {
		bind := func(ip string, port int, proto string) hostPortBind {
			return hostPortBind{ip: netip.MustParseAddr(ip), port: port, proto: proto}
		}
		cases := []struct {
			name string
			a, b hostPortBind
			want bool
		}{
			{"same_loopback", bind("127.0.0.1", 8080, "tcp"), bind("127.0.0.1", 8080, "tcp"), true},
			{"wildcard4_vs_loopback4", bind("0.0.0.0", 8080, "tcp"), bind("127.0.0.1", 8080, "tcp"), true},
			{"wildcard4_vs_lan4", bind("0.0.0.0", 8080, "tcp"), bind("192.168.1.5", 8080, "tcp"), true},
			{"distinct_unicast4", bind("127.0.0.1", 8080, "tcp"), bind("192.168.1.5", 8080, "tcp"), false},
			{"loopback4_vs_loopback6", bind("127.0.0.1", 8080, "tcp"), bind("::1", 8080, "tcp"), false},
			{"wildcard6_vs_loopback6", bind("::", 8080, "tcp"), bind("::1", 8080, "tcp"), true},
			{"wildcard6_vs_wildcard4", bind("::", 8080, "tcp"), bind("0.0.0.0", 8080, "tcp"), true},
			{"wildcard4_vs_loopback6", bind("0.0.0.0", 8080, "tcp"), bind("::1", 8080, "tcp"), false},
			{"tcp_vs_udp", bind("0.0.0.0", 8080, "tcp"), bind("0.0.0.0", 8080, "udp"), false},
			{"diff_port", bind("0.0.0.0", 8080, "tcp"), bind("0.0.0.0", 8081, "tcp"), false},
		}
		for _, tc := range cases {
			if got := tc.a.conflicts(tc.b); got != tc.want {
				t.Errorf("%s: (%v).conflicts(%v) = %v, want %v", tc.name, tc.a, tc.b, got, tc.want)
			}
			if got := tc.b.conflicts(tc.a); got != tc.want {
				t.Errorf("%s (sym): (%v).conflicts(%v) = %v, want %v", tc.name, tc.b, tc.a, got, tc.want)
			}
		}
	})
}

func findLocalNonLoopbackIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		v4 := ipNet.IP.To4()
		if v4 == nil || v4.IsLoopback() || v4.IsLinkLocalUnicast() || v4.IsUnspecified() {
			continue
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(v4.String(), "0"))
		if err == nil {
			_ = ln.Close()
			return v4.String()
		}
	}
	return ""
}

func TestSSHAuthSocketForwarding(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix domain socket mode check is POSIX-specific")
	}
	sockDir, err := os.MkdirTemp("", "gvxd-ssh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })

	sshSock := filepath.Join(sockDir, "agent.sock")
	lnSSH, err := net.Listen("unix", sshSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lnSSH.Close() })

	symlinkSSH := filepath.Join(sockDir, "link.sock")
	if err := os.Symlink(sshSock, symlinkSSH); err != nil {
		t.Fatal(err)
	}

	regularFile := filepath.Join(sockDir, "not-a-socket")
	if err := os.WriteFile(regularFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	newEnv := func(unixForward bool, sshFn func() string) *testEngine {
		dir := t.TempDir()
		fake := newFakeRunner()
		fake.features = rungv.Features{UnixForward: unixForward}
		fake.addImage("alpine:3.20", ImageConfig{Cmd: []string{"/bin/sh"}})
		eng := New(Config{
			Dir:         dir,
			Runner:      fake,
			Version:     "0.0.0-test",
			SSHAuthSock: sshFn,
		})
		if err := eng.Open(context.Background()); err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(eng.Handler())
		te := &testEngine{Engine: eng, fake: fake, srv: srv, dir: dir}
		t.Cleanup(func() {
			srv.Close()
			_ = eng.Close(time.Second)
		})
		return te
	}

	// 1. Real Unix socket via HostConfig.Binds and HostConfig.Mounts (including
	// redundant slashes and canonical Docker Desktop target path).
	t.Run("v021_real_socket", func(t *testing.T) {
		te := newEnv(true, func() string { return sshSock })

		stB, bB := te.do(t, http.MethodPost, "/v1.52/containers/create?name=ssh-bind", map[string]any{
			"Image": "alpine:3.20",
			"HostConfig": map[string]any{
				"Binds": []string{
					"/run/host-services/ssh-auth.sock:/ssh-agent",
					"/run//host-services/ssh-auth.sock:/run/host-services/ssh-auth.sock",
				},
			},
		})
		if stB != http.StatusCreated {
			t.Fatalf("create Binds = %d %s", stB, bB)
		}
		var cB struct{ ID string }
		_ = json.Unmarshal(bB, &cB)
		if stStart, bStart := te.do(t, http.MethodPost, "/v1.52/containers/"+cB.ID+"/start", nil); stStart != http.StatusNoContent {
			t.Fatalf("start Binds = %d %s", stStart, bStart)
		}
		spB := te.fake.Started()[len(te.fake.Started())-1]
		wantUF := []string{
			"/ssh-agent=" + sshSock,
			"/run/host-services/ssh-auth.sock=" + sshSock,
		}
		if !slices.Equal(spB.UnixForwards, wantUF) {
			t.Errorf("Binds UnixForwards = %v, want %v", spB.UnixForwards, wantUF)
		}
		if len(spB.Mounts) != 0 {
			t.Errorf("Binds Spec.Mounts = %+v, want empty", spB.Mounts)
		}
		recB, _ := te.lookup(cB.ID)
		if len(recB.Mounts) != 2 || recB.Mounts[0].Source != "/run/host-services/ssh-auth.sock" || recB.Mounts[0].Destination != "/ssh-agent" {
			t.Errorf("Binds inspect Mounts = %+v", recB.Mounts)
		}

		// HostConfig.Mounts type=bind stats the resolved SSH agent socket instead of
		// failing on the non-existent virtual /run/host-services/ssh-auth.sock path.
		stM, bM := te.do(t, http.MethodPost, "/v1.52/containers/create?name=ssh-mount", map[string]any{
			"Image": "alpine:3.20",
			"HostConfig": map[string]any{
				"Mounts": []map[string]any{
					{"Type": "bind", "Source": "/run/host-services/ssh-auth.sock", "Target": "/ssh-agent"},
				},
			},
		})
		if stM != http.StatusCreated {
			t.Fatalf("create Mounts = %d %s", stM, bM)
		}
		var cM struct{ ID string }
		_ = json.Unmarshal(bM, &cM)
		recM, err := te.lookup(cM.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(recM.Spec.UnixForwards, []string{"/ssh-agent=" + sshSock}) {
			t.Errorf("Mounts UnixForwards = %v, want [/ssh-agent=%s]", recM.Spec.UnixForwards, sshSock)
		}
		if len(recM.Spec.Mounts) != 0 {
			t.Errorf("Mounts Spec.Mounts = %+v, want empty", recM.Spec.Mounts)
		}
		if len(recM.Mounts) != 1 || recM.Mounts[0].Source != "/run/host-services/ssh-auth.sock" || recM.Mounts[0].Destination != "/ssh-agent" {
			t.Errorf("Mounts Container.Mounts = %+v", recM.Mounts)
		}
	})

	// 2. Symlinked SSH agent socket follows symlinks via os.Stat for both Binds and Mounts.
	t.Run("v021_symlink_socket", func(t *testing.T) {
		te := newEnv(true, func() string { return symlinkSSH })

		stB, bB := te.do(t, http.MethodPost, "/v1.52/containers/create?name=ssh-sym-bind", map[string]any{
			"Image": "alpine:3.20",
			"HostConfig": map[string]any{
				"Binds": []string{"/run/host-services/ssh-auth.sock:/ssh-agent"},
			},
		})
		if stB != http.StatusCreated {
			t.Fatalf("create symlink Binds = %d %s", stB, bB)
		}
		var cB struct{ ID string }
		_ = json.Unmarshal(bB, &cB)
		recB, _ := te.lookup(cB.ID)
		if !slices.Equal(recB.Spec.UnixForwards, []string{"/ssh-agent=" + symlinkSSH}) {
			t.Errorf("symlink Binds UnixForwards = %v, want [/ssh-agent=%s]", recB.Spec.UnixForwards, symlinkSSH)
		}

		stM, bM := te.do(t, http.MethodPost, "/v1.52/containers/create?name=ssh-sym-mount", map[string]any{
			"Image": "alpine:3.20",
			"HostConfig": map[string]any{
				"Mounts": []map[string]any{
					{"Type": "bind", "Source": "/run/host-services/ssh-auth.sock", "Target": "/ssh-agent"},
				},
			},
		})
		if stM != http.StatusCreated {
			t.Fatalf("create symlink Mounts = %d %s", stM, bM)
		}
		var cM struct{ ID string }
		_ = json.Unmarshal(bM, &cM)
		recM, _ := te.lookup(cM.ID)
		if !slices.Equal(recM.Spec.UnixForwards, []string{"/ssh-agent=" + symlinkSSH}) {
			t.Errorf("symlink Mounts UnixForwards = %v, want [/ssh-agent=%s]", recM.Spec.UnixForwards, symlinkSSH)
		}
	})

	// 3. Nil SSHAuthSock callback falls back to os.Getenv("SSH_AUTH_SOCK");
	// explicit callback returning "" overrides env and disables forwarding.
	t.Run("v021_env_fallback_and_disable_override", func(t *testing.T) {
		t.Setenv("SSH_AUTH_SOCK", sshSock)

		teEnv := newEnv(true, nil)
		stE, bE := teEnv.do(t, http.MethodPost, "/v1.52/containers/create?name=ssh-env", map[string]any{
			"Image": "alpine:3.20",
			"HostConfig": map[string]any{
				"Mounts": []map[string]any{
					{"Type": "bind", "Source": "/run/host-services/ssh-auth.sock", "Target": "/ssh-agent"},
				},
			},
		})
		if stE != http.StatusCreated {
			t.Fatalf("create env fallback = %d %s", stE, bE)
		}
		var cE struct{ ID string }
		_ = json.Unmarshal(bE, &cE)
		recE, _ := teEnv.lookup(cE.ID)
		if !slices.Equal(recE.Spec.UnixForwards, []string{"/ssh-agent=" + sshSock}) {
			t.Errorf("env fallback UnixForwards = %v, want [/ssh-agent=%s]", recE.Spec.UnixForwards, sshSock)
		}

		// Callback returning "" disables forwarding even when SSH_AUTH_SOCK is set in env.
		teDis := newEnv(true, func() string { return "" })
		stDB, bDB := teDis.do(t, http.MethodPost, "/v1.52/containers/create?name=ssh-dis-bind", map[string]any{
			"Image": "alpine:3.20",
			"HostConfig": map[string]any{
				"Binds": []string{"/run/host-services/ssh-auth.sock:/ssh-agent"},
			},
		})
		if stDB != http.StatusCreated {
			t.Fatalf("create disabled Binds = %d %s", stDB, bDB)
		}
		var cDB struct{ ID string }
		_ = json.Unmarshal(bDB, &cDB)
		recDB, _ := teDis.lookup(cDB.ID)
		for _, uf := range recDB.Spec.UnixForwards {
			if strings.Contains(uf, sshSock) {
				t.Errorf("disabled callback still forwarded to env SSH_AUTH_SOCK: %v", recDB.Spec.UnixForwards)
			}
		}
		if !isUnixSocket(sshAuthSocketSource) {
			if len(recDB.Spec.UnixForwards) != 0 {
				t.Errorf("disabled Binds UnixForwards = %v, want empty", recDB.Spec.UnixForwards)
			}
			if len(recDB.Spec.Mounts) != 1 || recDB.Spec.Mounts[0].Type != sandbox.MountBind {
				t.Errorf("disabled Binds Spec.Mounts = %+v, want fallback MountBind", recDB.Spec.Mounts)
			}
		}
	})

	// 4. Missing, empty, or non-socket SSHAuthSock does not forward and preserves clean fallback/400.
	for _, bad := range []struct {
		name string
		path string
	}{
		{"empty", ""},
		{"missing", filepath.Join(sockDir, "does-not-exist.sock")},
		{"regular_file", regularFile},
	} {
		t.Run("v021_bad_"+bad.name, func(t *testing.T) {
			te := newEnv(true, func() string { return bad.path })

			stB, bB := te.do(t, http.MethodPost, "/v1.52/containers/create?name=ssh-bad-bind", map[string]any{
				"Image": "alpine:3.20",
				"HostConfig": map[string]any{
					"Binds": []string{"/run/host-services/ssh-auth.sock:/ssh-agent"},
				},
			})
			if stB != http.StatusCreated {
				t.Fatalf("create Binds = %d %s", stB, bB)
			}
			var cB struct{ ID string }
			_ = json.Unmarshal(bB, &cB)
			recB, _ := te.lookup(cB.ID)
			if !isUnixSocket(sshAuthSocketSource) {
				if len(recB.Spec.UnixForwards) != 0 {
					t.Errorf("Binds UnixForwards = %v, want empty", recB.Spec.UnixForwards)
				}
				if len(recB.Spec.Mounts) != 1 || recB.Spec.Mounts[0].Type != sandbox.MountBind {
					t.Errorf("Binds Spec.Mounts = %+v, want MountBind fallback", recB.Spec.Mounts)
				}
			}

			stM, bM := te.do(t, http.MethodPost, "/v1.52/containers/create?name=ssh-bad-mount", map[string]any{
				"Image": "alpine:3.20",
				"HostConfig": map[string]any{
					"Mounts": []map[string]any{
						{"Type": "bind", "Source": "/run/host-services/ssh-auth.sock", "Target": "/ssh-agent"},
					},
				},
			})
			if _, err := os.Stat(sshAuthSocketSource); err != nil {
				if stM != http.StatusBadRequest {
					t.Errorf("Mounts bind without host %s = %d %s, want 400", sshAuthSocketSource, stM, bM)
				}
			}
		})
	}

	// 5. Features.UnixForward=false preserves normal non-forwarded path behavior.
	t.Run("v020_unix_forward_false", func(t *testing.T) {
		te := newEnv(false, func() string { return sshSock })

		stB, bB := te.do(t, http.MethodPost, "/v1.52/containers/create?name=ssh-v20-bind", map[string]any{
			"Image": "alpine:3.20",
			"HostConfig": map[string]any{
				"Binds": []string{"/run/host-services/ssh-auth.sock:/ssh-agent"},
			},
		})
		if stB != http.StatusCreated {
			t.Fatalf("v0.20 create Binds = %d %s", stB, bB)
		}
		var cB struct{ ID string }
		_ = json.Unmarshal(bB, &cB)
		recB, _ := te.lookup(cB.ID)
		if len(recB.Spec.UnixForwards) != 0 {
			t.Errorf("v0.20 Binds UnixForwards = %v, want empty", recB.Spec.UnixForwards)
		}
		if len(recB.Spec.Mounts) != 1 || recB.Spec.Mounts[0].Type != sandbox.MountBind || recB.Spec.Mounts[0].Source != "/run/host-services/ssh-auth.sock" {
			t.Errorf("v0.20 Binds Spec.Mounts = %+v, want MountBind", recB.Spec.Mounts)
		}

		stM, bM := te.do(t, http.MethodPost, "/v1.52/containers/create?name=ssh-v20-mount", map[string]any{
			"Image": "alpine:3.20",
			"HostConfig": map[string]any{
				"Mounts": []map[string]any{
					{"Type": "bind", "Source": "/run/host-services/ssh-auth.sock", "Target": "/ssh-agent"},
				},
			},
		})
		if _, err := os.Stat(sshAuthSocketSource); err != nil {
			if stM != http.StatusBadRequest {
				t.Errorf("v0.20 Mounts bind = %d %s, want 400", stM, bM)
			}
		}
	})
}
