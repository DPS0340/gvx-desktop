//go:build darwin && arm64

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop-vm/vmassets"
	"github.com/ironpark/gvx-desktop-vm/vmclient"
	"github.com/ironpark/gvx-desktop-vm/vmhelper"
	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// TestMicroVMRealE2E exercises the real Apple Silicon Virtualization.framework
// micro-VM container runtime end-to-end (arm64 --runtime=microvm native without
// Rosetta, amd64 automatic Rosetta, TCP port forwarding, logs, exec, stop, rm).
//
// Opt-in only:
//
//	GVX_MICROVM_TEST=1 GVX_VM_ASSETS=/tmp/gvxd-vm-assets-v2 go test -v -run TestMicroVMRealE2E ./internal/engine
func TestMicroVMRealE2E(t *testing.T) {
	if testing.Short() || os.Getenv("GVX_MICROVM_TEST") != "1" {
		t.Skip("set GVX_MICROVM_TEST=1 and drop -short to run real Apple Silicon micro-VM E2E")
	}
	assetsOverride := strings.TrimSpace(os.Getenv(vmassets.EnvOverride))
	if assetsOverride == "" {
		t.Skipf("set %s to a vm-assets-v2 directory", vmassets.EnvOverride)
	}
	rungvPath, err := rungv.Locate(os.Getenv("GVX_RUNGV"))
	if err != nil {
		t.Skip("rungv not found")
	}

	helperBin := strings.TrimSpace(os.Getenv("GVX_VM_HELPER"))
	if helperBin == "" {
		helperBin = buildSignedVMHelper(t)
	}

	root, err := os.MkdirTemp("/tmp", "gvxd-mvm-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	cacheDir := filepath.Join(root, "cache")
	dataDir := filepath.Join(root, "data")
	for _, d := range []string{cacheDir, dataDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	rvClient := &rungv.Client{Path: rungvPath, CacheDir: cacheDir}
	e := New(Config{
		Dir:     dataDir,
		Runner:  NewRunner(func() *rungv.Client { return rvClient }, dataDir),
		Version: "0.0.0-e2e",
		Logf:    func(f string, a ...any) { t.Logf(f, a...) },
	})
	if err := e.Open(context.Background()); err != nil {
		t.Fatalf("Engine.Open: %v", err)
	}
	t.Cleanup(func() { _ = e.Close(10 * time.Second) })

	if err := e.SetMicroVM(&MicroVMConfig{
		Launcher: vmclient.Launcher{
			Path: helperBin,
			Args: []string{vmhelper.CmdServe},
			Log:  func(line string) { t.Logf("[vm-helper] %s", line) },
		},
		Assets: func(ctx context.Context, progress func(done, total int64)) (vmassets.Dir, error) {
			return vmassets.Resolve(ctx, vmassets.Options{
				Override: assetsOverride,
				Progress: progress,
			})
		},
	}); err != nil {
		t.Fatalf("SetMicroVM: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: e.Handler()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	baseURL := "http://" + ln.Addr().String()

	doJSON := func(method, p string, body any) (int, []byte) {
		t.Helper()
		var r io.Reader
		if body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			r = strings.NewReader(string(b))
		}
		req, err := http.NewRequest(method, baseURL+p, r)
		if err != nil {
			t.Fatal(err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, p, err)
		}
		defer res.Body.Close()
		out, _ := io.ReadAll(res.Body)
		return res.StatusCode, out
	}

	waitExited := func(id string) containerInspectResponse {
		t.Helper()
		deadline := time.Now().Add(45 * time.Second)
		for time.Now().Before(deadline) {
			st, b := doJSON(http.MethodGet, "/containers/"+id+"/json", nil)
			if st == http.StatusOK {
				var ins containerInspectResponse
				if json.Unmarshal(b, &ins) == nil && ins.State.Status == StatusExited {
					return ins
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		_, b := doJSON(http.MethodGet, "/containers/"+id+"/json", nil)
		t.Fatalf("timed out waiting for %s to exit: %s", id, b)
		return containerInspectResponse{}
	}

	// Pull alpine:3.20 for both arm64 and amd64 into temp cache.
	for _, plat := range []string{"linux/arm64", "linux/amd64"} {
		st, b := doJSON(http.MethodPost, "/images/create?fromImage=alpine&tag=3.20&platform="+plat, nil)
		if st != http.StatusOK {
			t.Fatalf("pull alpine:3.20 (%s) = %d %s", plat, st, b)
		}
	}

	t.Run("arm64_native_microvm", func(t *testing.T) {
		st, b := doJSON(http.MethodPost, "/containers/create?name=e2e-arm64", map[string]any{
			"Image":      "alpine:3.20",
			"Cmd":        []string{"uname", "-rm"},
			"HostConfig": map[string]any{"Runtime": "microvm"},
		})
		if st != http.StatusCreated {
			t.Fatalf("create = %d %s", st, b)
		}
		var cr containerCreateResponse
		_ = json.Unmarshal(b, &cr)
		if st, b := doJSON(http.MethodPost, "/containers/"+cr.ID+"/start", nil); st != http.StatusNoContent {
			t.Fatalf("start = %d %s", st, b)
		}
		ins := waitExited(cr.ID)
		if ins.State.ExitCode != 0 {
			t.Fatalf("exit code = %d", ins.State.ExitCode)
		}
		_, lb := doJSON(http.MethodGet, "/containers/"+cr.ID+"/logs?stdout=1&stderr=1", nil)
		out := string(lb)
		t.Logf("arm64 uname -rm: %q", out)
		if !strings.Contains(out, "aarch64") || !strings.Contains(out, "6.18.") {
			t.Fatalf("unexpected arm64 uname -rm: %q", out)
		}
		if st, b := doJSON(http.MethodDelete, "/containers/"+cr.ID, nil); st != http.StatusNoContent {
			t.Fatalf("delete = %d %s", st, b)
		}
	})

	t.Run("amd64_auto_rosetta", func(t *testing.T) {
		st, b := doJSON(http.MethodPost, "/containers/create?name=e2e-amd64&platform=linux/amd64", map[string]any{
			"Image": "alpine:3.20",
			"Cmd":   []string{"uname", "-m"},
		})
		if st != http.StatusCreated {
			t.Fatalf("create = %d %s", st, b)
		}
		var cr containerCreateResponse
		_ = json.Unmarshal(b, &cr)
		if st, b := doJSON(http.MethodPost, "/containers/"+cr.ID+"/start", nil); st != http.StatusNoContent {
			t.Fatalf("start = %d %s", st, b)
		}
		ins := waitExited(cr.ID)
		if ins.State.ExitCode != 0 {
			t.Fatalf("exit code = %d", ins.State.ExitCode)
		}
		_, lb := doJSON(http.MethodGet, "/containers/"+cr.ID+"/logs?stdout=1&stderr=1", nil)
		out := string(lb)
		t.Logf("amd64 uname -m: %q", out)
		if !strings.Contains(out, "x86_64") {
			t.Fatalf("unexpected amd64 uname -m: %q", out)
		}
		if st, b := doJSON(http.MethodDelete, "/containers/"+cr.ID, nil); st != http.StatusNoContent {
			t.Fatalf("delete = %d %s", st, b)
		}
	})

	t.Run("tcp_forward_exec_stop_rm", func(t *testing.T) {
		st, b := doJSON(http.MethodPost, "/containers/create?name=e2e-svc", map[string]any{
			"Image": "alpine:3.20",
			"Cmd": []string{
				"/bin/sh", "-c",
				`mkdir -p /www && echo hello-microvm > /www/index.html && while true; do nc -l -p 8080 -e /bin/sh -c 'printf "HTTP/1.1 200 OK\r\nContent-Length: 14\r\nConnection: close\r\n\r\nhello-microvm\n"'; done`,
			},
			"HostConfig": map[string]any{
				"Runtime": "microvm",
				"PortBindings": map[string]any{
					"8080/tcp": []map[string]string{{"HostIp": "127.0.0.1", "HostPort": ""}},
				},
			},
		})
		if st != http.StatusCreated {
			t.Fatalf("create = %d %s", st, b)
		}
		var cr containerCreateResponse
		_ = json.Unmarshal(b, &cr)
		if st, b := doJSON(http.MethodPost, "/containers/"+cr.ID+"/start", nil); st != http.StatusNoContent {
			t.Fatalf("start = %d %s", st, b)
		}

		_, ib := doJSON(http.MethodGet, "/containers/"+cr.ID+"/json", nil)
		var ins containerInspectResponse
		_ = json.Unmarshal(ib, &ins)
		binds := ins.NetworkSettings.Ports["8080/tcp"]
		if len(binds) != 1 || binds[0].HostPort == "" {
			t.Fatalf("no bound port: %+v", ins.NetworkSettings.Ports)
		}

		// Poll forwarded HTTP endpoint until nc listener is ready.
		url := fmt.Sprintf("http://%s:%s/index.html", binds[0].HostIP, binds[0].HostPort)
		var gotBody string
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			res, err := http.Get(url)
			if err == nil {
				rb, _ := io.ReadAll(res.Body)
				_ = res.Body.Close()
				if res.StatusCode == http.StatusOK {
					gotBody = strings.TrimSpace(string(rb))
					break
				}
			}
			time.Sleep(150 * time.Millisecond)
		}
		if gotBody != "hello-microvm" {
			_, logs := doJSON(http.MethodGet, "/containers/"+cr.ID+"/logs?stdout=1&stderr=1", nil)
			_, cur := doJSON(http.MethodGet, "/containers/"+cr.ID+"/json", nil)
			t.Fatalf("forwarded HTTP %s = %q, want hello-microvm\nlogs=%q\ninspect=%s", url, gotBody, string(logs), string(cur))
		}

		// Exec, top, stats, pause/unpause inside running micro-VM.
		st, b = doJSON(http.MethodPost, "/containers/"+cr.ID+"/exec", map[string]any{
			"Cmd":          []string{"cat", "/www/index.html"},
			"AttachStdout": true,
		})
		if st != http.StatusCreated {
			t.Fatalf("exec create = %d %s", st, b)
		}
		var ex struct{ ID string }
		_ = json.Unmarshal(b, &ex)
		if st, b := doJSON(http.MethodPost, "/exec/"+ex.ID+"/start", map[string]any{"Detach": true}); st != http.StatusOK {
			t.Fatalf("exec start = %d %s", st, b)
		}
		exDeadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(exDeadline) {
			_, eb := doJSON(http.MethodGet, "/exec/"+ex.ID+"/json", nil)
			var exIns execInspectResponse
			if json.Unmarshal(eb, &exIns) == nil && !exIns.Running && exIns.ExitCode != nil {
				if *exIns.ExitCode != 0 {
					t.Fatalf("exec exit code = %d", *exIns.ExitCode)
				}
				break
			}
			time.Sleep(50 * time.Millisecond)
		}

		if st, b := doJSON(http.MethodGet, "/containers/"+cr.ID+"/top", nil); st != http.StatusOK || !strings.Contains(string(b), "sh") {
			t.Fatalf("top = %d %s", st, b)
		}
		if st, b := doJSON(http.MethodGet, "/containers/"+cr.ID+"/stats?stream=0&one-shot=1", nil); st != http.StatusOK || !strings.Contains(string(b), "memory_stats") {
			t.Fatalf("stats = %d %s", st, b)
		}
		if st, b := doJSON(http.MethodPost, "/containers/"+cr.ID+"/pause", nil); st != http.StatusNoContent {
			t.Fatalf("pause = %d %s", st, b)
		}
		if st, b := doJSON(http.MethodPost, "/containers/"+cr.ID+"/unpause", nil); st != http.StatusNoContent {
			t.Fatalf("unpause = %d %s", st, b)
		}

		// Stop & remove cleanly.
		if st, b := doJSON(http.MethodPost, "/containers/"+cr.ID+"/stop?t=5", nil); st != http.StatusNoContent {
			t.Fatalf("stop = %d %s", st, b)
		}
		waitExited(cr.ID)
		if st, b := doJSON(http.MethodDelete, "/containers/"+cr.ID, nil); st != http.StatusNoContent {
			t.Fatalf("delete = %d %s", st, b)
		}
		if rv := e.MicroVMState().RunningVMs; rv != 0 {
			t.Fatalf("RunningVMs = %d after rm, want 0", rv)
		}
	})

	t.Run("named_volume_flock_and_persistence", func(t *testing.T) {
		if st, b := doJSON(http.MethodPost, "/volumes/create", map[string]any{"Name": "e2e-mvm-vol"}); st != http.StatusCreated {
			t.Fatalf("volume create = %d %s", st, b)
		}
		st, b := doJSON(http.MethodPost, "/containers/create?name=e2e-vol-w", map[string]any{
			"Image": "alpine:3.20",
			"Cmd":   []string{"/bin/sh", "-c", "echo persisted-vol > /data/v.txt"},
			"HostConfig": map[string]any{
				"Runtime": "microvm",
				"Binds":   []string{"e2e-mvm-vol:/data"},
			},
		})
		if st != http.StatusCreated {
			t.Fatalf("create vol writer = %d %s", st, b)
		}
		var cw containerCreateResponse
		_ = json.Unmarshal(b, &cw)
		if st, b := doJSON(http.MethodPost, "/containers/"+cw.ID+"/start", nil); st != http.StatusNoContent {
			t.Fatalf("start vol writer = %d %s", st, b)
		}
		if ins := waitExited(cw.ID); ins.State.ExitCode != 0 {
			_, logs := doJSON(http.MethodGet, "/containers/"+cw.ID+"/logs?stdout=1&stderr=1", nil)
			t.Fatalf("vol writer exit=%d logs=%q", ins.State.ExitCode, string(logs))
		}
		_, _ = doJSON(http.MethodDelete, "/containers/"+cw.ID, nil)

		st, b = doJSON(http.MethodPost, "/containers/create?name=e2e-vol-r", map[string]any{
			"Image": "alpine:3.20",
			"Cmd":   []string{"cat", "/data/v.txt"},
			"HostConfig": map[string]any{
				"Runtime": "microvm",
				"Binds":   []string{"e2e-mvm-vol:/data:ro"},
			},
		})
		if st != http.StatusCreated {
			t.Fatalf("create vol reader = %d %s", st, b)
		}
		var cr containerCreateResponse
		_ = json.Unmarshal(b, &cr)
		if st, b := doJSON(http.MethodPost, "/containers/"+cr.ID+"/start", nil); st != http.StatusNoContent {
			t.Fatalf("start vol reader = %d %s", st, b)
		}
		if ins := waitExited(cr.ID); ins.State.ExitCode != 0 {
			t.Fatalf("vol reader exit=%d", ins.State.ExitCode)
		}
		_, lb := doJSON(http.MethodGet, "/containers/"+cr.ID+"/logs?stdout=1&stderr=1", nil)
		if !strings.Contains(string(lb), "persisted-vol") {
			t.Fatalf("vol reader logs = %q, want persisted-vol", string(lb))
		}
		_, _ = doJSON(http.MethodDelete, "/containers/"+cr.ID, nil)
	})
}
