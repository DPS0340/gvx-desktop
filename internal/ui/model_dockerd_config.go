package ui

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/ui"

	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// dockerEngineSpec is the Docker Engine's sandbox: the spec it runs with
// while it runs, its saved one otherwise.
func (m *Model) dockerEngineSpec() (sandbox.Spec, bool) {
	r, ok := m.RecipeSandbox(ggui.Peek(m.Rows), recipes.Docker)
	if !ok {
		return recipes.DockerEngine(), false
	}
	if spec, ok := m.activeSpec(r.Spec.ID); ok {
		return spec, true
	}
	return r.Spec, true
}

// OpenDockerConfig shows the engine's daemon.json in the editor: the file
// on this computer, or what the engine starts with when there is none.
func (m *Model) OpenDockerConfig() {
	spec, _ := m.dockerEngineSpec()
	text, err := recipes.ReadDockerConfig()
	if err != nil {
		m.toastError(tr("Could not read %s", tildePath(recipes.DockerConfigFile())), err)
		return
	}
	if strings.TrimSpace(text) == "" {
		if out, err := recipes.NormalizeDockerConfig(spec, nil); err == nil {
			text = string(out)
		}
	}
	m.DockerConfigText.Set(text)
	m.DockerConfigErr.Set("")
	m.DockerConfigOpen.Set(true)
}

// SaveDockerConfig writes the edited daemon.json and has a running engine
// read it again (SIGHUP) when dockerd can apply every change so; it offers
// a restart otherwise.
func (m *Model) SaveDockerConfig() {
	text := ggui.Peek(m.DockerConfigText)
	if _, err := recipes.ParseDockerConfig([]byte(text)); err != nil {
		m.DockerConfigErr.Set(tr("Not valid JSON: %s", strings.TrimPrefix(err.Error(), "daemon.json: ")))
		return
	}
	spec, _ := m.dockerEngineSpec()
	before, _ := recipes.ReadDockerConfig()
	var written []byte
	m.async(func(context.Context) error {
		var err error
		written, err = recipes.WriteDockerConfig(spec, []byte(text))
		return err
	}, func(err error) {
		if err != nil {
			m.toastError(tr("Could not save %s", tildePath(recipes.DockerConfigFile())), err)
			return
		}
		m.DockerConfigOpen.Set(false)
		m.applyDockerConfig([]byte(before), written)
	})
}

// applyDockerConfig tells the running engine about a saved daemon.json.
func (m *Model) applyDockerConfig(before, after []byte) {
	r, ok := m.RecipeSandbox(ggui.Peek(m.Rows), recipes.Docker)
	if !ok || !r.State.Status.Active() {
		m.toastOK(tr("daemon.json saved"), tr("The Docker Engine reads it as it starts."))
		return
	}
	id := r.Spec.ID
	restart := func(why string) {
		m.Toasts.Push(ui.Toast(tr("daemon.json saved"), why).Info().Duration(10*time.Second).
			Action(tr("Restart"), func() { m.RestartSandbox(id) }))
	}
	spec, _ := m.activeSpec(id)
	if !recipes.HasDockerConfig(spec) {
		restart(tr("The running Docker Engine started before it read daemon.json from this computer. Restart it to use the file."))
		return
	}
	changed, needRestart, err := recipes.DockerConfigChanges(before, after)
	switch {
	case err != nil:
		m.toastError(tr("Could not apply daemon.json"), err)
		return
	case len(changed) == 0:
		m.toastOK(tr("daemon.json saved"), tr("Nothing changed."))
		return
	case len(needRestart) > 0:
		restart(tr("dockerd applies %s only as it starts. Restart the Docker Engine to apply it.", strings.Join(needRestart, ", ")))
		return
	}
	if r.State.Paused {
		m.Toasts.Push(ui.Toast(tr("daemon.json saved"), tr("The Docker Engine is paused. Resume it and save again to apply the file, or restart it.")).Info())
		return
	}
	ctl, ok := m.sandboxControl(id)
	if !ok {
		restart(tr("dockerd reads it again on a restart: this rungv cannot signal the running engine."))
		return
	}
	m.async(func(ctx context.Context) error {
		return reloadDockerd(ctx, ctl, m.sup.Logs(id))
	}, func(err error) {
		if err != nil {
			m.toastError(tr("dockerd did not apply daemon.json"), err)
			return
		}
		m.toastOK(tr("daemon.json applied"), tr("dockerd read it again without a restart: %s.", strings.Join(changed, ", ")))
	})
}

// dockerdReloadWait is how long reloadDockerd waits for dockerd to say how
// the reload went.
var dockerdReloadWait = 8 * time.Second

// reloadDockerd sends SIGHUP to dockerd, the engine's first process, and
// waits for what it logs of the reload.
func reloadDockerd(ctx context.Context, ctl sandbox.Control, logs *sandbox.LogBuffer) error {
	seq := logs.Seq()
	if err := ctl.Kill(ctx, rungv.KillOptions{Signal: "SIGHUP"}); err != nil {
		return err
	}
	deadline := time.Now().Add(dockerdReloadWait)
	for time.Now().Before(deadline) {
		for _, l := range logs.Snapshot() {
			if l.Seq <= seq || l.Stream == sandbox.System {
				continue
			}
			if ok, err := dockerdReloadResult(l.Text); ok || err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return errors.New(tr("dockerd said nothing of a reload; see the Docker Engine's logs"))
}

// dockerdReloadResult reads a line of dockerd's after a SIGHUP: ok when it
// applied the file, an error when it would not.
func dockerdReloadResult(line string) (ok bool, err error) {
	switch {
	case strings.Contains(line, "Reloaded configuration"):
		return true, nil
	case strings.Contains(line, "level=error") || strings.Contains(line, "level=fatal"):
		// time=… level=error msg="Error reloading configuration" error="unable to …"
		if v, ok := logfmtValue(line, "error"); ok {
			return false, errors.New(v)
		}
		if v, ok := logfmtValue(line, "msg"); ok {
			return false, errors.New(v)
		}
		return false, errors.New(line)
	}
	return false, nil
}

// logfmtValue is the value of key in a logfmt line, as dockerd writes its
// log: key=word or key="quoted \"text\"".
func logfmtValue(line, key string) (string, bool) {
	for i := 0; i < len(line); {
		j := strings.Index(line[i:], key+"=")
		if j < 0 {
			return "", false
		}
		j += i
		if j > 0 && line[j-1] != ' ' {
			i = j + len(key) + 1
			continue
		}
		rest := line[j+len(key)+1:]
		if !strings.HasPrefix(rest, `"`) {
			v, _, _ := strings.Cut(rest, " ")
			return v, true
		}
		if v, err := strconv.QuotedPrefix(rest); err == nil {
			if u, err := strconv.Unquote(v); err == nil {
				return u, true
			}
		}
		return strings.Trim(rest, `"`), true
	}
	return "", false
}
