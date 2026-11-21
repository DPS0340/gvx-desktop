package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/ui"

	"github.com/ironpark/gvx-desktop/internal/dockerproxy"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// initDockerProxy constructs the Model's Docker API proxy and keeps an atomic
// snapshot of the Docker Engine spec for request-time bind-mount checks. While
// the engine sandbox is active, the snapshot reflects the spec it started with;
// otherwise it falls back to the saved spec. Call it from NewModel after m.sup
// and m.Rows are initialized.
func (m *Model) initDockerProxy() {
	ggui.Watch(m.Rows, func(rows []Row) {
		m.syncEngineSpec(rows)
	})
	m.dockerProxy = dockerproxy.New(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", recipes.DockerBackendSocket())
	})
	m.dockerProxy.Rewrite = m.rewriteContainerCreate
	m.dockerProxy.Check = m.checkContainerCreate
	m.dockerProxy.Changed = func() {
		m.post(func() {
			m.LoadDocker()
			m.LoadDockerImages()
		})
	}
	ggui.OnCleanup(m.CloseDockerProxy)
}

func (m *Model) syncEngineSpec(rows []Row) {
	for _, r := range rows {
		if r.Spec.Recipe != recipes.Docker {
			continue
		}
		if m.sup != nil {
			if active, ok := m.sup.ActiveSpec(r.Spec.ID); ok {
				m.engineSpec.Store(&active)
				return
			}
		}
		c := r.Spec.Clone()
		m.engineSpec.Store(&c)
		return
	}
	m.engineSpec.Store(nil)
}

// startDockerProxy binds the Docker API proxy listener for the configured
// endpoint. It is skipped when Offline is set.
func (m *Model) startDockerProxy() {
	if m.Offline || m.dockerProxy == nil || m.dockerProxyStarted {
		return
	}
	m.dockerProxyStarted = true
	m.bindDockerProxy(m.DockerAPI())
}

// bindDockerProxy binds or moves the Docker API proxy listener to api and
// records any listener error in DockerProxyErr and a warning toast.
func (m *Model) bindDockerProxy(api recipes.DockerAPI) {
	if m.Offline || m.dockerProxy == nil {
		return
	}
	network, addr := api.ListenAddr()
	if err := m.dockerProxy.Listen(network, addr); err != nil {
		m.DockerProxyErr.Set(err.Error())
		m.Toasts.Push(ui.Toast(tr("Could not listen for Docker"), err.Error()).Warning().Duration(8 * time.Second))
		return
	}
	m.DockerProxyErr.Set("")
}

// CloseDockerProxy stops the Docker API proxy listener and closes active
// connections.
func (m *Model) CloseDockerProxy() {
	if m.dockerProxy != nil {
		_ = m.dockerProxy.Close()
	}
}

func (m *Model) checkContainerCreate(_ *http.Request, body []byte) error {
	sp := m.engineSpec.Load()
	if sp == nil {
		return nil
	}
	spec := *sp
	if m.sup != nil {
		if active, ok := m.sup.ActiveSpec(spec.ID); ok {
			spec = active
		}
	}
	return checkCreateBinds(body, spec)
}

// checkCreateBinds rejects a POST /containers/create body that bind-mounts a
// host path outside the folders shared with engine. Unparsable bodies pass
// through untouched so dockerd returns its own error.
func checkCreateBinds(body []byte, engine sandbox.Spec) error {
	var req struct {
		HostConfig *struct {
			Binds  []string `json:"Binds"`
			Mounts []struct {
				Type   string `json:"Type"`
				Source string `json:"Source"`
			} `json:"Mounts"`
		} `json:"HostConfig"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.HostConfig == nil {
		return nil
	}
	for _, b := range req.HostConfig.Binds {
		src, _, ok := strings.Cut(b, ":")
		if !ok || !strings.HasPrefix(src, "/") {
			continue
		}
		if recipes.UnsharedBind(src, engine) {
			return unsharedBindError(src)
		}
	}
	for _, m := range req.HostConfig.Mounts {
		if !strings.EqualFold(m.Type, "bind") || !strings.HasPrefix(m.Source, "/") {
			continue
		}
		if recipes.UnsharedBind(m.Source, engine) {
			return unsharedBindError(m.Source)
		}
	}
	return nil
}

func unsharedBindError(path string) error {
	return fmt.Errorf("%s is not in a folder shared with the Docker Engine. Share it in GVX Desktop → Settings → Docker, restart the engine, and run this again.", path)
}

// rewriteContainerCreate changes a container's creation on its way to the
// engine: on Windows the folders docker names the Windows way are named as
// the engine has them.
func (m *Model) rewriteContainerCreate(_ *http.Request, body []byte) []byte {
	if runtime.GOOS != "windows" {
		return nil
	}
	return windowsBinds(body)
}

// windowsSource splits a bind at the colon after its source, C:\\src:/dst
// into C:\\src and :/dst, the drive's colon aside; whether the source is a
// Windows folder is recipes.WindowsGuestPath's to say.
func windowsSource(bind string) (source, rest string) {
	if len(bind) > 2 {
		if i := strings.IndexByte(bind[2:], ':'); i >= 0 {
			return bind[:2+i], bind[2+i:]
		}
	}
	return bind, ""
}

// windowsBinds returns a POST /containers/create body with the bind mount
// sources written the Windows way, C:\Users\me\src, as the engine has
// them, /c/Users/me/src; nil when there is none, or the body is not what
// it should be, which is dockerd's to say.
func windowsBinds(body []byte) []byte {
	var req map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // a memory limit must come out as it went in
	if err := dec.Decode(&req); err != nil {
		return nil
	}
	host, _ := req["HostConfig"].(map[string]any)
	if host == nil {
		return nil
	}
	changed := false
	if binds, _ := host["Binds"].([]any); binds != nil {
		for i, b := range binds {
			s, _ := b.(string)
			src, rest := windowsSource(s)
			if g, err := recipes.WindowsGuestPath(src); err == nil {
				binds[i], changed = g+rest, true
			}
		}
	}
	if mounts, _ := host["Mounts"].([]any); mounts != nil {
		for _, mt := range mounts {
			mount, _ := mt.(map[string]any)
			if typ, _ := mount["Type"].(string); !strings.EqualFold(typ, "bind") {
				continue
			}
			src, _ := mount["Source"].(string)
			if g, err := recipes.WindowsGuestPath(src); err == nil {
				mount["Source"], changed = g, true
			}
		}
	}
	if !changed {
		return nil
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(req); err != nil {
		return nil
	}
	return bytes.TrimRight(b.Bytes(), "\n")
}
