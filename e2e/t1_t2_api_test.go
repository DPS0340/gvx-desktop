//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/engine"
	"github.com/ironpark/gvx-desktop/internal/ext"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/uiblock"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

// =============================================================================
// Shared E2E-3 API Fixture Sources & Helpers (all unexported prefixed e2e3API)
// =============================================================================

// e2e3APIDockerFixtureSource exercises GVX_DOCKER_HOST, Host/Ext endpoints,
// folder picking, and Docker container create / ping / list requests.
const e2e3APIDockerFixtureSource = `package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func dockerClient(raw string) (*http.Client, string) {
	tr := &http.Transport{}
	base := strings.TrimRight(raw, "/")
	if strings.HasPrefix(raw, "unix://") {
		sock := strings.TrimPrefix(raw, "unix://")
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}
		base = "http://docker"
	} else if strings.HasPrefix(raw, "/") {
		sock := raw
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}
		base = "http://docker"
	} else if strings.HasPrefix(raw, "tcp://") {
		base = "http://" + strings.TrimPrefix(raw, "tcp://")
	}
	return &http.Client{Transport: tr, Timeout: 5 * time.Second}, base
}

func doDocker(method, path string, body any) (int, string, error) {
	rawHost := os.Getenv("GVX_DOCKER_HOST")
	if rawHost == "" {
		return 0, "", os.ErrNotExist
	}
	cl, base := dockerClient(rawHost)
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, "", err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		return 0, "", err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := cl.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw), nil
}

func writeResult(name string, v any) error {
	dir := os.Getenv("GVX_EXT_DATA_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name+".json"), b, 0o600)
}

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("docker-fixture"))
		},
	})
	app.Command("probe-env", func(ctx *extsdk.CommandContext) error {
		return writeResult("probe_env", map[string]any{
			"docker_host": os.Getenv("GVX_DOCKER_HOST"),
			"data_dir":    os.Getenv("GVX_EXT_DATA_DIR"),
		})
	})
	app.Command("create-plain", func(ctx *extsdk.CommandContext) error {
		st, body, err := doDocker(http.MethodPost, "/containers/create", map[string]any{
			"Image": "alpine:3.20",
			"Cmd":   []string{"/bin/sh"},
		})
		errStr := ""
		if err != nil {
			errStr = err.Error()
		}
		return writeResult("create_plain", map[string]any{"status": st, "body": body, "err": errStr})
	})
	app.Command("pick-folder", func(ctx *extsdk.CommandContext) error {
		dir, ok, err := ctx.Host.PickFolder(ctx)
		errStr := ""
		if err != nil {
			errStr = err.Error()
		}
		return writeResult("pick_folder", map[string]any{"dir": dir, "ok": ok, "err": errStr})
	})
	app.Command("create-bind-allowed", func(ctx *extsdk.CommandContext) error {
		dir := os.Getenv("GVX_EXT_DATA_DIR")
		b, err := os.ReadFile(filepath.Join(dir, "bind_target.txt"))
		if err != nil {
			return err
		}
		bindSrc := strings.TrimSpace(string(b))
		st, body, derr := doDocker(http.MethodPost, "/containers/create", map[string]any{
			"Image": "alpine:3.20",
			"HostConfig": map[string]any{
				"Binds": []string{bindSrc + ":/work"},
			},
		})
		errStr := ""
		if derr != nil {
			errStr = derr.Error()
		}
		return writeResult("create_bind_allowed", map[string]any{"status": st, "body": body, "err": errStr})
	})
	app.Command("create-mount-files", func(ctx *extsdk.CommandContext) error {
		filesDir := filepath.Join(os.Getenv("GVX_EXT_DATA_DIR"), "work")
		if err := os.MkdirAll(filesDir, 0o700); err != nil {
			return err
		}
		st, body, derr := doDocker(http.MethodPost, "/containers/create", map[string]any{
			"Image": "alpine:3.20",
			"HostConfig": map[string]any{
				"Mounts": []map[string]any{
					{"Type": "bind", "Source": filesDir, "Target": "/data"},
				},
			},
		})
		errStr := ""
		if derr != nil {
			errStr = derr.Error()
		}
		return writeResult("create_mount_files", map[string]any{"status": st, "body": body, "err": errStr})
	})
	app.Command("ping-and-list", func(ctx *extsdk.CommandContext) error {
		stPing, bodyPing, errPing := doDocker(http.MethodGet, "/_ping", nil)
		stList, bodyList, errList := doDocker(http.MethodGet, "/containers/json", nil)
		ep, el := "", ""
		if errPing != nil {
			ep = errPing.Error()
		}
		if errList != nil {
			el = errList.Error()
		}
		return writeResult("ping_and_list", map[string]any{
			"ping_status": stPing, "ping_body": bodyPing, "ping_err": ep,
			"list_status": stList, "list_body": bodyList, "list_err": el,
		})
	})
	app.Command("create-spoof-label", func(ctx *extsdk.CommandContext) error {
		st, body, derr := doDocker(http.MethodPost, "/containers/create", map[string]any{
			"Image":  "alpine:3.20",
			"Labels": map[string]string{"io.gvx.extension": "victim-ext"},
		})
		errStr := ""
		if derr != nil {
			errStr = derr.Error()
		}
		return writeResult("create_spoof_label", map[string]any{"status": st, "body": body, "err": errStr})
	})
	app.Command("create-bad-binds", func(ctx *extsdk.CommandContext) error {
		home, _ := os.UserHomeDir()
		sshDir := filepath.Join(home, ".ssh")
		st1, body1, _ := doDocker(http.MethodPost, "/containers/create", map[string]any{
			"Image":      "alpine:3.20",
			"HostConfig": map[string]any{"Binds": []string{"/etc:/host-etc"}},
		})
		st2, body2, _ := doDocker(http.MethodPost, "/containers/create", map[string]any{
			"Image":      "alpine:3.20",
			"HostConfig": map[string]any{"Binds": []string{sshDir + ":/host-ssh"}},
		})
		return writeResult("create_bad_binds", map[string]any{
			"etc_status": st1, "etc_body": body1,
			"ssh_status": st2, "ssh_body": body2,
		})
	})
	app.Command("create-mount-escape", func(ctx *extsdk.CommandContext) error {
		dataDir := os.Getenv("GVX_EXT_DATA_DIR")
		b, err := os.ReadFile(filepath.Join(dataDir, "outside_dir.txt"))
		if err != nil {
			return err
		}
		outside := strings.TrimSpace(string(b))
		symPath := filepath.Join(dataDir, "escape-link")
		_ = os.Remove(symPath)
		if err := os.Symlink(outside, symPath); err != nil {
			return err
		}
		traversal := filepath.Join(dataDir, "..", "secrets.json")
		st1, body1, _ := doDocker(http.MethodPost, "/containers/create", map[string]any{
			"Image": "alpine:3.20",
			"HostConfig": map[string]any{
				"Mounts": []map[string]any{{"Type": "bind", "Source": traversal, "Target": "/esc1"}},
			},
		})
		st2, body2, _ := doDocker(http.MethodPost, "/containers/create", map[string]any{
			"Image": "alpine:3.20",
			"HostConfig": map[string]any{
				"Mounts": []map[string]any{{"Type": "bind", "Source": symPath, "Target": "/esc2"}},
			},
		})
		return writeResult("create_mount_escape", map[string]any{
			"trav_status": st1, "trav_body": body1,
			"sym_status":  st2, "sym_body":  body2,
		})
	})
	app.Command("ping-unavailable", func(ctx *extsdk.CommandContext) error {
		st1, body1, err1 := doDocker(http.MethodGet, "/_ping", nil)
		st2, body2, err2 := doDocker(http.MethodPost, "/containers/create", map[string]any{"Image": "alpine:3.20"})
		e1, e2 := "", ""
		if err1 != nil {
			e1 = err1.Error()
		}
		if err2 != nil {
			e2 = err2.Error()
		}
		return writeResult("ping_unavailable", map[string]any{
			"ping_status": st1, "ping_body": body1, "ping_err": e1,
			"create_status": st2, "create_body": body2, "create_err": e2,
		})
	})
	_ = app.Run(context.Background())
}
`

// e2e3APIFullHostFixtureSource exercises Host/Extension bidirectional API,
// views, commands, tools, settings, SSE events, sandboxes, and gesture gating.
const e2e3APIFullHostFixtureSource = `package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func recordJSON(name string, v any) error {
	dir := os.Getenv("GVX_EXT_DATA_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name+".json"), b, 0o600)
}

func main() {
	_ = recordJSON("endpoints", map[string]string{
		"host_api":   os.Getenv("GVX_HOST_API"),
		"ext_listen": os.Getenv("GVX_EXT_LISTEN"),
		"ext_token":  os.Getenv("GVX_EXT_TOKEN"),
		"data_dir":   os.Getenv("GVX_EXT_DATA_DIR"),
	})

	var counter atomic.Int64
	var mu sync.Mutex
	var events []extsdk.HostEvent
	var capturedGesture string

	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(
				ui.Text(fmt.Sprintf("count=%d", counter.Load()), extsdk.Key("counter")),
				ui.Button("Increment", extsdk.Key("inc"), extsdk.OnTap("inc")),
				ui.Button("Pick", extsdk.Key("pick"), extsdk.OnTap("pick")),
			)
		},
		On: extsdk.Handlers{
			"inc": func(ctx *extsdk.EventContext) error {
				counter.Add(1)
				ctx.NotifyChanged()
				return nil
			},
			"pick": func(ctx *extsdk.EventContext) error {
				dir, ok, err := ctx.Host.PickFolder(ctx)
				errStr := ""
				if err != nil {
					errStr = err.Error()
				}
				return recordJSON("view_pick", map[string]any{"dir": dir, "ok": ok, "err": errStr})
			},
		},
	})

	app.Command("check-info", func(ctx *extsdk.CommandContext) error {
		info, err := ctx.Host.Info(ctx)
		if err != nil {
			return err
		}
		return recordJSON("host_info", info)
	})

	app.Command("record-cmd", func(ctx *extsdk.CommandContext) error {
		return recordJSON("cmd_ran", map[string]any{"cmd": ctx.CommandID, "gesture": ctx.GestureID()})
	})

	app.Command("set-badge", func(ctx *extsdk.CommandContext) error {
		return ctx.Host.SetBadge(ctx, extsdk.BadgeRequest{
			Text:    "3",
			Tone:    "accent",
			Tooltip: "3 active",
		})
	})

	app.Command("send-toast", func(ctx *extsdk.CommandContext) error {
		return ctx.Host.Toast(ctx, "info", "hello from ext")
	})

	app.Command("open-term-and-url", func(ctx *extsdk.CommandContext) error {
		if err := ctx.Host.OpenTerminal(ctx, extsdk.OpenTerminalRequest{
			TargetType: "container",
			TargetID:   "ctr-1",
			Shell:      "/bin/sh",
		}); err != nil {
			return err
		}
		return ctx.Host.OpenURL(ctx, "https://example.invalid/docs")
	})

	app.Command("copy-text", func(ctx *extsdk.CommandContext) error {
		return ctx.Host.Copy(ctx, "copied-from-gesture")
	})

	app.Command("capture-gesture", func(ctx *extsdk.CommandContext) error {
		mu.Lock()
		capturedGesture = ctx.GestureID()
		mu.Unlock()
		return recordJSON("captured_gesture", map[string]string{"id": ctx.GestureID()})
	})

	app.Command("test-bad-gestures", func(ctx *extsdk.CommandContext) error {
		// 1. Background context with no gesture ID.
		bg := context.Background()
		errMissingURL := ctx.Host.OpenURL(bg, "https://example.invalid/missing")
		errMissingCopy := ctx.Host.Copy(bg, "missing")
		// 2. Unknown/spoofed gesture ID.
		spoofCtx := extsdk.WithGestureID(bg, "bogus-gesture-999")
		errUnknown := ctx.Host.OpenURL(spoofCtx, "https://example.invalid/spoof")
		// 3. Expired/completed gesture captured from earlier handler.
		mu.Lock()
		oldGID := capturedGesture
		mu.Unlock()
		var errExpired error
		if oldGID != "" {
			errExpired = ctx.Host.OpenURL(extsdk.WithGestureID(bg, oldGID), "https://example.invalid/expired")
		} else {
			errExpired = errors.New("no captured gesture")
		}
		return recordJSON("bad_gestures", map[string]any{
			"missing_url_gesture_req":  extsdk.IsGestureRequired(errMissingURL),
			"missing_copy_gesture_req": extsdk.IsGestureRequired(errMissingCopy),
			"unknown_gesture_req":      extsdk.IsGestureRequired(errUnknown),
			"expired_gesture_req":      extsdk.IsGestureRequired(errExpired),
			"missing_url_err":          fmt.Sprint(errMissingURL),
			"unknown_err":              fmt.Sprint(errUnknown),
			"expired_err":              fmt.Sprint(errExpired),
		})
	})

	app.Command("test-9-intents", func(ctx *extsdk.CommandContext) error {
		okCount := 0
		var ninthErr error
		for i := 1; i <= 9; i++ {
			err := ctx.Host.Copy(ctx, fmt.Sprintf("intent-%d", i))
			if i <= 8 {
				if err == nil {
					okCount++
				}
			} else {
				ninthErr = err
			}
		}
		mu.Lock()
		capturedGesture = ctx.GestureID()
		mu.Unlock()
		return recordJSON("nine_intents", map[string]any{
			"ok_count":  okCount,
			"ninth_err": fmt.Sprint(ninthErr),
			"ninth_403": extsdk.IsGestureRequired(ninthErr) || extsdk.IsPermissionDenied(ninthErr),
		})
	})

	app.Command("replay-captured", func(ctx *extsdk.CommandContext) error {
		mu.Lock()
		gid := capturedGesture
		mu.Unlock()
		err := ctx.Host.Copy(extsdk.WithGestureID(context.Background(), gid), "replay")
		return recordJSON("replay_result", map[string]any{
			"err":     fmt.Sprint(err),
			"blocked": extsdk.IsGestureRequired(err) || extsdk.IsPermissionDenied(err),
		})
	})

	app.Command("test-missing-ui-perms", func(ctx *extsdk.CommandContext) error {
		_, _, errPick := ctx.Host.PickFolder(ctx)
		errTerm := ctx.Host.OpenTerminal(ctx, extsdk.OpenTerminalRequest{TargetType: "container", TargetID: "c1"})
		errURL := ctx.Host.OpenURL(ctx, "https://example.invalid")
		return recordJSON("missing_ui_perms", map[string]any{
			"pick_403": extsdk.IsPermissionDenied(errPick),
			"term_403": extsdk.IsPermissionDenied(errTerm),
			"url_403":  extsdk.IsPermissionDenied(errURL),
		})
	})

	app.Command("test-unsafe-urls", func(ctx *extsdk.CommandContext) error {
		bad := []string{
			"file:///etc/passwd",
			"javascript:alert(1)",
			"data:text/html,hi",
			"https://user:pass@example.invalid",
			"",
		}
		results := make(map[string]string, len(bad))
		for _, u := range bad {
			if err := ctx.Host.OpenURL(ctx, u); err != nil {
				results[u] = err.Error()
			} else {
				results[u] = "OK"
			}
		}
		return recordJSON("unsafe_urls", results)
	})

	app.Command("test-copy-and-toast-limits", func(ctx *extsdk.CommandContext) error {
		errBig := ctx.Host.Copy(ctx, strings.Repeat("a", (64<<10)+1))
		errUTF8 := ctx.Host.Copy(ctx, "bad\xffutf8")
		toastOK := 0
		var err13 error
		for i := 1; i <= 13; i++ {
			err := ctx.Host.Toast(ctx, "info", fmt.Sprintf("toast-%d", i))
			if i <= 12 {
				if err == nil {
					toastOK++
				}
			} else {
				err13 = err
			}
		}
		return recordJSON("copy_toast_limits", map[string]any{
			"big_err":     fmt.Sprint(errBig),
			"utf8_err":    fmt.Sprint(errUTF8),
			"toast_ok":    toastOK,
			"toast13_429": extsdk.IsRateLimited(err13),
			"toast13_err": fmt.Sprint(err13),
		})
	})

	app.Command("mount-sock-denied", func(ctx *extsdk.CommandContext) error {
		_, err := ctx.Host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
			Name:            "sneaky",
			Image:           "alpine:3.20",
			MountDockerSock: true,
		})
		return recordJSON("mount_sock_denied", map[string]any{
			"denied": extsdk.IsPermissionDenied(err),
			"err":    fmt.Sprint(err),
		})
	})

	app.Command("unowned-sandbox", func(ctx *extsdk.CommandContext) error {
		target := "user-owned-sandbox"
		_, errStart := ctx.Host.SandboxStart(ctx, target)
		_, errStop := ctx.Host.SandboxStop(ctx, target)
		errDel := ctx.Host.SandboxDelete(ctx, target)
		_, errExec := ctx.Host.SandboxExec(ctx, target, extsdk.ExecRequest{Argv: []string{"echo", "hi"}})
		_, errLogs := ctx.Host.SandboxLogs(ctx, target, 10)
		_, errSnapCreate := ctx.Host.SnapshotCreate(ctx, target, extsdk.CreateSnapshotRequest{Name: "s1"})
		_, errSnapList := ctx.Host.SnapshotList(ctx, target)
		_, errRestore := ctx.Host.Restore(ctx, target, extsdk.RestoreRequest{Snapshot: "s1"})
		errSnapDel := ctx.Host.SnapshotDelete(ctx, target, "s1")
		return recordJSON("unowned_sandbox", map[string]any{
			"start_403":       extsdk.IsPermissionDenied(errStart),
			"stop_403":        extsdk.IsPermissionDenied(errStop),
			"delete_403":      extsdk.IsPermissionDenied(errDel),
			"exec_403":        extsdk.IsPermissionDenied(errExec),
			"logs_403":        extsdk.IsPermissionDenied(errLogs),
			"snap_create_403": extsdk.IsPermissionDenied(errSnapCreate),
			"snap_list_403":   extsdk.IsPermissionDenied(errSnapList),
			"restore_403":     extsdk.IsPermissionDenied(errRestore),
			"snap_del_403":    extsdk.IsPermissionDenied(errSnapDel),
		})
	})

	app.Command("owned-sandbox-lifecycle", func(ctx *extsdk.CommandContext) error {
		ckpt := true
		sbx, err := ctx.Host.SandboxCreate(ctx, extsdk.CreateSandboxRequest{
			Name:       "ext-owned",
			Image:      "alpine:3.20",
			Command:    "sleep 60",
			Checkpoint: &ckpt,
			Ephemeral:  true,
		})
		if err != nil {
			return recordJSON("owned_sandbox", map[string]any{"step": "create", "err": err.Error()})
		}
		list, err := ctx.Host.SandboxList(ctx, true)
		if err != nil {
			return recordJSON("owned_sandbox", map[string]any{"step": "list", "err": err.Error()})
		}
		snap, err := ctx.Host.SnapshotCreate(ctx, sbx.ID, extsdk.CreateSnapshotRequest{Name: "cp1", LeaveRunning: true})
		if err != nil {
			return recordJSON("owned_sandbox", map[string]any{"step": "snap_create", "err": err.Error()})
		}
		snaps, err := ctx.Host.SnapshotList(ctx, sbx.ID)
		if err != nil {
			return recordJSON("owned_sandbox", map[string]any{"step": "snap_list", "err": err.Error()})
		}
		_ = ctx.Host.SnapshotDelete(ctx, sbx.ID, "cp1")
		_, _ = ctx.Host.SandboxStop(ctx, sbx.ID)
		_ = ctx.Host.SandboxDelete(ctx, sbx.ID)
		return recordJSON("owned_sandbox", map[string]any{
			"ok":        true,
			"id":        sbx.ID,
			"list_len":  len(list),
			"snap_name": snap.Name,
			"snaps_len": len(snaps),
		})
	})

	app.Tool("echo", func(ctx *extsdk.ToolContext, args map[string]any) (any, error) {
		return map[string]any{"echo": args["msg"]}, nil
	})

	app.OnSettingsChanged(func(ctx *extsdk.Context) error {
		s, err := ctx.Host.Settings(ctx)
		if err != nil {
			return err
		}
		return recordJSON("settings_changed", map[string]any{
			"api_url":             s.String("api_url"),
			"enabled":             s.Bool("enabled"),
			"api_token_is_set":    s.IsSet("api_token"),
			"api_token_plaintext": s.String("api_token"),
			"raw_token":           s.Raw()["api_token"],
		})
	})

	app.OnEvent(func(ctx *extsdk.Context, ev extsdk.HostEvent) {
		mu.Lock()
		events = append(events, ev)
		snap := append([]extsdk.HostEvent(nil), events...)
		mu.Unlock()
		_ = recordJSON("host_events", snap)
	})

	_ = app.Run(context.Background())
}
`

// e2e3APIHealthySource is a minimal long-running extsdk binary fixture.
const e2e3APIHealthySource = `package main

import (
	"context"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("healthy-ready"))
		},
	})
	_ = app.Run(context.Background())
}
`

// e2e3APICrashableSource exits with code 1 while crash.flag exists in
// GVX_EXT_DATA_DIR (or runs healthy when crash.flag is absent).
const e2e3APICrashableSource = `package main

import (
	"context"
	"os"
	"path/filepath"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	if dir := os.Getenv("GVX_EXT_DATA_DIR"); dir != "" {
		if _, err := os.Stat(filepath.Join(dir, "crash.flag")); err == nil {
			os.Exit(1)
		}
	}
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("recovered-ready"))
		},
	})
	_ = app.Run(context.Background())
}
`

func e2e3APIWriteBinaryDirWithIdle(t *testing.T, root, id string, binData []byte, idleTimeoutSec int, extraTOML string, perms ...string) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "schema_version = 1\n")
	fmt.Fprintf(&b, "id = %q\n", id)
	fmt.Fprintf(&b, "version = \"0.1.0\"\n")
	fmt.Fprintf(&b, "publisher = \"acme\"\n")
	fmt.Fprintf(&b, "name = \"Binary Extension %s\"\n", id)
	fmt.Fprintf(&b, "description = \"Developer mode E2E extension\"\n")
	fmt.Fprintf(&b, "icon = \"terminal\"\n")
	if len(perms) > 0 {
		quoted := make([]string, len(perms))
		for i, p := range perms {
			quoted[i] = fmt.Sprintf("%q", p)
		}
		fmt.Fprintf(&b, "permissions = [%s]\n", strings.Join(quoted, ", "))
	}
	fmt.Fprintf(&b, "\n[runtime]\n")
	fmt.Fprintf(&b, "type = \"binary\"\n")
	fmt.Fprintf(&b, "dev_entrypoint = \"bin/ext\"\n")
	fmt.Fprintf(&b, "idle_timeout_sec = %d\n", idleTimeoutSec)
	fmt.Fprintf(&b, "\n[[contributes.view]]\n")
	fmt.Fprintf(&b, "id = \"main\"\n")
	fmt.Fprintf(&b, "title = \"Main\"\n")
	fmt.Fprintf(&b, "icon = \"terminal\"\n")
	if extraTOML != "" {
		b.WriteString(extraTOML)
	}

	extDir := filepath.Join(root, id)
	writeExtFixture(t, extDir, b.String(), nil)
	if err := os.MkdirAll(filepath.Join(extDir, "bin"), 0o700); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "bin", "ext"), binData, 0o755); err != nil {
		t.Fatalf("write dev_entrypoint: %v", err)
	}
	return extDir
}

func e2e3APIInstallAndStartBinaryExt(t *testing.T, mgr *ext.Manager, id, sourceCode, extraTOML string, perms ...string) string {
	t.Helper()
	binPath := buildFixtureSDKBinary(t, sourceCode)
	binData, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read fixture binary: %v", err)
	}
	extDir := e2e3APIWriteBinaryDirWithIdle(t, t.TempDir(), id, binData, 0, extraTOML, perms...)
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal(%s): %v", id, err)
	}
	if err := mgr.AllowAndBuild(id); err != nil {
		t.Fatalf("AllowAndBuild(%s): %v", id, err)
	}
	e2e3APIWaitForExtState(t, mgr, id, ext.StateRunning, 5*time.Second)
	return extDir
}

func e2e3APIFindExtInfo(t *testing.T, mgr *ext.Manager, id string) (ext.ExtensionInfo, bool) {
	t.Helper()
	for _, item := range mgr.List() {
		if item.ID == id {
			return item, true
		}
	}
	return ext.ExtensionInfo{}, false
}

func e2e3APIWaitForExtState(t *testing.T, mgr *ext.Manager, id string, want ext.ExtensionState, timeout time.Duration) ext.ExtensionInfo {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last ext.ExtensionInfo
	for time.Now().Before(deadline) {
		if info, ok := e2e3APIFindExtInfo(t, mgr, id); ok {
			last = info
			if info.State == want {
				return info
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for extension %q state %q (last: %+v)", id, want, last)
	return ext.ExtensionInfo{}
}

func e2e3APIMustReadJSONFile(t *testing.T, path string, dst any) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil && len(bytes.TrimSpace(b)) > 0 {
			if uerr := json.Unmarshal(b, dst); uerr == nil {
				return
			} else {
				lastErr = uerr
			}
		} else if err != nil {
			lastErr = err
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("failed reading JSON file %s: %v", path, lastErr)
}

func e2e3APIHostEndpointClient(t *testing.T, endpoint string) (*http.Client, string) {
	t.Helper()
	if strings.HasPrefix(endpoint, "unix://") {
		sock := strings.TrimPrefix(endpoint, "unix://")
		return newUnixHTTPClient(sock), "http://unix"
	}
	if strings.HasPrefix(endpoint, "/") {
		return newUnixHTTPClient(endpoint), "http://unix"
	}
	return &http.Client{Timeout: 5 * time.Second}, strings.TrimRight(endpoint, "/")
}

func e2e3APIDoRawJSON(t *testing.T, client *http.Client, method, rawURL, bearer, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, rawURL, strings.NewReader(body))
	if err != nil {
		t.Fatalf("http.NewRequest(%s %s): %v", method, rawURL, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do(%s %s): %v", method, rawURL, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}
	return resp.StatusCode, raw
}

func e2e3APINonReservedDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	guest, err := recipes.GuestPath(dir)
	isReserved := err != nil || recipes.ReservedGuestPath(guest)
	if !isReserved && runtime.GOOS == "darwin" {
		if guest == "/private" || (strings.HasPrefix(guest, "/private/") && recipes.ReservedGuestPath(strings.TrimPrefix(guest, "/private"))) {
			isReserved = true
		}
	}
	if !isReserved {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Fatalf("cannot find non-reserved host folder (temp dir %q is reserved): %v", dir, err)
	}
	custom, err := os.MkdirTemp(home, "gvx-e2e3-api-*")
	if err != nil {
		t.Fatalf("os.MkdirTemp in home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(custom) })
	if resolved, err := filepath.EvalSymlinks(custom); err == nil {
		custom = resolved
	}
	return custom
}

// e2e3APIFakeImage stores seeded OCI image metadata for e2e3APIFakeEngineRunner.
type e2e3APIFakeImage struct {
	id   string
	repo string
	tag  string
	size int64
}

// e2e3APIFakeEngineRunner is a minimal engine.Runner double used in F20 KeepImages
// prune integration tests via public engine.New(engine.Config{..., KeepImages: mgr.KeepImages}).
type e2e3APIFakeEngineRunner struct {
	mu     sync.Mutex
	images map[string]e2e3APIFakeImage
}

func e2e3APINewFakeEngineRunner() *e2e3APIFakeEngineRunner {
	return &e2e3APIFakeEngineRunner{images: make(map[string]e2e3APIFakeImage)}
}

func (r *e2e3APIFakeEngineRunner) seedImage(repo, tag string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	sum := sha256.Sum256([]byte(repo + ":" + tag))
	id := "sha256:" + hex.EncodeToString(sum[:])
	r.images[id] = e2e3APIFakeImage{id: id, repo: repo, tag: tag, size: 1 << 20}
	return id
}

func (r *e2e3APIFakeEngineRunner) hasRef(ref string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, img := range r.images {
		if img.id == ref || (img.repo+":"+img.tag) == ref {
			return true
		}
	}
	return false
}

func (r *e2e3APIFakeEngineRunner) SetHandlers(func(sandbox.Event), func(string, sandbox.Output)) {
}
func (r *e2e3APIFakeEngineRunner) Start(sandbox.Spec) error              { return nil }
func (r *e2e3APIFakeEngineRunner) Stop(string, time.Duration) error      { return nil }
func (r *e2e3APIFakeEngineRunner) Signal(string, syscall.Signal) error   { return nil }
func (r *e2e3APIFakeEngineRunner) State(string) sandbox.State            { return sandbox.State{} }
func (r *e2e3APIFakeEngineRunner) Stdin(string) io.Writer                { return nil }
func (r *e2e3APIFakeEngineRunner) CloseStdin(string) error               { return nil }
func (r *e2e3APIFakeEngineRunner) Resize(string, uint16, uint16) error   { return nil }
func (r *e2e3APIFakeEngineRunner) Forget(string)                         {}
func (r *e2e3APIFakeEngineRunner) Features() rungv.Features              { return rungv.Features{} }
func (r *e2e3APIFakeEngineRunner) Control(string) sandbox.Control        { return nil }
func (r *e2e3APIFakeEngineRunner) Pause(context.Context, string) error   { return nil }
func (r *e2e3APIFakeEngineRunner) Unpause(context.Context, string) error { return nil }
func (r *e2e3APIFakeEngineRunner) Update(context.Context, string, rungv.Update) (rungv.Resources, error) {
	return rungv.Resources{}, nil
}
func (r *e2e3APIFakeEngineRunner) Shutdown(string, string, time.Duration) error { return nil }
func (r *e2e3APIFakeEngineRunner) Images(context.Context) ([]rungv.Image, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]rungv.Image, 0, len(r.images))
	for _, img := range r.images {
		out = append(out, rungv.Image{
			ID:         img.id,
			Repository: img.repo,
			Tag:        img.tag,
			Size:       "1MB",
			CreatedAt:  time.Now().UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}
func (r *e2e3APIFakeEngineRunner) InspectImages(_ context.Context, refs ...string) ([]engine.ImageInspect, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []engine.ImageInspect
	for _, ref := range refs {
		for _, img := range r.images {
			if img.id == ref || (img.repo+":"+img.tag) == ref {
				var tags []string
				if img.repo != "" && img.tag != "" {
					tags = []string{img.repo + ":" + img.tag}
				}
				out = append(out, engine.ImageInspect{
					ID:           img.id,
					RepoTags:     tags,
					Created:      time.Now().UTC(),
					Architecture: runtime.GOARCH,
					Os:           "linux",
					Size:         img.size,
				})
				break
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("not found")
	}
	return out, nil
}
func (r *e2e3APIFakeEngineRunner) Pull(context.Context, string, string, func(string)) error {
	return nil
}
func (r *e2e3APIFakeEngineRunner) RemoveImage(_ context.Context, ref string, _ bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, img := range r.images {
		if img.id == ref || (img.repo+":"+img.tag) == ref {
			delete(r.images, k)
			return nil
		}
	}
	return errors.New("not found")
}
func (r *e2e3APIFakeEngineRunner) TagImage(context.Context, string, string) error { return nil }
func (r *e2e3APIFakeEngineRunner) LoadImages(context.Context, io.Reader, func(string)) error {
	return nil
}
func (r *e2e3APIFakeEngineRunner) SaveImages(context.Context, io.Writer, ...string) error { return nil }
func (r *e2e3APIFakeEngineRunner) PruneImages(context.Context) (string, error)            { return "", nil }
func (r *e2e3APIFakeEngineRunner) Volumes(context.Context) ([]rungv.Volume, error)        { return nil, nil }
func (r *e2e3APIFakeEngineRunner) CreateVolume(context.Context, string, string) error     { return nil }
func (r *e2e3APIFakeEngineRunner) RemoveVolume(context.Context, string) error             { return nil }
func (r *e2e3APIFakeEngineRunner) DiskUsage(context.Context) ([]rungv.DiskUsage, error) {
	return nil, nil
}
func (r *e2e3APIFakeEngineRunner) Info(context.Context) (rungv.Info, error) {
	return rungv.Info{}, nil
}

// =============================================================================
// Feature 17: Scoped Docker Proxy (docker_scope.go) — 5 Tier 1 + 5 Tier 2
// =============================================================================

func TestF17_T1_DockerHostEnvOnlyWhenDockerPermGranted(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	e2e3APIInstallAndStartBinaryExt(t, mgr, "no-docker-ext", e2e3APIDockerFixtureSource, `
[[contributes.command]]
id = "probe-env"
title = "Probe Env"
`)
	e2e3APIInstallAndStartBinaryExt(t, mgr, "with-docker-ext", e2e3APIDockerFixtureSource, `
[[contributes.command]]
id = "probe-env"
title = "Probe Env"
`, ext.PermDocker)

	if err := mgr.RunCommand("no-docker-ext", "probe-env", "en"); err != nil {
		t.Fatalf("RunCommand(no-docker-ext): %v", err)
	}
	if err := mgr.RunCommand("with-docker-ext", "probe-env", "en"); err != nil {
		t.Fatalf("RunCommand(with-docker-ext): %v", err)
	}

	var noRes, withRes struct {
		DockerHost string `json:"docker_host"`
	}
	e2e3APIMustReadJSONFile(t, filepath.Join(ext.FilesDir(dataDir, "no-docker-ext"), "probe_env.json"), &noRes)
	e2e3APIMustReadJSONFile(t, filepath.Join(ext.FilesDir(dataDir, "with-docker-ext"), "probe_env.json"), &withRes)

	if noRes.DockerHost != "" {
		t.Fatalf("expected empty GVX_DOCKER_HOST without docker perm, got %q", noRes.DockerHost)
	}
	if withRes.DockerHost == "" {
		t.Fatal("expected non-empty GVX_DOCKER_HOST with docker perm granted")
	}
}

func TestF17_T1_InjectsExtensionLabelOnContainerCreate(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	e2e3APIInstallAndStartBinaryExt(t, mgr, "label-ext", e2e3APIDockerFixtureSource, `
[[contributes.command]]
id = "create-plain"
title = "Create Plain"
`, ext.PermDocker)

	if err := mgr.RunCommand("label-ext", "create-plain", "en"); err != nil {
		t.Fatalf("RunCommand(create-plain): %v", err)
	}

	var res struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
		Err    string `json:"err"`
	}
	e2e3APIMustReadJSONFile(t, filepath.Join(ext.FilesDir(dataDir, "label-ext"), "create_plain.json"), &res)
	if res.Status != http.StatusCreated || res.Err != "" {
		t.Fatalf("POST /containers/create failed: status=%d err=%q body=%s", res.Status, res.Err, res.Body)
	}

	reqs := h.DockerBackend.SnapshotRequests()
	found := false
	for _, r := range reqs {
		if r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/containers/create") {
			labels, _ := r.CreatePayload["Labels"].(map[string]any)
			if labels["io.gvx.extension"] == "label-ext" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("backend did not receive injected Labels[io.gvx.extension]=label-ext; reqs=%+v", reqs)
	}
}

func TestF17_T1_AllowsBindInAllowedFolders(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	allowedDir := e2e3APINonReservedDir(t)
	subDir := filepath.Join(allowedDir, "sub")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	h.UIHost.PickFolderResult = allowedDir

	e2e3APIInstallAndStartBinaryExt(t, mgr, "bind-ok-ext", e2e3APIDockerFixtureSource, `
[[contributes.command]]
id = "pick-folder"
title = "Pick Folder"

[[contributes.command]]
id = "create-bind-allowed"
title = "Bind Allowed"
`, ext.PermDocker, ext.PermUIPickFolder)

	if err := mgr.RunCommand("bind-ok-ext", "pick-folder", "en"); err != nil {
		t.Fatalf("RunCommand(pick-folder): %v", err)
	}
	if err := os.WriteFile(filepath.Join(ext.FilesDir(dataDir, "bind-ok-ext"), "bind_target.txt"), []byte(subDir), 0o600); err != nil {
		t.Fatalf("write bind_target.txt: %v", err)
	}
	if err := mgr.RunCommand("bind-ok-ext", "create-bind-allowed", "en"); err != nil {
		t.Fatalf("RunCommand(create-bind-allowed): %v", err)
	}

	var res struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
		Err    string `json:"err"`
	}
	e2e3APIMustReadJSONFile(t, filepath.Join(ext.FilesDir(dataDir, "bind-ok-ext"), "create_bind_allowed.json"), &res)
	if res.Status != http.StatusCreated || res.Err != "" {
		t.Fatalf("allowed bind mount failed: status=%d err=%q body=%s", res.Status, res.Err, res.Body)
	}
	if len(h.DockerBackend.SnapshotRequests()) == 0 {
		t.Fatal("expected MockDockerBackend to receive bind-allowed container create")
	}
}

func TestF17_T1_AllowsMountInExtFilesDir(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	e2e3APIInstallAndStartBinaryExt(t, mgr, "mount-files-ext", e2e3APIDockerFixtureSource, `
[[contributes.command]]
id = "create-mount-files"
title = "Mount Files"
`, ext.PermDocker)

	if err := mgr.RunCommand("mount-files-ext", "create-mount-files", "en"); err != nil {
		t.Fatalf("RunCommand(create-mount-files): %v", err)
	}

	var res struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
		Err    string `json:"err"`
	}
	e2e3APIMustReadJSONFile(t, filepath.Join(ext.FilesDir(dataDir, "mount-files-ext"), "create_mount_files.json"), &res)
	if res.Status != http.StatusCreated || res.Err != "" {
		t.Fatalf("extension-data/<id>/files mount failed: status=%d err=%q body=%s", res.Status, res.Err, res.Body)
	}
	if len(h.DockerBackend.SnapshotRequests()) == 0 {
		t.Fatal("expected MockDockerBackend to receive Mounts request inside files dir")
	}
}

func TestF17_T1_ProxiesPingAndContainersList(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	e2e3APIInstallAndStartBinaryExt(t, mgr, "proxy-basic-ext", e2e3APIDockerFixtureSource, `
[[contributes.command]]
id = "ping-and-list"
title = "Ping and List"
`, ext.PermDocker)

	if err := mgr.RunCommand("proxy-basic-ext", "ping-and-list", "en"); err != nil {
		t.Fatalf("RunCommand(ping-and-list): %v", err)
	}

	var res struct {
		PingStatus int    `json:"ping_status"`
		PingBody   string `json:"ping_body"`
		ListStatus int    `json:"list_status"`
		ListBody   string `json:"list_body"`
	}
	e2e3APIMustReadJSONFile(t, filepath.Join(ext.FilesDir(dataDir, "proxy-basic-ext"), "ping_and_list.json"), &res)
	if res.PingStatus != http.StatusOK || strings.TrimSpace(res.PingBody) != "OK" {
		t.Fatalf("GET /_ping = (%d, %q), want (200, OK)", res.PingStatus, res.PingBody)
	}
	if res.ListStatus != http.StatusOK || strings.TrimSpace(res.ListBody) != "[]" {
		t.Fatalf("GET /containers/json = (%d, %q), want (200, [])", res.ListStatus, res.ListBody)
	}
	if len(h.DockerBackend.SnapshotRequests()) < 2 {
		t.Fatalf("expected >=2 proxied requests to MockDockerBackend, got %d", len(h.DockerBackend.SnapshotRequests()))
	}
}

func TestF17_T2_RejectsSpoofedExtensionLabel(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	e2e3APIInstallAndStartBinaryExt(t, mgr, "spoof-ext", e2e3APIDockerFixtureSource, `
[[contributes.command]]
id = "create-spoof-label"
title = "Spoof Label"
`, ext.PermDocker)

	if err := mgr.RunCommand("spoof-ext", "create-spoof-label", "en"); err != nil {
		t.Fatalf("RunCommand(create-spoof-label): %v", err)
	}

	var res struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	}
	e2e3APIMustReadJSONFile(t, filepath.Join(ext.FilesDir(dataDir, "spoof-ext"), "create_spoof_label.json"), &res)
	if res.Status != http.StatusBadRequest && res.Status != http.StatusForbidden {
		t.Fatalf("spoofed io.gvx.extension label status = %d, want 400 or 403; body=%s", res.Status, res.Body)
	}
	for _, r := range h.DockerBackend.SnapshotRequests() {
		if strings.Contains(r.Path, "/containers/create") {
			t.Fatalf("spoofed /containers/create request leaked to Docker backend: %+v", r)
		}
	}
}

func TestF17_T2_RejectsUnapprovedHostConfigBinds(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	e2e3APIInstallAndStartBinaryExt(t, mgr, "bad-bind-ext", e2e3APIDockerFixtureSource, `
[[contributes.command]]
id = "create-bad-binds"
title = "Bad Binds"
`, ext.PermDocker)

	if err := mgr.RunCommand("bad-bind-ext", "create-bad-binds", "en"); err != nil {
		t.Fatalf("RunCommand(create-bad-binds): %v", err)
	}

	var res struct {
		EtcStatus int    `json:"etc_status"`
		EtcBody   string `json:"etc_body"`
		SSHStatus int    `json:"ssh_status"`
		SSHBody   string `json:"ssh_body"`
	}
	e2e3APIMustReadJSONFile(t, filepath.Join(ext.FilesDir(dataDir, "bad-bind-ext"), "create_bad_binds.json"), &res)
	if res.EtcStatus < 400 || res.EtcStatus >= 500 {
		t.Fatalf("/etc bind status = %d, want 4xx; body=%s", res.EtcStatus, res.EtcBody)
	}
	if res.SSHStatus < 400 || res.SSHStatus >= 500 {
		t.Fatalf("~/.ssh bind status = %d, want 4xx; body=%s", res.SSHStatus, res.SSHBody)
	}
	for _, r := range h.DockerBackend.SnapshotRequests() {
		if strings.Contains(r.Path, "/containers/create") {
			t.Fatalf("unapproved bind mount reached Docker backend: %+v", r)
		}
	}
}

func TestF17_T2_RejectsMountTraversalAndSymlinkEscape(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))
	outsideDir := t.TempDir()

	e2e3APIInstallAndStartBinaryExt(t, mgr, "escape-mount-ext", e2e3APIDockerFixtureSource, `
[[contributes.command]]
id = "create-mount-escape"
title = "Escape Mount"
`, ext.PermDocker)

	if err := os.WriteFile(filepath.Join(ext.FilesDir(dataDir, "escape-mount-ext"), "outside_dir.txt"), []byte(outsideDir), 0o600); err != nil {
		t.Fatalf("write outside_dir.txt: %v", err)
	}
	if err := mgr.RunCommand("escape-mount-ext", "create-mount-escape", "en"); err != nil {
		t.Fatalf("RunCommand(create-mount-escape): %v", err)
	}

	var res struct {
		TravStatus int    `json:"trav_status"`
		TravBody   string `json:"trav_body"`
		SymStatus  int    `json:"sym_status"`
		SymBody    string `json:"sym_body"`
	}
	e2e3APIMustReadJSONFile(t, filepath.Join(ext.FilesDir(dataDir, "escape-mount-ext"), "create_mount_escape.json"), &res)
	if res.TravStatus < 400 || res.TravStatus >= 500 {
		t.Fatalf(".. traversal mount status = %d, want 4xx; body=%s", res.TravStatus, res.TravBody)
	}
	if res.SymStatus < 400 || res.SymStatus >= 500 {
		t.Fatalf("symlink escape mount status = %d, want 4xx; body=%s", res.SymStatus, res.SymBody)
	}
	for _, r := range h.DockerBackend.SnapshotRequests() {
		if strings.Contains(r.Path, "/containers/create") {
			t.Fatalf("escaped mount reached Docker backend: %+v", r)
		}
	}
}

func TestF17_T2_Returns503DockerJSONWhenEngineStopped(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	e2e3APIInstallAndStartBinaryExt(t, mgr, "down-engine-ext", e2e3APIDockerFixtureSource, `
[[contributes.command]]
id = "ping-unavailable"
title = "Ping Unavailable"
`, ext.PermDocker)

	h.DockerBackend.SetUnavailable(true)
	if err := mgr.RunCommand("down-engine-ext", "ping-unavailable", "en"); err != nil {
		t.Fatalf("RunCommand(ping-unavailable): %v", err)
	}

	var res struct {
		PingStatus   int    `json:"ping_status"`
		PingBody     string `json:"ping_body"`
		CreateStatus int    `json:"create_status"`
		CreateBody   string `json:"create_body"`
	}
	e2e3APIMustReadJSONFile(t, filepath.Join(ext.FilesDir(dataDir, "down-engine-ext"), "ping_unavailable.json"), &res)
	if res.PingStatus != http.StatusServiceUnavailable {
		t.Fatalf("GET /_ping when stopped = %d, want 503; body=%s", res.PingStatus, res.PingBody)
	}
	if res.CreateStatus != http.StatusServiceUnavailable {
		t.Fatalf("POST /containers/create when stopped = %d, want 503; body=%s", res.CreateStatus, res.CreateBody)
	}
	for _, rawBody := range []string{res.PingBody, res.CreateBody} {
		var errObj struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal([]byte(rawBody), &errObj); err != nil || errObj.Message == "" {
			t.Fatalf("expected Docker JSON {\"message\":...} on 503, got %q (err=%v)", rawBody, err)
		}
	}
}

func TestF17_T2_RevokingDockerPermBlocksProxy(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	e2e3APIInstallAndStartBinaryExt(t, mgr, "revoke-docker-ext", e2e3APIDockerFixtureSource, `
[[contributes.command]]
id = "probe-env"
title = "Probe Env"

[[contributes.command]]
id = "ping-and-list"
title = "Ping and List"
`, ext.PermDocker)

	if err := mgr.RunCommand("revoke-docker-ext", "probe-env", "en"); err != nil {
		t.Fatalf("RunCommand(probe-env): %v", err)
	}
	var envRes struct {
		DockerHost string `json:"docker_host"`
	}
	e2e3APIMustReadJSONFile(t, filepath.Join(ext.FilesDir(dataDir, "revoke-docker-ext"), "probe_env.json"), &envRes)
	if envRes.DockerHost == "" {
		t.Fatal("expected GVX_DOCKER_HOST before revocation")
	}

	if err := mgr.RevokePermissions("revoke-docker-ext"); err != nil {
		t.Fatalf("RevokePermissions: %v", err)
	}
	info := e2e3APIWaitForExtState(t, mgr, "revoke-docker-ext", ext.StateWaiting, 5*time.Second)
	if len(info.Granted) != 0 {
		t.Fatalf("expected 0 granted permissions after revoke, got %v", info.Granted)
	}

	client, base := e2e3APIHostEndpointClient(t, envRes.DockerHost)
	req, _ := http.NewRequest(http.MethodGet, base+"/_ping", nil)
	resp, err := client.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("expected revoked Docker proxy to stop or reject (403/503), got 200 OK")
		}
	}
}

// =============================================================================
// Feature 18: Bidirectional Host/Extension HTTP+JSON+SSE API — 5 T1 + 5 T2
// =============================================================================

func TestF18_T1_HostInfoReturnsExpectedFields(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	want := ext.HostInfo{
		AppVersion:   "0.11.0-e2e",
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		Locale:       "ko",
		Theme:        "light",
		DockerMode:   "sandbox",
		DockerReady:  true,
		RungvVersion: "0.21.5",
	}
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true), WithHostInfo(want))

	e2e3APIInstallAndStartBinaryExt(t, mgr, "info-ext", e2e3APIFullHostFixtureSource, `
[[contributes.command]]
id = "check-info"
title = "Check Info"
`)

	if err := mgr.RunCommand("info-ext", "check-info", "ko"); err != nil {
		t.Fatalf("RunCommand(check-info): %v", err)
	}

	var got extsdk.HostInfo
	e2e3APIMustReadJSONFile(t, filepath.Join(ext.FilesDir(dataDir, "info-ext"), "host_info.json"), &got)
	if got.AppVersion != want.AppVersion || got.OS != want.OS || got.Arch != want.Arch ||
		got.Locale != want.Locale || got.Theme != want.Theme || got.DockerMode != want.DockerMode ||
		got.DockerReady != want.DockerReady || got.RungvVersion != want.RungvVersion {
		t.Fatalf("HostInfo mismatch: got %+v, want %+v", got, want)
	}
}

func TestF18_T1_SettingsAndChangedNotification(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	extraTOML := `
[contributes.settings]
api_url = { type = "string", label = "API URL", default = "https://default.invalid" }
api_token = { type = "secret", label = "API Token" }
enabled = { type = "boolean", label = "Enabled", default = true }
`
	e2e3APIInstallAndStartBinaryExt(t, mgr, "settings-ext", e2e3APIFullHostFixtureSource, extraTOML, ext.PermSecrets)

	if err := mgr.SetSetting("settings-ext", "api_url", "https://updated.invalid"); err != nil {
		t.Fatalf("SetSetting(api_url): %v", err)
	}
	if err := mgr.SetSetting("settings-ext", "api_token", "top-secret-token-value"); err != nil {
		t.Fatalf("SetSetting(api_token): %v", err)
	}

	var res struct {
		APIURL            string         `json:"api_url"`
		Enabled           bool           `json:"enabled"`
		APITokenIsSet     bool           `json:"api_token_is_set"`
		APITokenPlaintext string         `json:"api_token_plaintext"`
		RawToken          map[string]any `json:"raw_token"`
	}
	settingsChangedPath := filepath.Join(ext.FilesDir(dataDir, "settings-ext"), "settings_changed.json")
	deadline := time.Now().Add(3 * time.Second)
	var lastRaw []byte
	var lastErr error
	matched := false
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(settingsChangedPath)
		if err != nil {
			lastErr = err
		} else if len(bytes.TrimSpace(b)) > 0 {
			lastRaw = b
			var cand struct {
				APIURL            string         `json:"api_url"`
				Enabled           bool           `json:"enabled"`
				APITokenIsSet     bool           `json:"api_token_is_set"`
				APITokenPlaintext string         `json:"api_token_plaintext"`
				RawToken          map[string]any `json:"raw_token"`
			}
			if uerr := json.Unmarshal(b, &cand); uerr != nil {
				lastErr = uerr
			} else if cand.APIURL == "https://updated.invalid" && cand.APITokenIsSet {
				res = cand
				matched = true
				break
			} else {
				res = cand
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !matched {
		t.Fatalf("timed out waiting for settings_changed.json with api_url and api_token set (lastErr=%v raw=%s parsed=%+v)", lastErr, string(lastRaw), res)
	}
	if !res.Enabled {
		t.Fatal("expected enabled default true")
	}
	if res.APITokenPlaintext != "" {
		t.Fatalf("secret setting not properly masked: isSet=%v plaintext=%q", res.APITokenIsSet, res.APITokenPlaintext)
	}
	if res.RawToken["set"] != true {
		t.Fatalf("expected raw masked secret {\"set\":true}, got %+v", res.RawToken)
	}
}

func TestF18_T1_HostEventsSSEDelivery(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	e2e3APIInstallAndStartBinaryExt(t, mgr, "events-ext", e2e3APIFullHostFixtureSource, "")

	// Give the background /v1/events SSE subscriber a moment to connect, then emit events.
	time.Sleep(150 * time.Millisecond)
	mgr.NotifyHostEvent("host.theme", map[string]string{"theme": "light"})
	mgr.NotifyHostEvent("host.locale", map[string]string{"locale": "ko"})
	mgr.NotifyHostEvent("docker.state", map[string]any{"ready": true})
	mgr.NotifyHostEvent("settings.changed", map[string]string{"key": "mode"})

	eventsPath := filepath.Join(ext.FilesDir(dataDir, "events-ext"), "host_events.json")
	deadline := time.Now().Add(5 * time.Second)
	wantTypes := []string{"host.theme", "host.locale", "docker.state", "settings.changed"}
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(eventsPath); err == nil {
			var got []extsdk.HostEvent
			if json.Unmarshal(b, &got) == nil {
				seen := map[string]bool{}
				for _, ev := range got {
					seen[ev.Type] = true
				}
				allPresent := true
				for _, wt := range wantTypes {
					if !seen[wt] {
						allPresent = false
						break
					}
				}
				if allPresent {
					return
				}
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	raw, _ := os.ReadFile(eventsPath)
	t.Fatalf("timed out waiting for host SSE events %v; recorded=%s", wantTypes, string(raw))
}

func TestF18_T1_ViewStreamEventCommandAndToolRoundtrip(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	e2e3APIInstallAndStartBinaryExt(t, mgr, "roundtrip-ext", e2e3APIFullHostFixtureSource, `
[[contributes.command]]
id = "record-cmd"
title = "Record Cmd"

[[contributes.tool]]
id = "echo"
description = "Echo back message"
[contributes.tool.params]
msg = { type = "string", description = "Message", required = true }
`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	updates, err := mgr.SubscribeView(ctx, "roundtrip-ext", "main", "en")
	if err != nil {
		t.Fatalf("SubscribeView: %v", err)
	}

	var gotInitial bool
	deadline := time.After(3 * time.Second)
	for !gotInitial {
		select {
		case u, ok := <-updates:
			if !ok {
				t.Fatal("SubscribeView exited early before initial tree")
			}
			if u.Tree != nil {
				b, _ := json.Marshal(u.Tree)
				if strings.Contains(string(b), "count=0") {
					gotInitial = true
				}
			}
		case <-deadline:
			t.Fatal("timed out waiting for initial uiblock tree from SubscribeView")
		}
	}

	if err := mgr.SendViewEvent("roundtrip-ext", "main", uiblock.Event{
		Node:   "/inc",
		Kind:   uiblock.EventTap,
		Locale: "en",
	}); err != nil {
		t.Fatalf("SendViewEvent(/inc): %v", err)
	}

	var gotUpdated bool
	deadline = time.After(3 * time.Second)
	for !gotUpdated {
		select {
		case u, ok := <-updates:
			if !ok {
				t.Fatal("SubscribeView exited early before count=1 update")
			}
			b, _ := json.Marshal(u)
			if strings.Contains(string(b), "count=1") {
				gotUpdated = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for count=1 update after SendViewEvent")
		}
	}

	if err := mgr.RunCommand("roundtrip-ext", "record-cmd", "en"); err != nil {
		t.Fatalf("RunCommand(record-cmd): %v", err)
	}
	var cmdRes struct {
		Cmd     string `json:"cmd"`
		Gesture string `json:"gesture"`
	}
	e2e3APIMustReadJSONFile(t, filepath.Join(ext.FilesDir(dataDir, "roundtrip-ext"), "cmd_ran.json"), &cmdRes)
	if cmdRes.Cmd != "record-cmd" || cmdRes.Gesture == "" {
		t.Fatalf("expected command record-cmd with non-empty gesture, got %+v", cmdRes)
	}

	// Verify /v1/tools/echo over the Extension API listener using the host-assigned token/endpoint.
	var ep struct {
		ExtListen string `json:"ext_listen"`
		ExtToken  string `json:"ext_token"`
	}
	e2e3APIMustReadJSONFile(t, filepath.Join(ext.FilesDir(dataDir, "roundtrip-ext"), "endpoints.json"), &ep)
	if ep.ExtListen == "" || ep.ExtToken == "" {
		t.Fatalf("expected GVX_EXT_LISTEN and GVX_EXT_TOKEN in endpoints.json, got %+v", ep)
	}
	extClient, extBase := e2e3APIHostEndpointClient(t, ep.ExtListen)
	st, _, _ := httpDoJSON(t, extClient, http.MethodPost, extBase+"/v1/tools/echo", "", "", map[string]any{"args": map[string]any{"msg": "ping"}}, nil)
	if st != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /v1/tools/echo = %d, want 401 (confirming live Ext API listener)", st)
	}
	var toolOut map[string]any
	st, _, raw := httpDoJSON(t, extClient, http.MethodPost, extBase+"/v1/tools/echo", ep.ExtToken, "", map[string]any{"args": map[string]any{"msg": "ping"}}, &toolOut)
	if st != http.StatusOK || toolOut["echo"] != "ping" {
		t.Fatalf("authenticated POST /v1/tools/echo = (%d, %s), want (200, echo=ping)", st, string(raw))
	}
}

func TestF18_T1_SandboxesAndSnapshotsOwnedScope(t *testing.T) {
	binPath, _, _ := requireRungv(t)

	dataDir := newShortTempDataDir(t)
	supDir := newShortTempDataDir(t)
	cacheDir := t.TempDir()
	sup := &sandbox.Supervisor{
		Client: func() *rungv.Client {
			return &rungv.Client{Path: binPath, CacheDir: cacheDir}
		},
		DataDir: supDir,
	}
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true), WithSupervisor(sup))

	e2e3APIInstallAndStartBinaryExt(t, mgr, "sbx-owner-ext", e2e3APIFullHostFixtureSource, `
[[contributes.command]]
id = "owned-sandbox-lifecycle"
title = "Owned Sandbox"
`, ext.PermSandboxes, ext.PermSnapshots)

	if err := mgr.RunCommand("sbx-owner-ext", "owned-sandbox-lifecycle", "en"); err != nil {
		t.Fatalf("RunCommand(owned-sandbox-lifecycle): %v", err)
	}
	var res struct {
		OK       bool   `json:"ok"`
		Step     string `json:"step"`
		Err      string `json:"err"`
		ID       string `json:"id"`
		ListLen  int    `json:"list_len"`
		SnapName string `json:"snap_name"`
		SnapsLen int    `json:"snaps_len"`
	}
	e2e3APIMustReadJSONFile(t, filepath.Join(ext.FilesDir(dataDir, "sbx-owner-ext"), "owned_sandbox.json"), &res)
	if !res.OK {
		t.Fatalf("owned sandbox/snapshot lifecycle failed at step %q: %s", res.Step, res.Err)
	}
	if res.ListLen < 1 || res.SnapName != "cp1" || res.SnapsLen < 1 {
		t.Fatalf("unexpected sandbox/snapshot result: %+v", res)
	}
}

func TestF18_T2_Unauthorized401OnHostAndExtAPI(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	e2e3APIInstallAndStartBinaryExt(t, mgr, "auth-check-ext", e2e3APIFullHostFixtureSource, `
[[contributes.command]]
id = "record-cmd"
title = "Record Cmd"

[[contributes.tool]]
id = "echo"
description = "Echo"
`)

	var ep struct {
		HostAPI   string `json:"host_api"`
		ExtListen string `json:"ext_listen"`
	}
	e2e3APIMustReadJSONFile(t, filepath.Join(ext.FilesDir(dataDir, "auth-check-ext"), "endpoints.json"), &ep)

	hostClient, hostBase := e2e3APIHostEndpointClient(t, ep.HostAPI)
	for _, tc := range [][2]string{
		{http.MethodGet, "/v1/host/info"},
		{http.MethodGet, "/v1/settings"},
		{http.MethodGet, "/v1/storage"},
		{http.MethodGet, "/v1/events"},
		{http.MethodPost, "/v1/ui/badge"},
	} {
		for _, tok := range []string{"", "invalid-host-token"} {
			st, _, _ := httpDoJSON(t, hostClient, tc[0], hostBase+tc[1], tok, "", nil, nil)
			if st != http.StatusUnauthorized {
				t.Fatalf("Host API %s %s with token %q = %d, want 401", tc[0], tc[1], tok, st)
			}
		}
	}

	extClient, extBase := e2e3APIHostEndpointClient(t, ep.ExtListen)
	for _, tc := range [][2]string{
		{http.MethodGet, "/v1/health"},
		{http.MethodGet, "/v1/views/main/stream"},
		{http.MethodPost, "/v1/views/main/event"},
		{http.MethodPost, "/v1/commands/record-cmd"},
		{http.MethodPost, "/v1/tools/echo"},
		{http.MethodPost, "/v1/shutdown"},
	} {
		for _, tok := range []string{"", "invalid-ext-token"} {
			st, _, _ := httpDoJSON(t, extClient, tc[0], extBase+tc[1], tok, "", nil, nil)
			if st != http.StatusUnauthorized {
				t.Fatalf("Ext API %s %s with token %q = %d, want 401", tc[0], tc[1], tok, st)
			}
		}
	}
}

func TestF18_T2_Forbidden403OnUnownedSandboxAndSnapshot(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	sup := &sandbox.Supervisor{DataDir: newShortTempDataDir(t)}
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true), WithSupervisor(sup))

	e2e3APIInstallAndStartBinaryExt(t, mgr, "unowned-sbx-ext", e2e3APIFullHostFixtureSource, `
[[contributes.command]]
id = "unowned-sandbox"
title = "Unowned Sandbox"
`, ext.PermSandboxes, ext.PermSnapshots)

	if err := mgr.RunCommand("unowned-sbx-ext", "unowned-sandbox", "en"); err != nil {
		t.Fatalf("RunCommand(unowned-sandbox): %v", err)
	}
	var res map[string]bool
	e2e3APIMustReadJSONFile(t, filepath.Join(ext.FilesDir(dataDir, "unowned-sbx-ext"), "unowned_sandbox.json"), &res)
	for k, ok := range res {
		if !ok {
			t.Fatalf("expected 403 Forbidden for unowned sandbox/snapshot op %q, got false (%+v)", k, res)
		}
	}
}

func TestF18_T2_MountDockerSockRequiresDockerPerm(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	sup := &sandbox.Supervisor{DataDir: newShortTempDataDir(t)}
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true), WithSupervisor(sup))

	// Granted sandboxes, NOT docker.
	e2e3APIInstallAndStartBinaryExt(t, mgr, "no-sock-perm-ext", e2e3APIFullHostFixtureSource, `
[[contributes.command]]
id = "mount-sock-denied"
title = "Mount Sock Denied"
`, ext.PermSandboxes)

	if err := mgr.RunCommand("no-sock-perm-ext", "mount-sock-denied", "en"); err != nil {
		t.Fatalf("RunCommand(mount-sock-denied): %v", err)
	}
	var res struct {
		Denied bool   `json:"denied"`
		Err    string `json:"err"`
	}
	e2e3APIMustReadJSONFile(t, filepath.Join(ext.FilesDir(dataDir, "no-sock-perm-ext"), "mount_sock_denied.json"), &res)
	if !res.Denied {
		t.Fatalf("expected 403 permission denied for mount_docker_sock without docker perm, got err=%q", res.Err)
	}
}

func TestF18_T2_InvalidUIBlockTreeDoesNotCrashHost(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	badUISource := `package main

import (
	"context"

	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

func main() {
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			// Raw hex color in Tone is forbidden by uiblock.Read.
			return ui.Column(ui.Text("bad", extsdk.Tone("#ff0000")))
		},
	})
	_ = app.Run(context.Background())
}
`
	e2e3APIInstallAndStartBinaryExt(t, mgr, "bad-ui-ext", badUISource, "")
	e2e3APIInstallAndStartBinaryExt(t, mgr, "good-ui-ext", e2e3APIHealthySource, "")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var badErrMsg string
	subCtx, subCancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer subCancel()
	badCh, err := mgr.SubscribeView(subCtx, "bad-ui-ext", "main", "en")
	if err == nil {
		for u := range badCh {
			if u.Error != "" {
				badErrMsg = u.Error
				subCancel()
				break
			}
		}
	}
	if err == nil && badErrMsg == "" {
		t.Fatal("expected invalid uiblock tree to produce an error in SubscribeView or uiblock.Update.Error")
	}

	// Verify the healthy sibling extension remains unaffected and serves its view cleanly.
	goodCtx, goodCancel := context.WithTimeout(ctx, 2*time.Second)
	defer goodCancel()
	goodUpdates, goodErr := mgr.SubscribeView(goodCtx, "good-ui-ext", "main", "en")
	if goodErr != nil {
		t.Fatalf("healthy sibling SubscribeView: %v", goodErr)
	}
	select {
	case u, ok := <-goodUpdates:
		if !ok || u.Tree == nil || u.Error != "" {
			t.Fatalf("healthy sibling extension affected by bad UI: ok=%v update=%+v", ok, u)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("healthy sibling extension failed to deliver view tree")
	}
}

func TestF18_T2_NotFound404AndBadRequest400(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	e2e3APIInstallAndStartBinaryExt(t, mgr, "err-codes-ext", e2e3APIFullHostFixtureSource, `
[[contributes.command]]
id = "record-cmd"
title = "Record Cmd"
`)

	if err := mgr.RunCommand("err-codes-ext", "does-not-exist", "en"); err == nil {
		t.Fatal("expected error for unknown command via mgr.RunCommand")
	}

	// Also verify direct Extension API 404/400 behavior on unknown routes & malformed JSON
	// using a live extsdk.App instance bound in-process.
	sockDir := newShortTempDataDir(t)
	sockPath := filepath.Join(sockDir, "e-codes.sock")
	app := extsdk.New()
	app.View("main", extsdk.View{
		Render: func(ctx *extsdk.ViewContext, ui *extsdk.UI) extsdk.Node {
			return ui.Column(ui.Text("ok"))
		},
	})
	app.Command("known", func(ctx *extsdk.CommandContext) error { return nil })

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = app.RunWithConfig(runCtx, extsdk.Config{
			ExtID:      "codes-sdk",
			ListenAddr: "unix://" + sockPath,
			ExtToken:   "tok-codes",
		})
	}()

	cl := newUnixHTTPClient(sockPath)
	deadline := time.Now().Add(3 * time.Second)
	ready := false
	var lastPollErr error
	for time.Now().Before(deadline) {
		req, reqErr := http.NewRequest(http.MethodGet, "http://unix/v1/health", nil)
		if reqErr != nil {
			t.Fatalf("http.NewRequest: %v", reqErr)
		}
		req.Header.Set("Authorization", "Bearer tok-codes")
		resp, err := cl.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ready = true
				break
			}
			lastPollErr = fmt.Errorf("status=%d", resp.StatusCode)
		} else {
			lastPollErr = err
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("extension API did not become ready at %s within deadline: %v", sockPath, lastPollErr)
	}

	for _, nf := range [][2]string{
		{http.MethodGet, "/v1/views/unknown/stream"},
		{http.MethodPost, "/v1/views/unknown/event"},
		{http.MethodPost, "/v1/commands/unknown"},
		{http.MethodPost, "/v1/tools/unknown"},
	} {
		st, _ := e2e3APIDoRawJSON(t, cl, nf[0], "http://unix"+nf[1], "tok-codes", `{}`)
		if st != http.StatusNotFound {
			t.Fatalf("%s %s = %d, want 404", nf[0], nf[1], st)
		}
	}

	for _, br := range []string{"/v1/views/main/event", "/v1/commands/known", "/v1/settings/changed"} {
		st, _ := e2e3APIDoRawJSON(t, cl, http.MethodPost, "http://unix"+br, "tok-codes", `{not-valid-json`)
		if st != http.StatusBadRequest {
			t.Fatalf("POST %s malformed JSON = %d, want 400", br, st)
		}
	}
}

// =============================================================================
// Feature 19: Gesture Gating (X-GVX-Gesture-ID) & UI Bridge — 5 T1 + 5 T2
// =============================================================================

func TestF19_T1_BadgeUpdatesExtensionInfo(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	e2e3APIInstallAndStartBinaryExt(t, mgr, "badge-ext", e2e3APIFullHostFixtureSource, `
[[contributes.command]]
id = "set-badge"
title = "Set Badge"
`)

	since := h.UIHost.DrainChanged()
	if err := mgr.RunCommand("badge-ext", "set-badge", "en"); err != nil {
		t.Fatalf("RunCommand(set-badge): %v", err)
	}
	h.UIHost.WaitChangedAfter(t, since, 3*time.Second)

	info, ok := e2e3APIFindExtInfo(t, mgr, "badge-ext")
	if !ok {
		t.Fatal("badge-ext not found in mgr.List()")
	}
	if info.Badge.Text != "3" || info.Badge.Tone != "accent" || info.Badge.Tooltip != "3 active" {
		t.Fatalf("unexpected ExtensionInfo.Badge: %+v", info.Badge)
	}
}

func TestF19_T1_ToastInvokesUIHost(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	// ui:toast is implicit (not listed in permissions).
	e2e3APIInstallAndStartBinaryExt(t, mgr, "toast-ext", e2e3APIFullHostFixtureSource, `
[[contributes.command]]
id = "send-toast"
title = "Send Toast"
`)

	if err := mgr.RunCommand("toast-ext", "send-toast", "en"); err != nil {
		t.Fatalf("RunCommand(send-toast): %v", err)
	}
	toasts := h.UIHost.SnapshotToasts()
	if len(toasts) != 1 || toasts[0].Level != "info" || toasts[0].Message != "hello from ext" {
		t.Fatalf("unexpected UIHost toasts: %+v", toasts)
	}
}

func TestF19_T1_GesturePickFolderUpdatesAllowedFolders(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	pickedDir := e2e3APINonReservedDir(t)
	h.UIHost.PickFolderResult = pickedDir

	e2e3APIInstallAndStartBinaryExt(t, mgr, "pick-ext", e2e3APIFullHostFixtureSource, "", ext.PermUIPickFolder)

	if err := mgr.SendViewEvent("pick-ext", "main", uiblock.Event{
		Node:   "/pick",
		Kind:   uiblock.EventTap,
		Locale: "en",
	}); err != nil {
		t.Fatalf("SendViewEvent(/pick): %v", err)
	}

	var res struct {
		Dir string `json:"dir"`
		OK  bool   `json:"ok"`
		Err string `json:"err"`
	}
	e2e3APIMustReadJSONFile(t, filepath.Join(ext.FilesDir(dataDir, "pick-ext"), "view_pick.json"), &res)
	if !res.OK || res.Dir != pickedDir || res.Err != "" {
		t.Fatalf("PickFolder returned %+v, want dir=%q ok=true", res, pickedDir)
	}
	if h.UIHost.SnapshotPickFolderCalls() != 1 {
		t.Fatalf("UIHost.PickFolderCalls = %d, want 1", h.UIHost.SnapshotPickFolderCalls())
	}

	info, ok := e2e3APIFindExtInfo(t, mgr, "pick-ext")
	if !ok || !slices.Contains(info.AllowedFolders, pickedDir) {
		t.Fatalf("ExtensionInfo.AllowedFolders = %v, want %q", info.AllowedFolders, pickedDir)
	}
	grantsBytes, err := os.ReadFile(ext.GrantsPath(dataDir))
	if err != nil || !strings.Contains(string(grantsBytes), pickedDir) {
		t.Fatalf("extension-grants.json missing %q (err=%v): %s", pickedDir, err, string(grantsBytes))
	}
}

func TestF19_T1_GestureOpenTerminalAndOpenURL(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))
	h.DockerBackend.SeedContainer("ctr-1", "term-url-ext")

	e2e3APIInstallAndStartBinaryExt(t, mgr, "term-url-ext", e2e3APIFullHostFixtureSource, `
[[contributes.command]]
id = "open-term-and-url"
title = "Open Term and URL"
`, ext.PermUITerminal, ext.PermUIOpenURL, ext.PermDocker)

	if err := mgr.RunCommand("term-url-ext", "open-term-and-url", "en"); err != nil {
		t.Fatalf("RunCommand(open-term-and-url): %v", err)
	}
	terms := h.UIHost.SnapshotTerminals()
	if len(terms) != 1 || terms[0].TargetType != "container" || terms[0].TargetID != "ctr-1" || terms[0].Shell != "/bin/sh" {
		t.Fatalf("unexpected UIHost terminals: %+v", terms)
	}
	urls := h.UIHost.SnapshotOpenURLs()
	if len(urls) != 1 || urls[0] != "https://example.invalid/docs" {
		t.Fatalf("unexpected UIHost openURLs: %+v", urls)
	}
}

func TestF19_T1_GestureCopyText(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	e2e3APIInstallAndStartBinaryExt(t, mgr, "copy-ext", e2e3APIFullHostFixtureSource, `
[[contributes.command]]
id = "copy-text"
title = "Copy Text"
`)

	if err := mgr.RunCommand("copy-ext", "copy-text", "en"); err != nil {
		t.Fatalf("RunCommand(copy-text): %v", err)
	}
	copied := h.UIHost.SnapshotCopied()
	if len(copied) != 1 || copied[0] != "copied-from-gesture" {
		t.Fatalf("unexpected UIHost copied: %+v", copied)
	}
}

func TestF19_T2_RejectsMissingUnknownAndExpiredGesture(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	e2e3APIInstallAndStartBinaryExt(t, mgr, "bad-gesture-ext", e2e3APIFullHostFixtureSource, `
[[contributes.command]]
id = "capture-gesture"
title = "Capture Gesture"

[[contributes.command]]
id = "test-bad-gestures"
title = "Test Bad Gestures"
`, ext.PermUIOpenURL, ext.PermUIPickFolder)

	if err := mgr.RunCommand("bad-gesture-ext", "capture-gesture", "en"); err != nil {
		t.Fatalf("RunCommand(capture-gesture): %v", err)
	}
	if err := mgr.RunCommand("bad-gesture-ext", "test-bad-gestures", "en"); err != nil {
		t.Fatalf("RunCommand(test-bad-gestures): %v", err)
	}

	var res struct {
		MissingURL bool   `json:"missing_url_gesture_req"`
		MissingCp  bool   `json:"missing_copy_gesture_req"`
		Unknown    bool   `json:"unknown_gesture_req"`
		Expired    bool   `json:"expired_gesture_req"`
		MissingErr string `json:"missing_url_err"`
		UnknownErr string `json:"unknown_err"`
		ExpiredErr string `json:"expired_err"`
	}
	e2e3APIMustReadJSONFile(t, filepath.Join(ext.FilesDir(dataDir, "bad-gesture-ext"), "bad_gestures.json"), &res)
	if !res.MissingURL || !res.MissingCp || !res.Unknown || !res.Expired {
		t.Fatalf("expected all missing/unknown/expired gesture calls to return 403 gesture error: %+v", res)
	}
	if len(h.UIHost.SnapshotOpenURLs()) != 0 || len(h.UIHost.SnapshotCopied()) != 0 {
		t.Fatalf("expected 0 UIHost side effects for rejected gestures, got urls=%v copied=%v", h.UIHost.SnapshotOpenURLs(), h.UIHost.SnapshotCopied())
	}
}

func TestF19_T2_EnforcesMax8IntentsAndBlocksReplay(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	e2e3APIInstallAndStartBinaryExt(t, mgr, "limit-gesture-ext", e2e3APIFullHostFixtureSource, `
[[contributes.command]]
id = "test-9-intents"
title = "Test 9 Intents"

[[contributes.command]]
id = "replay-captured"
title = "Replay Captured"
`)

	if err := mgr.RunCommand("limit-gesture-ext", "test-9-intents", "en"); err != nil {
		t.Fatalf("RunCommand(test-9-intents): %v", err)
	}
	var res struct {
		OKCount  int    `json:"ok_count"`
		NinthErr string `json:"ninth_err"`
		Ninth403 bool   `json:"ninth_403"`
	}
	e2e3APIMustReadJSONFile(t, filepath.Join(ext.FilesDir(dataDir, "limit-gesture-ext"), "nine_intents.json"), &res)
	if res.OKCount != 8 || !res.Ninth403 {
		t.Fatalf("expected 8 intents to succeed and 9th to fail 403, got %+v", res)
	}

	if err := mgr.RunCommand("limit-gesture-ext", "replay-captured", "en"); err != nil {
		t.Fatalf("RunCommand(replay-captured): %v", err)
	}
	var rep struct {
		Err     string `json:"err"`
		Blocked bool   `json:"blocked"`
	}
	e2e3APIMustReadJSONFile(t, filepath.Join(ext.FilesDir(dataDir, "limit-gesture-ext"), "replay_result.json"), &rep)
	if !rep.Blocked {
		t.Fatalf("expected replayed gesture ID to be blocked 403, got %+v", rep)
	}
	if len(h.UIHost.SnapshotCopied()) != 8 {
		t.Fatalf("expected exactly 8 copied entries, got %d", len(h.UIHost.SnapshotCopied()))
	}
}

func TestF19_T2_RejectsMissingUIPermissions(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	// No ui:pick-folder, ui:terminal, ui:open-url granted.
	e2e3APIInstallAndStartBinaryExt(t, mgr, "no-ui-perms-ext", e2e3APIFullHostFixtureSource, `
[[contributes.command]]
id = "test-missing-ui-perms"
title = "Missing UI Perms"
`)

	if err := mgr.RunCommand("no-ui-perms-ext", "test-missing-ui-perms", "en"); err != nil {
		t.Fatalf("RunCommand(test-missing-ui-perms): %v", err)
	}
	var res struct {
		Pick403 bool `json:"pick_403"`
		Term403 bool `json:"term_403"`
		URL403  bool `json:"url_403"`
	}
	e2e3APIMustReadJSONFile(t, filepath.Join(ext.FilesDir(dataDir, "no-ui-perms-ext"), "missing_ui_perms.json"), &res)
	if !res.Pick403 || !res.Term403 || !res.URL403 {
		t.Fatalf("expected 403 for all missing ui permissions, got %+v", res)
	}
	if h.UIHost.SnapshotPickFolderCalls() != 0 || len(h.UIHost.SnapshotTerminals()) != 0 || len(h.UIHost.SnapshotOpenURLs()) != 0 {
		t.Fatal("expected zero UIHost calls when permissions are missing")
	}
}

func TestF19_T2_OpenURLRejectsUnsafeSchemesAndUserinfo(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	e2e3APIInstallAndStartBinaryExt(t, mgr, "unsafe-url-ext", e2e3APIFullHostFixtureSource, `
[[contributes.command]]
id = "test-unsafe-urls"
title = "Unsafe URLs"
`, ext.PermUIOpenURL)

	if err := mgr.RunCommand("unsafe-url-ext", "test-unsafe-urls", "en"); err != nil {
		t.Fatalf("RunCommand(test-unsafe-urls): %v", err)
	}
	var res map[string]string
	e2e3APIMustReadJSONFile(t, filepath.Join(ext.FilesDir(dataDir, "unsafe-url-ext"), "unsafe_urls.json"), &res)
	for rawURL, outcome := range res {
		if outcome == "OK" {
			t.Fatalf("unsafe URL %q was allowed by /v1/ui/open-url", rawURL)
		}
	}
	if len(h.UIHost.SnapshotOpenURLs()) != 0 {
		t.Fatalf("expected 0 OpenURL invocations for unsafe URLs, got %v", h.UIHost.SnapshotOpenURLs())
	}
}

func TestF19_T2_CopyLimitAndToastRateLimit(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	e2e3APIInstallAndStartBinaryExt(t, mgr, "limits-ext", e2e3APIFullHostFixtureSource, `
[[contributes.command]]
id = "test-copy-and-toast-limits"
title = "Copy and Toast Limits"
`)

	if err := mgr.RunCommand("limits-ext", "test-copy-and-toast-limits", "en"); err != nil {
		t.Fatalf("RunCommand(test-copy-and-toast-limits): %v", err)
	}
	var res struct {
		BigErr     string `json:"big_err"`
		UTF8Err    string `json:"utf8_err"`
		ToastOK    int    `json:"toast_ok"`
		Toast13429 bool   `json:"toast13_429"`
		Toast13Err string `json:"toast13_err"`
	}
	e2e3APIMustReadJSONFile(t, filepath.Join(ext.FilesDir(dataDir, "limits-ext"), "copy_toast_limits.json"), &res)
	if !strings.Contains(res.BigErr, "400") || !strings.Contains(res.UTF8Err, "400") {
		t.Fatalf("expected 400 for >64 KiB and invalid UTF-8 Copy, got big=%q utf8=%q", res.BigErr, res.UTF8Err)
	}
	if res.ToastOK != 12 || !res.Toast13429 {
		t.Fatalf("expected 12 toasts to succeed and 13th to return 429, got ok=%d 429=%v err=%q", res.ToastOK, res.Toast13429, res.Toast13Err)
	}
	if len(h.UIHost.SnapshotCopied()) != 0 {
		t.Fatalf("expected 0 copied items from rejected copy calls, got %d", len(h.UIHost.SnapshotCopied()))
	}
	if len(h.UIHost.SnapshotToasts()) != 12 {
		t.Fatalf("expected 12 recorded toasts, got %d", len(h.UIHost.SnapshotToasts()))
	}
}

// =============================================================================
// Feature 20: KeepImages Protection & idle_timeout_sec — 5 Tier 1 + 5 Tier 2
// =============================================================================

func TestF20_T1_KeepImagesIncludesBuiltAndPulledActiveImages(t *testing.T) {
	mgr, h := newTestManager(t)

	localDir := createLocalExtensionDir(t, t.TempDir(), "built-ext", minimalManifestTOML("built-ext", "1.2.3"), nil)
	if _, err := mgr.InstallLocal(localDir); err != nil {
		t.Fatalf("InstallLocal(built-ext): %v", err)
	}
	_ = mgr.AllowAndBuild("built-ext")
	if len(h.ImageBuilder.SnapshotCalls()) != 1 {
		t.Fatalf("expected MockImageBuilder to build built-ext, got %d calls", len(h.ImageBuilder.SnapshotCalls()))
	}

	pulledManifest := `schema_version = 1
id = "pulled-ext"
version = "2.0.0"
publisher = "acme"
name = "Pulled Ext"
description = "Prebuilt image extension"

[runtime]
type = "sandbox"
dockerfile = ""
image = "ghcr.io/acme/pulled-ext:2.0.0"
`
	pulledDir := writeExtFixture(t, filepath.Join(t.TempDir(), "pulled-ext"), pulledManifest, nil)
	if _, err := mgr.InstallLocal(pulledDir); err != nil {
		t.Fatalf("InstallLocal(pulled-ext): %v", err)
	}
	_ = mgr.AllowAndBuild("pulled-ext")
	if !slices.Contains(h.ImagePuller.SnapshotPulledRefs(), "ghcr.io/acme/pulled-ext:2.0.0") {
		t.Fatalf("expected MockImagePuller to pull ghcr.io/acme/pulled-ext:2.0.0, got %v", h.ImagePuller.SnapshotPulledRefs())
	}

	kept := mgr.KeepImages()
	wantBuilt := ext.ExtensionImageTag("built-ext", "1.2.3", "")
	wantPulled := "ghcr.io/acme/pulled-ext:2.0.0"
	if !slices.Contains(kept, wantBuilt) {
		t.Fatalf("KeepImages() = %v, missing built image %q", kept, wantBuilt)
	}
	if !slices.Contains(kept, wantPulled) {
		t.Fatalf("KeepImages() = %v, missing pulled image %q", kept, wantPulled)
	}
}

func TestF20_T1_EnginePruneProtectsActiveExtensionImages(t *testing.T) {
	mgr, _ := newTestManager(t)

	localDir := createLocalExtensionDir(t, t.TempDir(), "prune-keep-ext", minimalManifestTOML("prune-keep-ext", "0.5.0"), nil)
	if _, err := mgr.InstallLocal(localDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	_ = mgr.AllowAndBuild("prune-keep-ext")

	builtTag := ext.ExtensionImageTag("prune-keep-ext", "0.5.0", "")
	repo, tag, _ := strings.Cut(builtTag, ":")

	runner := e2e3APINewFakeEngineRunner()
	runner.seedImage(repo, tag)
	runner.seedImage("unused", "1")

	eng := engine.New(engine.Config{
		Dir:        t.TempDir(),
		Runner:     runner,
		KeepImages: mgr.KeepImages,
		Version:    "0.0.0-e2e",
	})
	if err := eng.Open(context.Background()); err != nil {
		t.Fatalf("engine.Open: %v", err)
	}
	defer func() { _ = eng.Close(time.Second) }()

	srv := httptest.NewServer(eng.Handler())
	defer srv.Close()

	q := url.QueryEscape(`{"dangling":["false"]}`)
	st, _, body := httpDoJSON(t, srv.Client(), http.MethodPost, srv.URL+"/v1.52/images/prune?filters="+q, "", "", nil, nil)
	if st != http.StatusOK {
		t.Fatalf("POST /v1.52/images/prune = %d: %s", st, string(body))
	}
	if !runner.hasRef(builtTag) {
		t.Fatalf("active extension image %q was pruned despite KeepImages protection!", builtTag)
	}
	if runner.hasRef("unused:1") {
		t.Fatalf("unused:1 should have been pruned; body=%s", string(body))
	}
}

func TestF20_T1_IdleTimeoutStopsIdleExtension(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}

	binPath := buildFixtureSDKBinary(t, e2e3APIHealthySource)
	binData, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read fixture binary: %v", err)
	}
	extDir := e2e3APIWriteBinaryDirWithIdle(t, t.TempDir(), "idle-stop-ext", binData, 1, "")
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("idle-stop-ext"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	e2e3APIWaitForExtState(t, mgr, "idle-stop-ext", ext.StateRunning, 5*time.Second)

	// With idle_timeout_sec = 1 and no open view subscriptions, extension should auto-stop.
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		info, ok := e2e3APIFindExtInfo(t, mgr, "idle-stop-ext")
		if ok && info.State != ext.StateRunning && info.State != ext.StateStarting {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	info, _ := e2e3APIFindExtInfo(t, mgr, "idle-stop-ext")
	t.Fatalf("extension with idle_timeout_sec=1 stayed active (%+v)", info)
}

func TestF20_T1_ViewSubscribeWakesIdleStoppedExtension(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}

	binPath := buildFixtureSDKBinary(t, e2e3APIHealthySource)
	binData, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read fixture binary: %v", err)
	}
	extDir := e2e3APIWriteBinaryDirWithIdle(t, t.TempDir(), "wake-idle-ext", binData, 1, "")
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("wake-idle-ext"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	e2e3APIWaitForExtState(t, mgr, "wake-idle-ext", ext.StateRunning, 5*time.Second)

	// Wait for idle auto-stop.
	deadline := time.Now().Add(6 * time.Second)
	stopped := false
	for time.Now().Before(deadline) {
		info, ok := e2e3APIFindExtInfo(t, mgr, "wake-idle-ext")
		if ok && info.State != ext.StateRunning && info.State != ext.StateStarting {
			stopped = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !stopped {
		t.Fatal("extension never idle-stopped before wake test")
	}

	// SubscribeView must transparently wake the idle-stopped extension and deliver the initial tree.
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	updates, err := mgr.SubscribeView(ctx, "wake-idle-ext", "main", "en")
	if err != nil {
		t.Fatalf("SubscribeView(wake-idle-ext): %v", err)
	}

	select {
	case u, ok := <-updates:
		if !ok || u.Tree == nil {
			t.Fatalf("expected non-nil initial tree upon waking idle extension, ok=%v update=%+v", ok, u)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for SubscribeView to wake idle-stopped extension")
	}
	e2e3APIWaitForExtState(t, mgr, "wake-idle-ext", ext.StateRunning, 3*time.Second)
}

func TestF20_T1_IdleTimeoutZeroStaysResident(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	// Default idle_timeout_sec = 0.
	e2e3APIInstallAndStartBinaryExt(t, mgr, "resident-ext", e2e3APIHealthySource, "")

	time.Sleep(1500 * time.Millisecond)
	info, ok := e2e3APIFindExtInfo(t, mgr, "resident-ext")
	if !ok || info.State != ext.StateRunning {
		t.Fatalf("extension with idle_timeout_sec=0 did not remain running: %+v", info)
	}
}

func TestF20_T2_UninstallRemovesImageFromKeepImages(t *testing.T) {
	mgr, _ := newTestManager(t)

	localDir := createLocalExtensionDir(t, t.TempDir(), "uninstall-keep-ext", minimalManifestTOML("uninstall-keep-ext", "1.0.0"), nil)
	if _, err := mgr.InstallLocal(localDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	_ = mgr.AllowAndBuild("uninstall-keep-ext")

	wantTag := ext.ExtensionImageTag("uninstall-keep-ext", "1.0.0", "")
	if !slices.Contains(mgr.KeepImages(), wantTag) {
		t.Fatalf("expected %q in KeepImages before uninstall, got %v", wantTag, mgr.KeepImages())
	}

	if err := mgr.Uninstall("uninstall-keep-ext", false); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if slices.Contains(mgr.KeepImages(), wantTag) {
		t.Fatalf("KeepImages() still contains %q after Uninstall: %v", wantTag, mgr.KeepImages())
	}
}

func TestF20_T2_DisabledAndInvalidExcludedFromKeepImages(t *testing.T) {
	mgr, h := newTestManager(t)
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}

	disabledDir := createLocalExtensionDir(t, t.TempDir(), "disabled-keep-ext", minimalManifestTOML("disabled-keep-ext", "1.0.0"), nil)
	if _, err := mgr.InstallLocal(disabledDir); err != nil {
		t.Fatalf("InstallLocal(disabled-keep-ext): %v", err)
	}
	_ = mgr.AllowAndBuild("disabled-keep-ext")
	disabledTag := ext.ExtensionImageTag("disabled-keep-ext", "1.0.0", "")
	if !slices.Contains(mgr.KeepImages(), disabledTag) {
		t.Fatalf("expected %q in KeepImages before Disable, got %v", disabledTag, mgr.KeepImages())
	}
	if err := mgr.Disable("disabled-keep-ext"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if slices.Contains(mgr.KeepImages(), disabledTag) {
		t.Fatalf("disabled extension image %q should be excluded from KeepImages(): %v", disabledTag, mgr.KeepImages())
	}
	_ = mgr.Close(3 * time.Second)

	// Seed an invalid on-disk extension with a source.json image tag and verify a fresh Manager Start excludes it from KeepImages().
	invalidID := "invalid-keep-ext"
	invalidDir := ext.ExtensionDir(h.DataDir, invalidID)
	writeExtManifest(t, invalidDir, "not = valid = toml [")
	if err := ext.SaveSource(ext.SourcePath(h.DataDir, invalidID), ext.SourceSpec{
		Kind:     ext.SourceImage,
		ImageRef: "ghcr.io/acme/invalid-keep:1.0.0",
	}); err != nil {
		t.Fatalf("SaveSource: %v", err)
	}
	mgr2, _ := newTestManager(t, WithDataDir(h.DataDir))
	if err := mgr2.Start(context.Background()); err != nil {
		t.Fatalf("mgr2.Start: %v", err)
	}
	if slices.Contains(mgr2.KeepImages(), disabledTag) {
		t.Fatalf("disabled extension image %q should remain excluded from KeepImages(): %v", disabledTag, mgr2.KeepImages())
	}
	if slices.Contains(mgr2.KeepImages(), "ghcr.io/acme/invalid-keep:1.0.0") {
		t.Fatalf("invalid extension image should be excluded from KeepImages(): %v", mgr2.KeepImages())
	}
}

func TestF20_T2_ActiveViewSSEPreventsIdleStop(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}

	binPath := buildFixtureSDKBinary(t, e2e3APIHealthySource)
	binData, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read fixture binary: %v", err)
	}
	extDir := e2e3APIWriteBinaryDirWithIdle(t, t.TempDir(), "active-sse-ext", binData, 1, "")
	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	if err := mgr.AllowAndBuild("active-sse-ext"); err != nil {
		t.Fatalf("AllowAndBuild: %v", err)
	}
	e2e3APIWaitForExtState(t, mgr, "active-sse-ext", ext.StateRunning, 5*time.Second)

	subCtx, subCancel := context.WithCancel(context.Background())
	updates, err := mgr.SubscribeView(subCtx, "active-sse-ext", "main", "en")
	if err != nil {
		subCancel()
		t.Fatalf("SubscribeView(active-sse-ext): %v", err)
	}
	select {
	case _, ok := <-updates:
		if !ok {
			subCancel()
			t.Fatal("SubscribeView closed before initial SSE frame")
		}
	case <-time.After(3 * time.Second):
		subCancel()
		t.Fatal("timed out waiting for initial SSE frame")
	}

	// Remain subscribed past idle_timeout_sec (1s) -> must stay running.
	time.Sleep(2200 * time.Millisecond)
	info, ok := e2e3APIFindExtInfo(t, mgr, "active-sse-ext")
	if !ok || info.State != ext.StateRunning {
		subCancel()
		t.Fatalf("extension stopped despite active SSE subscription: %+v", info)
	}

	// Closing subscription must allow subsequent idle stop.
	subCancel()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		info, ok := e2e3APIFindExtInfo(t, mgr, "active-sse-ext")
		if ok && info.State != ext.StateRunning && info.State != ext.StateStarting {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	info, _ = e2e3APIFindExtInfo(t, mgr, "active-sse-ext")
	t.Fatalf("extension did not idle-stop after SSE stream closed (%+v)", info)
}

func TestF20_T2_ThreeConsecutiveCrashesEnterFailedState(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, _ := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	binPath := buildFixtureSDKBinary(t, e2e3APICrashableSource)
	binData, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read fixture binary: %v", err)
	}
	extDir := e2e3APIWriteBinaryDirWithIdle(t, t.TempDir(), "crash-3x-ext", binData, 0, "")
	if err := os.MkdirAll(ext.FilesDir(dataDir, "crash-3x-ext"), 0o700); err != nil {
		t.Fatalf("mkdir files: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ext.FilesDir(dataDir, "crash-3x-ext"), "crash.flag"), []byte("1"), 0o600); err != nil {
		t.Fatalf("write crash.flag: %v", err)
	}

	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	_ = mgr.AllowAndBuild("crash-3x-ext")

	// Backoff across 3 consecutive crashes (e.g. 1s/2s/4s or faster in test) settles at StateFailed.
	e2e3APIWaitForExtState(t, mgr, "crash-3x-ext", ext.StateFailed, 20*time.Second)
}

func TestF20_T2_EnableAndRebuildResetCrashCounter(t *testing.T) {
	dataDir := newShortTempDataDir(t)
	mgr, h := newTestManager(t, WithDataDir(dataDir), WithDeveloperMode(true))

	binPath := buildFixtureSDKBinary(t, e2e3APICrashableSource)
	binData, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read fixture binary: %v", err)
	}
	extDir := e2e3APIWriteBinaryDirWithIdle(t, t.TempDir(), "reset-crash-ext", binData, 0, "")
	flagPath := filepath.Join(ext.FilesDir(dataDir, "reset-crash-ext"), "crash.flag")
	if err := os.MkdirAll(filepath.Dir(flagPath), 0o700); err != nil {
		t.Fatalf("mkdir files: %v", err)
	}
	if err := os.WriteFile(flagPath, []byte("1"), 0o600); err != nil {
		t.Fatalf("write crash.flag: %v", err)
	}

	if _, err := mgr.InstallLocal(extDir); err != nil {
		t.Fatalf("InstallLocal: %v", err)
	}
	_ = mgr.AllowAndBuild("reset-crash-ext")
	e2e3APIWaitForExtState(t, mgr, "reset-crash-ext", ext.StateFailed, 20*time.Second)

	// Fix crash condition and call Enable -> must reset crash counter and enter StateRunning.
	if err := os.Remove(flagPath); err != nil {
		t.Fatalf("remove crash.flag: %v", err)
	}
	if err := mgr.Enable("reset-crash-ext"); err != nil {
		t.Fatalf("Enable after StateFailed: %v", err)
	}
	e2e3APIWaitForExtState(t, mgr, "reset-crash-ext", ext.StateRunning, 5*time.Second)

	// Also verify Rebuild resets build/crash state on a sandbox extension.
	sbxDir := createLocalExtensionDir(t, t.TempDir(), "rebuild-reset-ext", minimalManifestTOML("rebuild-reset-ext", "0.1.0"), nil)
	if _, err := mgr.InstallLocal(sbxDir); err != nil {
		t.Fatalf("InstallLocal(rebuild-reset-ext): %v", err)
	}
	_ = mgr.AllowAndBuild("rebuild-reset-ext")
	beforeCalls := len(h.ImageBuilder.SnapshotCalls())
	_ = mgr.Rebuild("rebuild-reset-ext")
	if len(h.ImageBuilder.SnapshotCalls()) <= beforeCalls {
		t.Fatalf("expected Rebuild to invoke ImageBuilder again (before=%d, after=%d)", beforeCalls, len(h.ImageBuilder.SnapshotCalls()))
	}
}
