package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
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

	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

func TestRenameUpdatesNetworkDNSNames(t *testing.T) {
	fake := newFakeRunner()
	fake.addImage("alpine:3.20", ImageConfig{Cmd: []string{"/bin/sh"}})
	te := reopenTestEngine(t, t.TempDir(), fake)

	if st, body := te.do(t, http.MethodPost, "/networks/create", map[string]any{"Name": "ren-net"}); st != http.StatusCreated {
		t.Fatalf("network create = %d %s", st, body)
	}
	st, body := te.do(t, http.MethodPost, "/containers/create?name=old-svc", map[string]any{
		"Image":      "alpine:3.20",
		"HostConfig": map[string]any{"NetworkMode": "ren-net"},
		"NetworkingConfig": map[string]any{
			"EndpointsConfig": map[string]any{
				"ren-net": map[string]any{"Aliases": []string{"alias1"}},
			},
		},
	})
	if st != http.StatusCreated {
		t.Fatalf("create = %d %s", st, body)
	}

	if st, body := te.do(t, http.MethodPost, "/containers/old-svc/rename?name=new-svc", nil); st != http.StatusNoContent {
		t.Fatalf("rename = %d %s", st, body)
	}

	st, body = te.do(t, http.MethodGet, "/containers/new-svc/json", nil)
	if st != http.StatusOK {
		t.Fatalf("inspect = %d %s", st, body)
	}
	var ins containerInspectResponse
	if err := json.Unmarshal(body, &ins); err != nil {
		t.Fatal(err)
	}
	ep, ok := ins.NetworkSettings.Networks["ren-net"]
	if !ok {
		t.Fatalf("missing ren-net endpoint: %+v", ins.NetworkSettings.Networks)
	}
	if !slices.Contains(ep.DNSNames, "new-svc") || slices.Contains(ep.DNSNames, "old-svc") || !slices.Contains(ep.DNSNames, "alias1") {
		t.Fatalf("unexpected DNSNames after rename: %v", ep.DNSNames)
	}
}

func TestBuiltinBridgeRestoredAfterRestart(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeRunner()
	fake.addImage("alpine:3.20", ImageConfig{Cmd: []string{"/bin/sh"}})
	te := reopenTestEngine(t, dir, fake)

	if st, body := te.do(t, http.MethodPost, "/networks/create", map[string]any{
		"Name":     "int-net",
		"Internal": true,
	}); st != http.StatusCreated {
		t.Fatalf("network create = %d %s", st, body)
	}
	if st, body := te.do(t, http.MethodPost, "/containers/create?name=dual-ctr", map[string]any{
		"Image": "alpine:3.20",
	}); st != http.StatusCreated {
		t.Fatalf("create = %d %s", st, body)
	}
	if st, body := te.do(t, http.MethodPost, "/networks/int-net/connect", map[string]any{
		"Container": "dual-ctr",
	}); st != http.StatusOK {
		t.Fatalf("connect int-net = %d %s", st, body)
	}

	// Reopen engine on same data dir.
	te.srv.Close()
	_ = te.Engine.Close(time.Second)
	te2 := reopenTestEngine(t, dir, fake)

	// Container connected to both bridge and internal network must NOT be isolated.
	c, err := te2.Engine.lookup("dual-ctr")
	if err != nil {
		t.Fatal(err)
	}
	addrs, isolated := te2.Engine.overlayAddrs(&c)
	if len(addrs) != 1 || isolated {
		t.Fatalf("overlayAddrs after restart = (%v, isolated=%v), want 1 addr and isolated=false", addrs, isolated)
	}

	// Disconnecting from builtin bridge must succeed without Force after restart.
	if st, body := te2.do(t, http.MethodPost, "/networks/bridge/disconnect", map[string]any{
		"Container": "dual-ctr",
	}); st != http.StatusOK {
		t.Fatalf("disconnect bridge after restart = %d %s", st, body)
	}

	// Now only on int-net -> isolated becomes true.
	c, _ = te2.Engine.lookup("dual-ctr")
	_, isolated = te2.Engine.overlayAddrs(&c)
	if !isolated {
		t.Fatalf("expected isolated=true after disconnecting bridge")
	}
}

func TestContainerCreateCPUQuotaAndValidation(t *testing.T) {
	fake := newFakeRunner()
	fake.addImage("alpine:3.20", ImageConfig{Cmd: []string{"/bin/sh"}})
	te := reopenTestEngine(t, t.TempDir(), fake)

	// CpuQuota + CpuPeriod translates into Spec.CPUs at create time.
	st, body := te.do(t, http.MethodPost, "/containers/create?name=quota-ctr", map[string]any{
		"Image": "alpine:3.20",
		"HostConfig": map[string]any{
			"CpuQuota":  50000,
			"CpuPeriod": 100000,
		},
	})
	if st != http.StatusCreated {
		t.Fatalf("create with CpuQuota = %d %s", st, body)
	}
	c, err := te.Engine.lookup("quota-ctr")
	if err != nil {
		t.Fatal(err)
	}
	if c.Spec.CPUs != "0.5" {
		t.Fatalf("Spec.CPUs = %q, want 0.5", c.Spec.CPUs)
	}

	// Default period (100000) when only CpuQuota is set.
	st, body = te.do(t, http.MethodPost, "/containers/create?name=quota-def", map[string]any{
		"Image": "alpine:3.20",
		"HostConfig": map[string]any{
			"CpuQuota": 150000,
		},
	})
	if st != http.StatusCreated {
		t.Fatalf("create with CpuQuota default period = %d %s", st, body)
	}
	c2, _ := te.Engine.lookup("quota-def")
	if c2.Spec.CPUs != "1.5" {
		t.Fatalf("Spec.CPUs = %q, want 1.5", c2.Spec.CPUs)
	}

	// Conflicting NanoCpus + CpuQuota rejected with 400.
	if st, _ := te.do(t, http.MethodPost, "/containers/create?name=bad-conflict", map[string]any{
		"Image": "alpine:3.20",
		"HostConfig": map[string]any{
			"NanoCpus": 1_000_000_000,
			"CpuQuota": 50000,
		},
	}); st != http.StatusBadRequest {
		t.Fatalf("conflicting NanoCpus+CpuQuota = %d, want 400", st)
	}

	// Memory < 6MB rejected with 400.
	if st, _ := te.do(t, http.MethodPost, "/containers/create?name=bad-mem", map[string]any{
		"Image": "alpine:3.20",
		"HostConfig": map[string]any{
			"Memory": 1024,
		},
	}); st != http.StatusBadRequest {
		t.Fatalf("Memory < 6MB = %d, want 400", st)
	}
}

func TestImageListBeforeSinceSubsecondPrecision(t *testing.T) {
	fake := newFakeRunner()
	base := time.Date(2026, 5, 1, 12, 0, 0, 100_000_000, time.UTC)
	fake.addImage("img:old", ImageConfig{Cmd: []string{"/bin/sh"}})
	fake.addImage("img:mid", ImageConfig{Cmd: []string{"/bin/sh"}})
	fake.addImage("img:new", ImageConfig{Cmd: []string{"/bin/sh"}})

	fake.mu.Lock()
	for k, img := range fake.images {
		switch {
		case slices.Contains(img.RepoTags, "img:old"):
			img.Created = base
		case slices.Contains(img.RepoTags, "img:mid"):
			img.Created = base.Add(200 * time.Millisecond)
		case slices.Contains(img.RepoTags, "img:new"):
			img.Created = base.Add(400 * time.Millisecond)
		}
		fake.images[k] = img
	}
	fake.mu.Unlock()

	te := reopenTestEngine(t, t.TempDir(), fake)

	// before=img:mid must return ONLY img:old, NEVER img:mid itself.
	st, body := te.do(t, http.MethodGet, `/images/json?filters={"before":{"img:mid":true}}`, nil)
	if st != http.StatusOK {
		t.Fatalf("list before = %d %s", st, body)
	}
	var beforeList []imageSummary
	if err := json.Unmarshal(body, &beforeList); err != nil {
		t.Fatal(err)
	}
	if len(beforeList) != 1 || !slices.Contains(beforeList[0].RepoTags, "img:old") {
		t.Fatalf("before=img:mid got %+v, want only img:old", beforeList)
	}

	// since=img:mid must return ONLY img:new, NEVER img:mid itself.
	st, body = te.do(t, http.MethodGet, `/images/json?filters={"since":{"img:mid":true}}`, nil)
	if st != http.StatusOK {
		t.Fatalf("list since = %d %s", st, body)
	}
	var sinceList []imageSummary
	if err := json.Unmarshal(body, &sinceList); err != nil {
		t.Fatal(err)
	}
	if len(sinceList) != 1 || !slices.Contains(sinceList[0].RepoTags, "img:new") {
		t.Fatalf("since=img:mid got %+v, want only img:new", sinceList)
	}
}

func TestImageForceDeleteRefusesRunningContainer(t *testing.T) {
	fake := newFakeRunner()
	fake.Script = func(p *fakeProc) int { return <-p.end }
	fake.addImage("busy:1", ImageConfig{Cmd: []string{"/bin/sleep", "60"}})
	te := reopenTestEngine(t, t.TempDir(), fake)

	st, body := te.do(t, http.MethodPost, "/containers/create?name=run-img-ctr", map[string]any{
		"Image": "busy:1",
	})
	if st != http.StatusCreated {
		t.Fatalf("create = %d %s", st, body)
	}
	if st, body := te.do(t, http.MethodPost, "/containers/run-img-ctr/start", nil); st != http.StatusNoContent {
		t.Fatalf("start = %d %s", st, body)
	}

	// Deleting the only tag or by ID with force=1 while running must return 409.
	st, body = te.do(t, http.MethodDelete, "/images/busy:1?force=1", nil)
	if st != http.StatusConflict || !strings.Contains(string(body), "running container") {
		t.Fatalf("force delete running image = %d %s, want 409 running container", st, body)
	}

	// Adding a second tag lets us untag the extra tag, while still blocking by ID.
	if st, body := te.do(t, http.MethodPost, "/images/busy:1/tag?repo=busy&tag=extra", nil); st != http.StatusCreated {
		t.Fatalf("tag = %d %s", st, body)
	}
	if st, body := te.do(t, http.MethodDelete, "/images/busy:extra", nil); st != http.StatusOK {
		t.Fatalf("untag extra alias = %d %s, want 200", st, body)
	}

	// Stop the container -> force=1 now succeeds.
	if st, body := te.do(t, http.MethodPost, "/containers/run-img-ctr/stop?t=1", nil); st != http.StatusNoContent {
		t.Fatalf("stop = %d %s", st, body)
	}
	if st, body := te.do(t, http.MethodDelete, "/images/busy:1?force=1", nil); st != http.StatusOK {
		t.Fatalf("force delete stopped image = %d %s, want 200", st, body)
	}
}

func TestLegacyBuildQuietEmitsImageIDStream(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses unix backend socket in fake buildkit")
	}
	dir, err := os.MkdirTemp("", "gvxd-lbq-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	fake := newFakeRunner()
	fake.addImage(BuilderImage, ImageConfig{})
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
			defer func() { _ = ln.Close(); _ = os.Remove(sockPath) }()
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
			var outHost string
			for _, m := range p.Spec.Mounts {
				if m.Target == "/out" {
					outHost = m.Source
				}
			}
			p.Write(sandbox.Stderr, "#1 noisy progress line\n")
			_ = os.WriteFile(filepath.Join(outHost, "image.tar"), []byte("quietimg:1\n"), 0o600)
			return 0
		}
		return 0
	}

	e := New(Config{Dir: dir, Runner: fake, Version: "0.0.0-test", BuilderSocketDir: dir})
	if err := e.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(e.Handler())
	t.Cleanup(func() { srv.Close(); _ = e.Close(time.Second) })

	ctxArchive := makeTestTar(t, []struct {
		hdr  tar.Header
		body string
	}{{hdr: tar.Header{Name: "Dockerfile"}, body: "FROM alpine:3.20\n"}})

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1.52/build?q=1&t=quietimg:1", bytes.NewReader(ctxArchive))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /build?q=1 = %d %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "noisy progress line") {
		t.Fatalf("quiet build leaked progress output:\n%s", body)
	}
	if !strings.Contains(string(body), `"stream":"sha256:`) {
		t.Fatalf("quiet build did not emit image ID stream frame:\n%s", body)
	}
}
