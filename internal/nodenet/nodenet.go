// Package nodenet joins the Kubernetes recipe's nodes, one sandbox each,
// on an overlay of the app's own (internal/overlay), as the sandbox engine
// joins containers on a user network: a node with an address on
// recipes.NodeSubnet runs netinit in front of its command, which makes its
// link to the hub. See docs/design/k3s-clusters.md §2.3.
package nodenet

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"sync"

	"github.com/ironpark/gvx-desktop/internal/overlay"
	"github.com/ironpark/gvx-desktop/internal/overlay/guest"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// network is the ID of the nodes' one network on the fabric.
const network = "k3s-nodes"

// Network is the nodes' overlay. The zero value is not usable: set Dir and
// Client.
type Network struct {
	// Dir is where netinit's mount directory is written for the sandboxes to mount.
	Dir string
	// Client is the rungv the sandboxes run with.
	Client func() *rungv.Client
	// Logf, when set, logs the hub's troubles.
	Logf func(string, ...any)

	mu      sync.Mutex
	fab     *overlay.Fabric
	bin     string
	members map[string]overlay.Member // by sandbox ID
	closed  bool
}

// Launch is the supervisor's Launch: a node with an address on the nodes'
// network joins it, and runs netinit in front of its command; the release
// takes it off as the run ends. Other sandboxes run as they are.
func (n *Network) Launch(spec sandbox.Spec) (sandbox.Spec, func(), error) {
	addr, ok := recipes.NodeAddr(spec)
	if !ok {
		return spec, nil, nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.openLocked(); err != nil {
		return spec, nil, err
	}
	if !spec.SetEntrypoint {
		// netinit goes in front of the entrypoint, which a node leaves
		// to its image.
		spec = spec.Clone()
		spec.Entrypoint, spec.SetEntrypoint = recipes.K3sEntrypoint, true
	}
	prefix := netip.PrefixFrom(addr, recipes.NodeSubnet.Bits())
	cfg, dns, err := n.fab.Join(spec.ID, []netip.Prefix{prefix}, overlay.JoinOptions{MTU: n.mtu()})
	if err != nil {
		return spec, nil, err
	}
	run, err := overlay.WithNetinit(spec, cfg, n.bin, dns)
	if err != nil {
		n.fab.Leave(spec.ID)
		return spec, nil, err
	}
	name := spec.Hostname
	if name == "" {
		name = spec.Name
	}
	n.members[spec.ID] = overlay.Member{ID: spec.ID, Endpoints: map[string]overlay.Endpoint{
		network: {Addr: addr, Names: []string{name}},
	}}
	n.syncLocked()
	var once sync.Once
	return run, func() { once.Do(func() { n.leave(spec.ID) }) }, nil
}

// openLocked writes netinit and makes the fabric, once.
func (n *Network) openLocked() error {
	if n.closed {
		return errClosed
	}
	if n.bin == "" {
		bin, err := guest.Netinit()
		if err != nil {
			return err
		}
		if n.bin, err = guest.Install(n.Dir, bin); err != nil {
			return err
		}
	}
	if n.fab == nil {
		n.fab = overlay.New(n.Logf)
		n.members = map[string]overlay.Member{}
	}
	return nil
}

// mtu is the overlay's when rungv can raise the sandbox's own for it.
func (n *Network) mtu() int {
	if c := n.Client(); c != nil && c.HasRunFlag(context.Background(), "tune") {
		return overlay.LinkMTU
	}
	return overlay.BaseMTU
}

func (n *Network) leave(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.fab == nil {
		return
	}
	n.fab.Leave(id)
	delete(n.members, id)
	n.syncLocked()
}

func (n *Network) syncLocked() {
	list := make([]overlay.Member, 0, len(n.members))
	for _, m := range n.members {
		list = append(list, m)
	}
	n.fab.SetMembers(list)
}

// Close stops the hub, once the nodes stopped.
func (n *Network) Close() {
	n.mu.Lock()
	fab := n.fab
	n.fab, n.closed = nil, true
	n.mu.Unlock()
	if fab != nil {
		fab.Close()
	}
}

var errClosed = errors.New("the nodes' network is closed")

// ServerToken reads the token agents join the cluster with from its
// running server (recipes.NodeTokenFile). k3s writes it once the server is
// up.
func ServerToken(ctx context.Context, ctl sandbox.Control) (string, error) {
	rc, err := ctl.CopyFrom(ctx, recipes.NodeTokenFile, rungv.CopyOptions{FollowLink: true})
	if err != nil {
		return "", err
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err != nil {
			return "", fmt.Errorf("read %s: %w", recipes.NodeTokenFile, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, 4096))
		if err != nil {
			return "", err
		}
		token := strings.TrimSpace(string(b))
		if token == "" {
			return "", fmt.Errorf("%s is empty", recipes.NodeTokenFile)
		}
		return token, nil
	}
}
