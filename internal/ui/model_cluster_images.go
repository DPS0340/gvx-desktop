package ui

// Images go from the Docker Engine into the cluster's nodes without a
// registry, as kind load docker-image does (docs/design/k3s-clusters.md §3).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/ui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/nodenet"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// loadNode is a node an image is loaded into.
type loadNode struct{ id, name string }

// loadTargets are the cluster's nodes that run with rungv's control, to
// load an image into, or why there are none.
func loadTargets(rows []Row) ([]loadNode, string) {
	nodes := clusterNodes(rows, nil)
	if len(nodes) == 0 {
		return nil, tr("Set up Kubernetes to load images into it.")
	}
	server := nodes[0].Row.State
	switch {
	case server.Status != sandbox.Running || server.Paused:
		return nil, tr("Start the cluster to load images into it.")
	case !server.Control:
		return nil, tr("Loading images into the cluster needs rungvx 0.20 or later.")
	}
	var out []loadNode
	for _, n := range nodes {
		if st := n.Row.State; st.Status == sandbox.Running && !st.Paused && st.Control {
			out = append(out, loadNode{n.Row.Spec.ID, n.Row.Spec.Name})
		}
	}
	return out, ""
}

// LoadBlocked says why img cannot be loaded into the cluster now, or "".
func LoadBlocked(img DockerImageRow, rows []Row) string {
	switch {
	case !img.Pullable():
		return tr("Tag the image first: pods name the image they run.")
	case img.ForeignArch():
		return tr("The cluster's nodes run images for this CPU (linux/%s) only.", goarch())
	}
	_, why := loadTargets(rows)
	return why
}

// LoadIntoCluster saves img from the Docker Engine and imports it into
// each running node of the cluster, where its pods run it without pulling
// it.
func (m *Model) LoadIntoCluster(img DockerImageRow) {
	rows := ggui.Peek(m.Rows)
	if why := LoadBlocked(img, rows); why != "" {
		m.toastError(tr("Could not load %s", img.Ref()), errors.New(why))
		return
	}
	nodes, _ := loadTargets(rows)
	ref := img.Ref()
	title := tr("Loading %s into the cluster", ref)
	toast := m.Toasts.Push(ui.Toast(title, tr("Saving it from the Docker Engine…")).Loading())
	step := func(text string) { m.post(func() { m.Toasts.Update(toast, ui.Toast(title, text).Loading()) }) }
	host := m.dockerBackendHost()
	m.async(func(ctx context.Context) error {
		archive, err := saveDockerImage(ctx, host, img.Target())
		if err != nil {
			return err
		}
		defer os.Remove(archive)
		var errs []error
		for _, n := range nodes {
			step(tr("Importing it into %s…", n.name))
			if err := m.importInto(ctx, n.id, archive); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", n.name, err))
			}
		}
		return errors.Join(errs...)
	}, func(err error) {
		if err != nil {
			m.Toasts.Update(toast, ui.Toast(tr("Could not load %s", ref), err.Error()).Error())
			return
		}
		m.Toasts.Update(toast, ui.Toast(tr("Loaded %s", ref),
			trn(len(nodes), "Into the cluster's %d node. Pods run it as %s without pulling it, unless their pull policy is Always, as a :latest tag's is by default.",
				"Into the cluster's %d nodes. Pods run it as %s without pulling it, unless their pull policy is Always, as a :latest tag's is by default.", ref)).Success())
	})
}

// saveDockerImage writes ref, as docker save does, to a temporary file and
// returns its path.
func saveDockerImage(ctx context.Context, host, ref string) (string, error) {
	c, err := dockerapi.New(host)
	if err != nil {
		return "", err
	}
	defer c.Close()
	rc, err := c.SaveImage(ctx, ref)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	f, err := os.CreateTemp("", "gvx-load-*.tar")
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, rc)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// importInto imports the archive at path into the node id runs.
func (m *Model) importInto(ctx context.Context, id, path string) error {
	ctl, ok := m.sandboxControl(id)
	if !ok {
		return errors.New(tr("it is not running"))
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return nodenet.ImportImage(ctx, ctl, f)
}
