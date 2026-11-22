package ui

import (
	"slices"
	"testing"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/store"
)

func testNetworks() []dockerapi.Network {
	shop := dockerapi.Network{Name: "shop_default", ID: "bb11", Driver: "bridge", Labels: map[string]string{"com.docker.compose.project": "shop"},
		Containers: map[string]dockerapi.NetworkMember{
			"c2":   {Name: "shop-redis-1", IPv4Address: "10.89.1.3/24"},
			"c1":   {Name: "shop-web-1", IPv4Address: "10.89.1.2/24"},
			"gone": {Name: "", IPv4Address: "10.89.1.9/24"},
		}}
	shop.IPAM.Config = append(shop.IPAM.Config, struct {
		Subnet  string `json:"Subnet"`
		Gateway string `json:"Gateway"`
	}{"10.89.1.0/24", "10.89.1.1"})
	return []dockerapi.Network{
		{Name: "scratch", ID: "ee44", Driver: "bridge"},
		shop,
		{Name: "none", ID: "dd33", Driver: "null"},
		{Name: "bridge", ID: "aa11", Driver: "bridge"},
	}
}

func TestSortFilterAndChooseNetworks(t *testing.T) {
	nets := testNetworks()
	var names []string
	for _, n := range sortNetworks(nets) {
		names = append(names, n.Name)
	}
	if !slices.Equal(names, []string{"bridge", "none", "scratch", "shop_default"}) {
		t.Errorf("sorted %v", names)
	}
	for q, want := range map[string]int{"": 4, "shop": 1, "10.89.1": 1, "NULL": 1, "bb1": 1, "nothing": 0} {
		if got := len(filterNetworks(nets, q)); got != want {
			t.Errorf("filter %q: %d, want %d", q, got, want)
		}
	}
	if chosenNetwork(nets, "shop_default").ID != "bb11" || chosenNetwork(nets, "bb11").Name != "shop_default" {
		t.Error("chosen by name or ID")
	}
	if got := chosenNetwork(nets, "gone").Name; got != "bridge" {
		t.Errorf("chosen without a match = %s, want the first", got)
	}
}

func TestNetworkMembers(t *testing.T) {
	web := dockerapi.Container{ID: "c1", Names: []string{"/shop-web-1"}, State: "running"}
	web.NetworkSettings.Networks = map[string]dockerapi.Endpoint{"shop_default": {DNSNames: []string{"shop-web-1", "web", "c1"}}}
	redis := dockerapi.Container{ID: "c2", Names: []string{"/shop-redis-1"}, State: "exited"}
	redis.NetworkSettings.Networks = map[string]dockerapi.Endpoint{"shop_default": {Aliases: []string{"redis"}}}
	got := networkMembers(testNetworks()[1], []dockerapi.Container{web, redis})
	if len(got) != 3 {
		t.Fatalf("members %+v", got)
	}
	// By name; the one the containers' list does not have yet comes first,
	// with no name and no state.
	if got[0].ID != "gone" || got[0].State != "" || got[0].Address != "10.89.1.9" {
		t.Errorf("unknown member %+v", got[0])
	}
	if got[1].Name != "shop-redis-1" || got[1].State != "exited" || !slices.Equal(got[1].Names, []string{"redis"}) {
		t.Errorf("redis %+v", got[1])
	}
	if got[2].Address != "10.89.1.2" || got[2].State != "running" || !slices.Equal(got[2].Names, []string{"shop-web-1", "web", "c1"}) {
		t.Errorf("web %+v", got[2])
	}
}

func TestNetworkRemovable(t *testing.T) {
	nets := testNetworks()
	if networkRemovable(nets[0]) != "" {
		t.Error("an unused network of one's own can be removed")
	}
	if networkRemovable(nets[1]) == "" || networkRemovable(nets[3]) == "" {
		t.Error("a used or built-in network cannot")
	}
}

// The Networks page opens only while the engine does, as Containers does,
// and shows the chosen network with its containers.
func TestNetworksPage(t *testing.T) {
	dir := t.TempDir()
	set := store.DefaultSettings()
	set.Language = LangEnglish
	set.SandboxEngine = true // the engine is open without a sandbox
	st := store.Store{Dir: dir}
	st.SaveSettings(set)
	t.Cleanup(func() { SetLanguage(LangEnglish) })
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return ggui.Provide(ggui.ReducedMotionKey, true, Build(m)) }, ggui.Sz(1200, 900))
	p.Setup(func() {
		m = NewModel(st, dir)
		m.Offline = true
		m.Attach(p)
	})
	t.Cleanup(p.Close)
	p.Frame()

	web := dockerapi.Container{ID: "c1", Names: []string{"/shop-web-1"}, State: "running"}
	web.NetworkSettings.Networks = map[string]dockerapi.Endpoint{"shop_default": {IPAddress: "10.89.1.2", DNSNames: []string{"shop-web-1", "web"}}}
	m.DockerContainers.Set(Listing[dockerapi.Container]{Loaded: true, Items: []dockerapi.Container{web}})
	m.DockerNetworks.Set(Listing[dockerapi.Network]{Loaded: true, Items: testNetworks()})
	m.ShowNetwork("shop_default")
	for range 3 {
		p.Frame()
	}
	if ggui.Peek(m.Page) != PageNetworks {
		t.Fatalf("page %s", ggui.Peek(m.Page))
	}
	for _, want := range []string{"Networks", "shop_default", "10.89.1.0/24", "10.89.1.2", "also web",
		"Containers on it reach each other by name: their container name, Compose service and aliases."} {
		if !semShows(p, want) {
			t.Errorf("no %q on the page:\n%s", want, p.Semantics())
		}
	}

	// The container's Connect tab lists the network, and leads back to it.
	m.SelectContainer("c1")
	m.ContainerDetail.Set(1)
	m.Go(PageContainers)
	for range 3 {
		p.Frame()
	}
	for _, want := range []string{"Reached as shop-web-1, web", "10.89.1.2"} {
		if !semShows(p, want) {
			t.Errorf("no %q in the Connect tab:\n%s", want, p.Semantics())
		}
	}
}

// semShows reports whether the window shows text, as a node's name.
func semShows(p *ggui.Probe, text string) bool {
	for _, n := range p.Semantics().All() {
		if n.Name == text {
			return true
		}
	}
	return false
}
