package ui

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
)

// sortNetworks puts the builtin networks first and the others by name, as
// docker network ls does not but a table reads best.
func sortNetworks(list []dockerapi.Network) []dockerapi.Network {
	out := slices.Clone(list)
	slices.SortFunc(out, func(a, b dockerapi.Network) int {
		if a.Builtin() != b.Builtin() {
			if a.Builtin() {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// filterNetworks keeps the networks whose name, driver, subnet, Compose
// project or ID has q in it.
func filterNetworks(list []dockerapi.Network, q string) []dockerapi.Network {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return list
	}
	var out []dockerapi.Network
	for _, n := range list {
		sub, _ := n.Subnet()
		if strings.Contains(strings.ToLower(n.Name+" "+n.Driver+" "+sub+" "+n.Project()+" "+n.ID), q) {
			out = append(out, n)
		}
	}
	return out
}

// chosenNetwork is the network of id (its ID or name), or the first.
func chosenNetwork(list []dockerapi.Network, id string) dockerapi.Network {
	for _, n := range list {
		if n.ID == id || n.Name == id {
			return n
		}
	}
	if len(list) > 0 {
		return sortNetworks(list)[0]
	}
	return dockerapi.Network{}
}

// NetworkMember is a container on a network, with what the containers'
// list knows of it: it is there while the network lists one the list does
// not have yet.
type NetworkMember struct {
	ID      string
	Name    string
	Address string // without its prefix length
	State   string // running, exited, …; "" when the list has no such container
	Names   []string
}

// networkMembers are the containers on n, by name.
func networkMembers(n dockerapi.Network, ctrs []dockerapi.Container) []NetworkMember {
	byID := map[string]dockerapi.Container{}
	for _, c := range ctrs {
		byID[c.ID] = c
	}
	var out []NetworkMember
	for id, nm := range n.Containers {
		addr, _, _ := strings.Cut(nm.IPv4Address, "/")
		mem := NetworkMember{ID: id, Name: nm.Name, Address: addr}
		if c, ok := byID[id]; ok {
			mem.State = c.State
			if mem.Name == "" {
				mem.Name = c.Name()
			}
			if ep, ok := c.NetworkSettings.Networks[n.Name]; ok {
				mem.Names = ep.DNSNames
				if len(mem.Names) == 0 {
					mem.Names = ep.Aliases
				}
			}
		}
		out = append(out, mem)
	}
	slices.SortFunc(out, func(a, b NetworkMember) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// SelectNetwork chooses a network on the Networks page, by ID or name.
func (m *Model) SelectNetwork(id string) {
	m.SelectedNetwork.Set(id)
}

// ShowNetwork opens the Networks page on a network, by name or ID.
func (m *Model) ShowNetwork(id string) {
	m.NetworkQuery.Set("")
	m.SelectNetwork(id)
	m.Go(PageNetworks)
}

// LoadDockerNetworks reads the engine's networks. Asked for while a
// listing runs, it lists them again after it.
func (m *Model) LoadDockerNetworks() {
	if m.Offline {
		return
	}
	if ggui.Peek(m.ContainersAccess) != ContainersOpen {
		m.DockerNetworks.Set(Listing[dockerapi.Network]{})
		return
	}
	if ggui.Peek(m.Settings).SandboxEngine {
		if pe := ggui.Peek(m.DockerProxyErr); pe != "" {
			m.DockerNetworks.Set(Listing[dockerapi.Network]{Loaded: true, Err: pe})
			return
		}
		if seErr := ggui.Peek(m.SandboxEngineStatus).Err; seErr != "" {
			m.DockerNetworks.Set(Listing[dockerapi.Network]{Loaded: true, Err: seErr})
			return
		}
	}
	if m.dockerNetworksLoading {
		m.dockerNetworksAgain = true
		return
	}
	m.dockerNetworksLoading = true
	host := m.dockerBackendHost()
	var items []dockerapi.Network
	m.DockerNetworks.Update(func(l Listing[dockerapi.Network]) Listing[dockerapi.Network] { l.Loading = true; return l })
	m.async(func(ctx context.Context) error {
		c, err := dockerapi.New(host)
		if err != nil {
			return err
		}
		defer c.Close()
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		items, err = c.Networks(ctx)
		return err
	}, func(err error) {
		m.dockerNetworksLoading = false
		if m.dockerNetworksAgain {
			m.dockerNetworksAgain = false
			m.LoadDockerNetworks()
			return
		}
		if ggui.Peek(m.ContainersAccess) != ContainersOpen {
			return
		}
		l := Listing[dockerapi.Network]{Items: items, Loaded: true}
		if err != nil {
			l.Err = err.Error()
		}
		m.DockerNetworks.Set(l)
	})
}

// networkCall runs fn against the engine and lists the networks and
// containers again after it, saying how it went.
func (m *Model) networkCall(busyKey string, fn func(context.Context, *dockerapi.Client) error, failed string, done func()) {
	if busyKey != "" {
		if m.isBusy(busyKey) {
			return
		}
		m.setBusy(busyKey, true)
	}
	host := m.dockerBackendHost()
	m.async(func(ctx context.Context) error {
		c, err := dockerapi.New(host)
		if err != nil {
			return err
		}
		defer c.Close()
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return fn(ctx, c)
	}, func(err error) {
		if busyKey != "" {
			m.setBusy(busyKey, false)
		}
		if err != nil {
			m.toastError(failed, err)
		} else if done != nil {
			done()
		}
		m.LoadDockerNetworks()
		m.LoadDocker()
	})
}

// AskRemoveNetwork confirms and removes a network no container is on.
func (m *Model) AskRemoveNetwork(n dockerapi.Network) {
	busyKey := "net:" + n.ID
	if m.isBusy(busyKey) {
		return
	}
	m.Confirm(tr("Remove %s?", n.Name), tr("The network is removed from the engine. Compose makes its project's network again at the next up."), tr("Remove network"), func() {
		m.networkCall(busyKey, func(ctx context.Context, c *dockerapi.Client) error {
			return c.RemoveNetwork(ctx, n.ID)
		}, tr("Could not remove %s", n.Name), func() { m.toastOK(tr("Removed %s", n.Name), "") })
	})
}

// AskPruneNetworks removes the networks no container is on.
func (m *Model) AskPruneNetworks() {
	const busyKey = "net:prune"
	if m.isBusy(busyKey) {
		return
	}
	m.Confirm(tr("Remove unused networks?"), tr("Every network that no container is on is removed. The built-in ones stay."), tr("Remove unused"), func() {
		var deleted []string
		m.networkCall(busyKey, func(ctx context.Context, c *dockerapi.Client) error {
			var err error
			deleted, err = c.PruneNetworks(ctx)
			return err
		}, tr("Could not remove unused networks"), func() {
			if len(deleted) == 0 {
				m.toastOK(tr("No unused networks"), "")
				return
			}
			m.toastOK(trn(len(deleted), "Removed %d network", "Removed %d networks"), strings.Join(deleted, ", "))
		})
	})
}
