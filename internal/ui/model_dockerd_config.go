package ui

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/ui"

	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// dockerEngineSpec is the Docker Engine's sandbox: the spec it runs with
// while it runs, its saved one otherwise.
func (m *Model) dockerEngineSpec() sandbox.Spec {
	r, ok := m.RecipeSandbox(ggui.Peek(m.Rows), recipes.Docker)
	if !ok {
		return recipes.DockerEngine()
	}
	if spec, ok := m.activeSpec(r.Spec.ID); ok {
		return spec
	}
	return r.Spec
}

// OpenDockerConfig shows the engine's daemon.json in the editor: the file
// on this computer, or what the engine starts with when there is none.
func (m *Model) OpenDockerConfig() {
	text, err := recipes.ReadDockerConfig()
	if err != nil {
		m.toastError(tr("Could not read %s", tildePath(recipes.DockerConfigFile())), err)
		return
	}
	if len(bytes.TrimSpace(text)) == 0 {
		if out, err := recipes.NormalizeDockerConfig(m.dockerEngineSpec(), nil); err == nil {
			text = out
		}
	}
	m.DockerConfigText.Set(string(text))
	m.DockerConfigErr.Set("")
	m.DockerConfigOpen.Set(true)
}

// SaveDockerConfig writes the edited daemon.json and has a running engine
// read it again (SIGHUP) when dockerd can apply every change so; it offers
// a restart otherwise.
func (m *Model) SaveDockerConfig() {
	text := []byte(ggui.Peek(m.DockerConfigText))
	if _, err := recipes.ParseDockerConfig(text); err != nil {
		m.DockerConfigErr.Set(tr("Not valid JSON: %s", err.Error()))
		return
	}
	spec := m.dockerEngineSpec()
	var before, written []byte
	m.async(func(context.Context) error {
		before, _ = recipes.ReadDockerConfig()
		var err error
		written, err = recipes.WriteDockerConfig(spec, text)
		return err
	}, func(err error) {
		if err != nil {
			m.toastError(tr("Could not save %s", tildePath(recipes.DockerConfigFile())), err)
			return
		}
		m.DockerConfigOpen.Set(false)
		m.applyDockerConfig(before, written)
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
		return recipes.ReloadDockerd(ctx, ctl, m.sup.Logs(id))
	}, func(err error) {
		if errors.Is(err, recipes.ErrNoReloadWord) {
			err = errors.New(tr("dockerd said nothing of a reload; see the Docker Engine's logs"))
		}
		if err != nil {
			m.toastError(tr("dockerd did not apply daemon.json"), err)
			return
		}
		m.toastOK(tr("daemon.json applied"), tr("dockerd read it again without a restart: %s.", strings.Join(changed, ", ")))
	})
}
