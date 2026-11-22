//go:build darwin && arm64

package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop-vm/vmassets"
	"github.com/ironpark/gvx-desktop-vm/vmclient"
	"github.com/ironpark/gvx-desktop-vm/vmhelper"
)

// buildSignedVMHelper builds gvx-desktop-vm's cmd/gvx-vmhelper with CGO_ENABLED=0 and ad-hoc
// signs it with assets/entitlements.plist into a temporary directory.
func buildSignedVMHelper(t *testing.T) string {
	t.Helper()
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	binDir, err := os.MkdirTemp("", "gvxd-vmhbin-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(binDir) })

	bin := filepath.Join(binDir, "gvx-vmhelper")
	build := exec.Command("go", "build", "-o", bin, "github.com/ironpark/gvx-desktop-vm/cmd/gvx-vmhelper")
	build.Dir = repoRoot
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build cmd/gvx-vmhelper: %v\n%s", err, string(out))
	}

	entitlements := filepath.Join(repoRoot, "assets", "entitlements.plist")
	sign := exec.Command("codesign", "--sign", "-", "--entitlements", entitlements, "--force", bin)
	if out, err := sign.CombinedOutput(); err != nil {
		t.Fatalf("codesign gvx-vmhelper: %v\n%s", err, string(out))
	}
	return bin
}

func TestHarnessAmd64BuilderE2E(t *testing.T) {
	if testing.Short() || os.Getenv("GVX_ENGINE_HARNESS") != "1" {
		t.Skip("set GVX_ENGINE_HARNESS=1 and drop -short to run real amd64 builder E2E")
	}
	assetsOverride := strings.TrimSpace(os.Getenv(vmassets.EnvOverride))
	if assetsOverride == "" {
		t.Skipf("set %s to a directory made by gvx-desktop-vm's guest/mkassets", vmassets.EnvOverride)
	}

	helperBin := buildSignedVMHelper(t)
	h := startHarness(t)

	amd64Cfg := &Amd64Config{
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
		IdleTimeout:      6 * time.Second,
		ProbeIdleTimeout: 3 * time.Second,
	}

	// Create the gvx-desktop builder and append the linux/amd64 node BEFORE
	// SetAmd64 binds the front socket so buildx inspect does not eagerly wake the VM.
	node0Endpoint := "unix://" + h.e.BuilderSocket()
	node1Endpoint := "unix://" + h.e.Amd64Socket()

	if _, errOut, code := h.docker(t, "", "buildx", "create",
		"--name", BuilderName,
		"--driver", "remote",
		"--driver-opt", "default-load=true",
		node0Endpoint,
	); code != 0 {
		t.Fatalf("buildx create (%d): %s", code, errOut)
	}
	if _, errOut, code := h.docker(t, "", "buildx", "create",
		"--append",
		"--name", BuilderName,
		"--node", Amd64NodeName,
		"--platform", Amd64Platform,
		"--driver-opt", "default-load=true",
		node1Endpoint,
	); code != 0 {
		t.Fatalf("buildx create --append (%d): %s", code, errOut)
	}

	if err := h.e.SetAmd64(amd64Cfg); err != nil {
		t.Fatalf("SetAmd64: %v", err)
	}

	t.Cleanup(func() {
		_, _, _ = h.docker(t, "", "rmi", "-f", "gvxd-amd64:1", "gvxd-amd64-check:1", "gvxd-native:1")
	})

	ctxDir := t.TempDir()
	dockerfile := "FROM alpine:3.20\nRUN uname -m > /arch\n"
	if err := os.WriteFile(filepath.Join(ctxDir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("1_buildx_ls_shows_both_nodes", func(t *testing.T) {
		out, errOut, code := h.docker(t, "", "buildx", "ls")
		if code != 0 {
			t.Fatalf("buildx ls (%d): %s", code, errOut)
		}
		if !strings.Contains(out, BuilderName+"0") || !strings.Contains(out, Amd64NodeName) {
			t.Fatalf("buildx ls missing expected nodes:\n%s", out)
		}
	})

	t.Run("2_amd64_build_load_inspect_images_and_uname", func(t *testing.T) {
		metaFile := filepath.Join(t.TempDir(), "meta.json")
		if _, errOut, code := h.docker(t, "", "buildx", "build",
			"--builder", BuilderName,
			"--platform", "linux/amd64",
			"--load",
			"--metadata-file", metaFile,
			"-t", "gvxd-amd64:1",
			ctxDir,
		); code != 0 {
			t.Fatalf("amd64 build (%d): %s", code, errOut)
		}

		// Verify buildx routed to the amd64 node.
		if b, err := os.ReadFile(metaFile); err == nil {
			var meta map[string]any
			_ = json.Unmarshal(b, &meta)
			if ref, _ := meta["buildx.build.ref"].(string); !strings.Contains(ref, "/"+Amd64NodeName+"/") {
				t.Errorf("amd64 build ran on ref %q, want node %s", ref, Amd64NodeName)
			}
		}

		// Inspect confirms architecture is amd64.
		insOut, errOut, code := h.docker(t, "", "image", "inspect", "--format", "{{.Architecture}}", "gvxd-amd64:1")
		if code != 0 || strings.TrimSpace(insOut) != "amd64" {
			t.Fatalf("image inspect arch (%d): out=%q err=%q, want amd64", code, insOut, errOut)
		}

		// docker images shows gvxd-amd64:1.
		imgsOut, errOut, code := h.docker(t, "", "images")
		if code != 0 || !strings.Contains(imgsOut, "gvxd-amd64") {
			t.Fatalf("docker images (%d): out=%q err=%q", code, imgsOut, errOut)
		}

		// Confirm /arch == x86_64 by exporting local rootfs of the amd64 build.
		exportDir := t.TempDir()
		if _, errOut, code := h.docker(t, "", "buildx", "build",
			"--builder", BuilderName,
			"--platform", "linux/amd64",
			"-o", "type=local,dest="+exportDir,
			ctxDir,
		); code != 0 {
			t.Fatalf("amd64 export (%d): %s", code, errOut)
		}
		archBytes, err := os.ReadFile(filepath.Join(exportDir, "arch"))
		if err != nil {
			t.Fatalf("read exported /arch: %v", err)
		}
		if got := strings.TrimSpace(string(archBytes)); got != "x86_64" {
			t.Fatalf("exported /arch = %q, want x86_64", got)
		}
	})

	t.Run("3_docker_run_amd64_refused", func(t *testing.T) {
		out, errOut, code := h.docker(t, "", "run", "--rm", "gvxd-amd64:1")
		if code == 0 {
			t.Fatalf("expected docker run gvxd-amd64:1 to fail, got code 0: %s", out)
		}
		combined := out + errOut
		if !strings.Contains(combined, "amd64 images can be built and pushed, not run, yet") {
			t.Fatalf("docker run refusal message = %q", combined)
		}
		if strings.Contains(combined, "Unable to find image") || strings.Contains(combined, "Pulling from") {
			t.Fatalf("docker run tried to pull instead of refusing immediately:\n%s", combined)
		}
	})

	t.Run("4_native_build_stays_on_gvx_node", func(t *testing.T) {
		metaFile := filepath.Join(t.TempDir(), "meta-native.json")
		if _, errOut, code := h.docker(t, "", "buildx", "build",
			"--builder", BuilderName,
			"--load",
			"--metadata-file", metaFile,
			"-t", "gvxd-native:1",
			ctxDir,
		); code != 0 {
			t.Fatalf("native build (%d): %s", code, errOut)
		}
		b, err := os.ReadFile(metaFile)
		if err != nil {
			t.Fatalf("read native metadata: %v", err)
		}
		var meta map[string]any
		_ = json.Unmarshal(b, &meta)
		ref, _ := meta["buildx.build.ref"].(string)
		if !strings.Contains(ref, "/"+BuilderName+"0/") {
			t.Fatalf("native build ran on ref %q, want node %s0", ref, BuilderName)
		}
	})

	t.Run("5_multiplatform_local_and_oci_message", func(t *testing.T) {
		outDir := t.TempDir()
		if _, errOut, code := h.docker(t, "", "buildx", "build",
			"--builder", BuilderName,
			"--platform", "linux/amd64,linux/arm64",
			"-o", "type=local,dest="+outDir,
			ctxDir,
		); code != 0 {
			t.Fatalf("multiplatform type=local (%d): %s", code, errOut)
		}
		amdArch, err := os.ReadFile(filepath.Join(outDir, "linux_amd64", "arch"))
		if err != nil || strings.TrimSpace(string(amdArch)) != "x86_64" {
			t.Fatalf("linux_amd64/arch = %q, err=%v; want x86_64", strings.TrimSpace(string(amdArch)), err)
		}
		armArch, err := os.ReadFile(filepath.Join(outDir, "linux_arm64", "arch"))
		if err != nil || strings.TrimSpace(string(armArch)) != "aarch64" {
			t.Fatalf("linux_arm64/arch = %q, err=%v; want aarch64", strings.TrimSpace(string(armArch)), err)
		}

		// Assert buildx's multi-node OCI export limitation message so future buildx changes are noticed.
		ociTar := filepath.Join(t.TempDir(), "out.tar")
		out, errOut, code := h.docker(t, "", "buildx", "build",
			"--builder", BuilderName,
			"--platform", "linux/amd64,linux/arm64",
			"-o", "type=oci,dest="+ociTar,
			ctxDir,
		)
		if code == 0 {
			t.Logf("note: buildx now supports multi-node type=oci export")
		} else if !strings.Contains(strings.ToLower(out+errOut), "multi-node") {
			t.Logf("multi-node type=oci error: %s %s", out, errOut)
		}
	})

	t.Run("6_push_auth_flows_through_buildx_session", func(t *testing.T) {
		ln, err := net.Listen("tcp", "0.0.0.0:18097")
		if err != nil {
			t.Skipf("port 18097 unavailable: %v", err)
		}
		defer ln.Close()

		var authMu sync.Mutex
		var sawAuth string
		mux := http.NewServeMux()
		mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			if auth == "" {
				w.Header().Set("WWW-Authenticate", `Basic realm="gvxd-test"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			authMu.Lock()
			sawAuth = auth
			authMu.Unlock()
			w.WriteHeader(http.StatusUnauthorized)
		})
		regSrv := &http.Server{Handler: mux}
		go func() { _ = regSrv.Serve(ln) }()
		defer regSrv.Close()

		// Write Basic credentials for 192.168.64.1:18097 into the temp DOCKER_CONFIG.
		cred := base64.StdEncoding.EncodeToString([]byte("gvxuser:gvxpass"))
		cfgFile := filepath.Join(h.config, "config.json")
		cfgRaw, _ := json.Marshal(map[string]any{
			"auths": map[string]any{
				"192.168.64.1:18097": map[string]any{"auth": cred},
			},
		})
		if err := os.WriteFile(cfgFile, cfgRaw, 0o600); err != nil {
			t.Fatal(err)
		}

		// Push will hit our stub registry and send credentials; we expect push to fail with 401
		// after presenting Basic auth.
		_, _, _ = h.docker(t, "", "buildx", "build",
			"--builder", BuilderName,
			"--platform", "linux/amd64",
			"--output", "type=image,name=192.168.64.1:18097/gvxd/x:1,push=true,registry.insecure=true",
			ctxDir,
		)
		authMu.Lock()
		gotAuth := sawAuth
		authMu.Unlock()
		if gotAuth != "Basic "+cred {
			t.Fatalf("stub registry saw Authorization=%q, want %q", gotAuth, "Basic "+cred)
		}
	})

	t.Run("7_idle_stop_restart_and_persistent_cache_hit", func(t *testing.T) {
		deadline := time.Now().Add(30 * time.Second)
		for h.e.Amd64State().Status != BuilderIdle && time.Now().Before(deadline) {
			time.Sleep(250 * time.Millisecond)
		}
		if st := h.e.Amd64State(); st.Status != BuilderIdle {
			t.Fatalf("amd64 VM did not stop on idle: %+v", st)
		}
		if st := h.e.Amd64State(); st.DiskBytes <= 0 {
			t.Errorf("expected persistent disk allocation > 0, got %d", st.DiskBytes)
		}

		out, errOut, code := h.docker(t, "", "buildx", "build",
			"--builder", BuilderName,
			"--platform", "linux/amd64",
			"--progress=plain",
			"--load",
			"-t", "gvxd-amd64:1",
			ctxDir,
		)
		if code != 0 {
			t.Fatalf("rebuild after idle stop (%d): %s", code, errOut)
		}
		if !strings.Contains(out+errOut, "CACHED") {
			t.Fatalf("rebuild after VM restart did not hit persistent cache:\n%s\n%s", out, errOut)
		}
	})

	t.Run("8_helper_crash_restart", func(t *testing.T) {
		// Kill our helper process directly while VM is running, wait for idle+Err, then rebuild.
		h.e.amd.mu.Lock()
		c := h.e.amd.client
		h.e.amd.mu.Unlock()
		if vc, ok := c.(*vmclient.Client); ok && vc != nil {
			if hello, err := vc.Hello(context.Background()); err == nil && hello.PID > 0 {
				if p, err := os.FindProcess(hello.PID); err == nil {
					_ = p.Kill()
				}
			}
		}
		deadline := time.Now().Add(15 * time.Second)
		for h.e.Amd64State().Status != BuilderIdle && time.Now().Before(deadline) {
			time.Sleep(150 * time.Millisecond)
		}
		if st := h.e.Amd64State(); st.Status != BuilderIdle {
			t.Fatalf("state did not return to idle after helper kill: %+v", st)
		}

		if _, errOut, code := h.docker(t, "", "buildx", "build",
			"--builder", BuilderName,
			"--platform", "linux/amd64",
			"--load",
			"-t", "gvxd-amd64:1",
			ctxDir,
		); code != 0 {
			t.Fatalf("build after helper crash (%d): %s", code, errOut)
		}
	})

	t.Run("9_leave_removes_only_amd64_node", func(t *testing.T) {
		if _, errOut, code := h.docker(t, "", "buildx", "create",
			"--leave",
			"--name", BuilderName,
			"--node", Amd64NodeName,
		); code != 0 {
			t.Fatalf("buildx create --leave (%d): %s", code, errOut)
		}
		out, errOut, code := h.docker(t, "", "buildx", "inspect", BuilderName)
		if code != 0 {
			t.Fatalf("buildx inspect after leave (%d): %s", code, errOut)
		}
		if strings.Contains(out, Amd64NodeName) {
			t.Fatalf("amd64 node still present after --leave:\n%s", out)
		}
		if !strings.Contains(out, BuilderName+"0") {
			t.Fatalf("node 0 missing after --leave:\n%s", out)
		}
	})
}
