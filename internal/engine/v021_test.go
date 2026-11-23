package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
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
		}
		if !slices.Equal(spec.UnixForwards, wantUF) {
			t.Errorf("spec.UnixForwards = %v, want %v", spec.UnixForwards, wantUF)
		}
		// Only the regular directory bind should remain in spec.Mounts.
		if len(spec.Mounts) != 1 || spec.Mounts[0].Type != sandbox.MountBind || spec.Mounts[0].Target != "/data" {
			t.Errorf("spec.Mounts = %+v, want only /data bind", spec.Mounts)
		}

		// docker inspect still reports all 3 binds in Mounts.
		stIns, bIns := te.do(t, http.MethodGet, "/v1.52/containers/"+created.ID+"/json", nil)
		if stIns != http.StatusOK {
			t.Fatalf("inspect = %d %s", stIns, bIns)
		}
		var ins struct {
			Mounts []MountPoint `json:"Mounts"`
		}
		_ = json.Unmarshal(bIns, &ins)
		if len(ins.Mounts) != 3 {
			t.Fatalf("inspect Mounts = %+v, want 3 entries", ins.Mounts)
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

		// Missing DockerSocket path causes HostConfig.Mounts bind to return 400.
		teMissing := newEnv(true, filepath.Join(sockDir, "does-not-exist.sock"))
		stBad, bBad := teMissing.do(t, http.MethodPost, "/v1.52/containers/create?name=uf-missing", map[string]any{
			"Image": "alpine:3.20",
			"HostConfig": map[string]any{
				"Mounts": []map[string]any{
					{"Type": "bind", "Source": "/var/run/docker.sock", "Target": "/var/run/docker.sock"},
				},
			},
		})
		if stBad != http.StatusBadRequest {
			t.Errorf("missing DockerSocket Mounts bind = %d %s, want 400", stBad, bBad)
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
