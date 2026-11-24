package overlay

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/ironpark/gvx-desktop/internal/overlay/guestcfg"
)

func pfx(s string) netip.Prefix { return netip.MustParsePrefix(s) }
func addr(s string) netip.Addr  { return netip.MustParseAddr(s) }

// newTestFabric is a Fabric whose upstream is a fake DNS server that
// answers every A query with 192.0.2.1.
func newTestFabric(t *testing.T) *Fabric {
	t.Helper()
	up := fakeUpstream(t)
	f := New(t.Logf)
	f.up.discover = func() []string { return []string{up} }
	t.Cleanup(f.Close)
	return f
}

func fakeUpstream(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, a, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var m dnsmessage.Message
			if m.Unpack(buf[:n]) != nil {
				continue
			}
			m.Response = true
			m.Answers = []dnsmessage.Resource{{
				Header: dnsmessage.ResourceHeader{Name: m.Questions[0].Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 1},
				Body:   &dnsmessage.AResource{A: [4]byte{192, 0, 2, 1}},
			}}
			out, _ := m.Pack()
			pc.WriteTo(out, a)
		}
	}()
	return pc.LocalAddr().String()
}

func query(t *testing.T, server netip.AddrPort, network, name string, typ dnsmessage.Type) dnsmessage.Message {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 77, RecursionDesired: true})
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name + "."), Type: typ, Class: dnsmessage.ClassINET})
	q, _ := b.Finish()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exchangeWith(ctx, server.String(), q, network)
	if err != nil {
		t.Fatalf("query %s over %s: %v", name, network, err)
	}
	var m dnsmessage.Message
	if err := m.Unpack(out); err != nil {
		t.Fatal(err)
	}
	if m.ID != 77 {
		t.Fatalf("answer ID = %d", m.ID)
	}
	return m
}

func aRecords(m dnsmessage.Message) []string {
	var out []string
	for _, a := range m.Answers {
		if r, ok := a.Body.(*dnsmessage.AResource); ok {
			out = append(out, netip.AddrFrom4(r.A).String())
		}
	}
	return out
}

func TestJoinConfiguresTheGuestAndThePeer(t *testing.T) {
	f := newTestFabric(t)
	cfg, dns, err := f.Join("c1", []netip.Prefix{pfx("10.89.1.2/24"), pfx("172.30.0.5/16")}, JoinOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.PrivateKey) != 32 || len(cfg.HubPublicKey) != 32 {
		t.Fatal("keys missing")
	}
	if !strings.HasPrefix(cfg.Endpoint, HostGateway+":") {
		t.Fatalf("endpoint = %s", cfg.Endpoint)
	}
	if !slices.Equal(cfg.Routes, []string{"10.89.0.0/16", "172.30.0.0/16"}) {
		t.Fatalf("routes = %v", cfg.Routes)
	}
	if !dns.Addr().IsLoopback() {
		t.Fatalf("dns = %s", dns)
	}
	if _, err := guestcfg.Decode(mustEncode(t, cfg)); err != nil {
		t.Fatal(err)
	}
	ipc, err := f.hub.dev.IpcGet()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ipc, "allowed_ip=10.89.1.2/32") || !strings.Contains(ipc, "allowed_ip=172.30.0.5/32") {
		t.Fatalf("hub peer:\n%s", ipc)
	}

	f.Leave("c1")
	ipc, _ = f.hub.dev.IpcGet()
	if strings.Contains(ipc, "public_key=") {
		t.Fatalf("peer left behind:\n%s", ipc)
	}
}

func TestSetAddrsKeepsKeyAndUpdatesHub(t *testing.T) {
	f := newTestFabric(t)
	if _, _, ok, err := f.SetAddrs("missing", []netip.Prefix{pfx("10.89.1.2/24")}); err != nil || ok {
		t.Fatalf("SetAddrs(missing) = ok=%v err=%v", ok, err)
	}
	orig, _, err := f.Join("c1", []netip.Prefix{pfx("10.89.1.2/24")}, JoinOptions{MTU: 8920})
	if err != nil {
		t.Fatal(err)
	}
	updated, gen1, ok, err := f.SetAddrs("c1", []netip.Prefix{pfx("10.89.1.2/24"), pfx("10.89.2.7/24"), pfx("172.31.0.9/16")})
	if err != nil || !ok || gen1 == 0 {
		t.Fatalf("SetAddrs = gen=%d ok=%v err=%v", gen1, ok, err)
	}
	if !slices.Equal(updated.PrivateKey, orig.PrivateKey) {
		t.Fatal("SetAddrs changed private key")
	}
	if !slices.Equal(updated.HubPublicKey, orig.HubPublicKey) || updated.Endpoint != orig.Endpoint || updated.MTU != 8920 {
		t.Fatalf("SetAddrs lost peer settings: %+v vs %+v", updated, orig)
	}
	if !slices.Equal(updated.Addresses, []string{"10.89.1.2/24", "10.89.2.7/24", "172.31.0.9/16"}) {
		t.Fatalf("addresses = %v", updated.Addresses)
	}
	if !slices.Equal(updated.Routes, []string{"10.89.0.0/16", "172.31.0.0/16"}) {
		t.Fatalf("routes = %v", updated.Routes)
	}
	ipc, err := f.hub.dev.IpcGet()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"allowed_ip=10.89.1.2/32", "allowed_ip=10.89.2.7/32", "allowed_ip=172.31.0.9/32"} {
		if !strings.Contains(ipc, want) {
			t.Fatalf("hub missing %s:\n%s", want, ipc)
		}
	}
	// Disconnect removes old address from hub allowed IPs.
	shrunk, gen2, ok, err := f.SetAddrs("c1", []netip.Prefix{pfx("10.89.2.7/24")})
	if err != nil || !ok || gen2 <= gen1 {
		t.Fatalf("SetAddrs shrink = gen=%d ok=%v err=%v", gen2, ok, err)
	}
	// Stale generation rollback is ignored.
	if f.RevertAddrs("c1", gen1, []netip.Prefix{pfx("10.89.1.2/24")}) {
		t.Fatal("RevertAddrs accepted stale generation")
	}
	// Matching generation rollback succeeds.
	if !f.RevertAddrs("c1", gen2, []netip.Prefix{pfx("10.89.1.2/24")}) {
		t.Fatal("RevertAddrs rejected current generation")
	}
	if got, _ := f.PeerAddrs("c1"); !slices.Equal(got, []netip.Prefix{pfx("10.89.1.2/24")}) {
		t.Fatalf("after revert PeerAddrs = %v", got)
	}
	// Set back to shrunk, then empty.
	shrunk, _, ok, err = f.SetAddrs("c1", []netip.Prefix{pfx("10.89.2.7/24")})
	if err != nil || !ok {
		t.Fatalf("SetAddrs shrink = ok=%v err=%v", ok, err)
	}
	if !slices.Equal(shrunk.Addresses, []string{"10.89.2.7/24"}) {
		t.Fatalf("shrunk addresses = %v", shrunk.Addresses)
	}
	ipc, _ = f.hub.dev.IpcGet()
	if strings.Contains(ipc, "10.89.1.2/32") || !strings.Contains(ipc, "allowed_ip=10.89.2.7/32") {
		t.Fatalf("hub after shrink:\n%s", ipc)
	}
	// Disconnect all networks leaves empty allowed IPs and decodable config.
	empty, _, ok, err := f.SetAddrs("c1", nil)
	if err != nil || !ok || len(empty.Addresses) != 0 {
		t.Fatalf("SetAddrs empty = %+v ok=%v err=%v", empty, ok, err)
	}
	if _, err := guestcfg.Decode(mustEncode(t, empty)); err != nil {
		t.Fatalf("Decode empty config: %v", err)
	}
}

func mustEncode(t *testing.T, c guestcfg.Config) string {
	s, err := c.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNetworksSeparateContainers(t *testing.T) {
	f := newTestFabric(t)
	f.SetMembers([]Member{
		{ID: "web", Endpoints: map[string]Endpoint{
			"front": {Addr: addr("10.89.1.2"), Names: []string{"web", "Web-1"}},
			"back":  {Addr: addr("10.89.2.2"), Names: []string{"web"}},
		}},
		{ID: "db", Endpoints: map[string]Endpoint{
			"back": {Addr: addr("10.89.2.3"), Names: []string{"db", "postgres"}},
		}},
		{ID: "proxy", Endpoints: map[string]Endpoint{
			"front": {Addr: addr("10.89.1.3"), Names: []string{"proxy"}},
		}},
		{ID: "stopped", Endpoints: map[string]Endpoint{
			"back": {Addr: addr("10.89.2.4"), Names: []string{"db"}},
		}},
	})
	var dns = map[string]netip.AddrPort{}
	for id, addrs := range map[string][]netip.Prefix{
		"web":   {pfx("10.89.1.2/24"), pfx("10.89.2.2/24")},
		"db":    {pfx("10.89.2.3/24")},
		"proxy": {pfx("10.89.1.3/24")},
	} {
		_, d, err := f.Join(id, addrs, JoinOptions{})
		if err != nil {
			t.Fatal(err)
		}
		dns[id] = d
	}

	allow := []struct {
		src, dst string
		want     bool
	}{
		{"10.89.1.2", "10.89.1.3", true},  // web → proxy on front
		{"10.89.2.3", "10.89.2.2", true},  // db → web on back
		{"10.89.2.3", "10.89.1.2", true},  // db → web's other address
		{"10.89.1.3", "10.89.2.3", false}, // proxy → db: no shared network
		{"10.89.2.3", "10.89.2.4", false}, // to a container that is not running
		{"10.89.9.9", "10.89.2.3", false}, // from nobody
	}
	for _, c := range allow {
		if got := f.allow(addr(c.src), addr(c.dst)); got != c.want {
			t.Errorf("allow(%s, %s) = %v", c.src, c.dst, got)
		}
	}

	// web sees db and proxy; proxy does not see db; names fold case.
	if got := aRecords(query(t, dns["web"], "udp", "db", dnsmessage.TypeA)); !slices.Equal(got, []string{"10.89.2.3"}) {
		t.Errorf("web: db = %v", got)
	}
	if got := aRecords(query(t, dns["web"], "tcp", "POSTGRES", dnsmessage.TypeA)); !slices.Equal(got, []string{"10.89.2.3"}) {
		t.Errorf("web: POSTGRES over tcp = %v", got)
	}
	if got := aRecords(query(t, dns["proxy"], "udp", "web-1", dnsmessage.TypeA)); !slices.Equal(got, []string{"10.89.1.2"}) {
		t.Errorf("proxy: web-1 = %v", got)
	}
	if got := aRecords(query(t, dns["db"], "udp", "web", dnsmessage.TypeA)); !slices.Equal(got, []string{"10.89.2.2"}) {
		t.Errorf("db: web = %v", got)
	}
	// A name of the containers' own has no AAAA, and says so without an error.
	if m := query(t, dns["web"], "udp", "db", dnsmessage.TypeAAAA); m.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 0 {
		t.Errorf("web: db AAAA = %v %v", m.RCode, m.Answers)
	}
	// db is not on proxy's network, so the upstream answers.
	if got := aRecords(query(t, dns["proxy"], "udp", "db", dnsmessage.TypeA)); !slices.Equal(got, []string{"192.0.2.1"}) {
		t.Errorf("proxy: db = %v", got)
	}
	if got := aRecords(query(t, dns["web"], "udp", "example.com", dnsmessage.TypeA)); !slices.Equal(got, []string{"192.0.2.1"}) {
		t.Errorf("web: example.com = %v", got)
	}

	// proxy joins back at run time: it answers with the address its link
	// has, and reaches db.
	f.SetMembers([]Member{
		{ID: "web", Endpoints: map[string]Endpoint{"front": {Addr: addr("10.89.1.2"), Names: []string{"web"}}, "back": {Addr: addr("10.89.2.2"), Names: []string{"web"}}}},
		{ID: "db", Endpoints: map[string]Endpoint{"back": {Addr: addr("10.89.2.3"), Names: []string{"db"}}}},
		{ID: "proxy", Endpoints: map[string]Endpoint{"front": {Addr: addr("10.89.1.3"), Names: []string{"proxy"}}, "back": {Addr: addr("10.89.2.9"), Names: []string{"proxy"}}}},
	})
	if got := aRecords(query(t, dns["db"], "udp", "proxy", dnsmessage.TypeA)); !slices.Equal(got, []string{"10.89.1.3"}) {
		t.Errorf("db: proxy after connect = %v", got)
	}
	if !f.allow(addr("10.89.1.3"), addr("10.89.2.3")) {
		t.Error("proxy cannot reach db after connect")
	}

	f.Leave("db")
	if f.allow(addr("10.89.2.2"), addr("10.89.2.3")) {
		t.Error("a container that left is still reachable")
	}
}

func TestIsolatedContainersResolveOnlyContainers(t *testing.T) {
	f := newTestFabric(t)
	f.SetMembers([]Member{
		{ID: "db", Endpoints: map[string]Endpoint{"back": {Addr: addr("10.89.2.3"), Names: []string{"db"}}}},
		{ID: "api", Endpoints: map[string]Endpoint{"back": {Addr: addr("10.89.2.4"), Names: []string{"api"}}}},
	})
	_, dns, err := f.Join("db", []netip.Prefix{pfx("10.89.2.3/24")}, JoinOptions{Isolated: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Join("api", []netip.Prefix{pfx("10.89.2.4/24")}, JoinOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := aRecords(query(t, dns, "udp", "api", dnsmessage.TypeA)); !slices.Equal(got, []string{"10.89.2.4"}) {
		t.Errorf("api = %v", got)
	}
	if m := query(t, dns, "udp", "example.com", dnsmessage.TypeA); m.RCode != dnsmessage.RCodeRefused {
		t.Errorf("example.com = %v %v, want refused", m.RCode, m.Answers)
	}
}

func TestRouterDropsWhatItMayNotPass(t *testing.T) {
	r := newRouter(func(src, dst netip.Addr) bool { return dst == addr("10.89.1.3") }, nil)
	pkt := func(dst [4]byte) []byte {
		p := make([]byte, 28)
		p[0] = 0x45
		copy(p[12:], []byte{10, 89, 1, 2})
		copy(p[16:], dst[:])
		return p
	}
	if _, err := r.Write([][]byte{pkt([4]byte{10, 89, 1, 3}), pkt([4]byte{10, 89, 1, 4}), {0x60, 0}}, 0); err != nil {
		t.Fatal(err)
	}
	bufs := [][]byte{make([]byte, 100), make([]byte, 100)}
	sizes := make([]int, 2)
	n, err := r.Read(bufs, sizes, 0)
	if err != nil || n != 1 || sizes[0] != 28 || bufs[0][19] != 3 {
		t.Fatalf("Read = %d %v, sizes %v", n, err, sizes)
	}
	r.Close()
	if _, err := r.Read(bufs, sizes, 0); err == nil {
		t.Fatal("Read after Close did not fail")
	}
}

type flakyPacketConn struct {
	net.PacketConn
	mu        sync.Mutex
	failCount int
	err       error
}

func (c *flakyPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	c.mu.Lock()
	if c.failCount > 0 {
		c.failCount--
		err := c.err
		c.mu.Unlock()
		return 0, nil, err
	}
	c.mu.Unlock()
	return c.PacketConn.ReadFrom(p)
}

func TestServeUDPSurvivesTransientError(t *testing.T) {
	transientErr := errors.New("simulated WSAECONNRESET")

	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		udp.Close()
		t.Fatal(err)
	}
	flaky := &flakyPacketConn{PacketConn: udp, failCount: 3, err: transientErr}
	s := &dnsServer{
		addr:         netip.MustParseAddrPort(udp.LocalAddr().String()),
		udp:          flaky,
		tcp:          tcp,
		resolve:      func(name string) []netip.Addr { return []netip.Addr{addr("10.89.1.9")} },
		transientErr: func(err error) bool { return errors.Is(err, transientErr) },
	}
	s.wg.Add(2)
	go s.serveUDP()
	go s.serveTCP()
	t.Cleanup(s.close)

	if got := aRecords(query(t, s.addr, "udp", "web", dnsmessage.TypeA)); !slices.Equal(got, []string{"10.89.1.9"}) {
		t.Fatalf("UDP query after transient errors = %v, want [10.89.1.9]", got)
	}
}

func makeIPv4Packet(src, dst [4]byte, payload byte) []byte {
	p := make([]byte, 24)
	p[0] = 0x45
	copy(p[12:16], src[:])
	copy(p[16:20], dst[:])
	p[20] = payload
	return p
}

func TestDirectPeerPacketsDNSAndHubBridge(t *testing.T) {
	f := newTestFabric(t)
	f.SetMembers([]Member{
		{ID: "mvm-a", Endpoints: map[string]Endpoint{
			"app": {Addr: addr("10.89.1.2"), Names: []string{"mvm-a", "api"}},
		}},
		{ID: "mvm-b", Endpoints: map[string]Endpoint{
			"app": {Addr: addr("10.89.1.3"), Names: []string{"mvm-b", "db"}},
		}},
		{ID: "gvx-c", Endpoints: map[string]Endpoint{
			"app": {Addr: addr("10.89.1.4"), Names: []string{"gvx-c", "cache"}},
		}},
		{ID: "other", Endpoints: map[string]Endpoint{
			"isolated-net": {Addr: addr("10.89.2.2"), Names: []string{"other"}},
		}},
	})

	dpA, cfgA, err := f.JoinDirect("mvm-a", []netip.Prefix{pfx("10.89.1.2/24")}, JoinOptions{MTU: 60000})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfgA.Addresses, []string{"10.89.1.2/24"}) || cfgA.MTU != 60000 {
		t.Fatalf("cfgA = %+v", cfgA)
	}
	dpB, _, err := f.JoinDirect("mvm-b", []netip.Prefix{pfx("10.89.1.3/24")}, JoinOptions{Isolated: true})
	if err != nil {
		t.Fatal(err)
	}
	dpOther, _, err := f.JoinDirect("other", []netip.Prefix{pfx("10.89.2.2/24")}, JoinOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// 1. DirectPeer -> DirectPeer on shared network succeeds.
	dpA.Send(makeIPv4Packet([4]byte{10, 89, 1, 2}, [4]byte{10, 89, 1, 3}, 0x55))
	pkt, ok := dpB.Recv()
	if !ok || pkt[20] != 0x55 {
		t.Fatalf("dpB.Recv = (%x, %v)", pkt, ok)
	}

	// 2. Spoofed source IP and unshared network packets are dropped.
	dpA.Send(makeIPv4Packet([4]byte{10, 89, 1, 99}, [4]byte{10, 89, 1, 3}, 0x66))
	dpA.Send(makeIPv4Packet([4]byte{10, 89, 1, 2}, [4]byte{10, 89, 2, 2}, 0x77))
	select {
	case p := <-dpB.rx:
		t.Fatalf("spoofed packet delivered to dpB: %x", p)
	case p := <-dpOther.rx:
		t.Fatalf("cross-network packet delivered to dpOther: %x", p)
	case <-time.After(30 * time.Millisecond):
	}

	// 3. DirectPeer <-> WireGuard hub bridge (mvm-a <-> gvx-c).
	_, gvxDNS, err := f.Join("gvx-c", []netip.Prefix{pfx("10.89.1.4/24")}, JoinOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// gvx-c's DNS resolves direct peer mvm-a!
	if got := aRecords(query(t, gvxDNS, "udp", "api", dnsmessage.TypeA)); !slices.Equal(got, []string{"10.89.1.2"}) {
		t.Fatalf("gvx-c DNS api = %v, want [10.89.1.2]", got)
	}
	// WireGuard router.Write delivers directly to dpA without going back to router.queue.
	if _, err := f.hub.router.Write([][]byte{makeIPv4Packet([4]byte{10, 89, 1, 4}, [4]byte{10, 89, 1, 2}, 0x88)}, 0); err != nil {
		t.Fatal(err)
	}
	pkt, ok = dpA.Recv()
	if !ok || pkt[20] != 0x88 {
		t.Fatalf("dpA.Recv from hub = (%x, %v)", pkt, ok)
	}
	// DirectPeer -> WireGuard peer enqueues onto hub router.
	dpA.Send(makeIPv4Packet([4]byte{10, 89, 1, 2}, [4]byte{10, 89, 1, 4}, 0x99))
	if !f.CanReach(addr("10.89.1.2"), addr("10.89.1.4")) {
		t.Fatal("CanReach(mvm-a, gvx-c) = false")
	}

	// 4. DirectPeer.ExchangeDNS: local names, upstream, and isolated refusal.
	buildQuery := func(name string, typ dnsmessage.Type) []byte {
		b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 55, RecursionDesired: true})
		_ = b.StartQuestions()
		_ = b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name + "."), Type: typ, Class: dnsmessage.ClassINET})
		q, _ := b.Finish()
		return q
	}
	unpack := func(raw []byte) dnsmessage.Message {
		var m dnsmessage.Message
		if err := m.Unpack(raw); err != nil {
			t.Fatalf("unpack dns: %v", err)
		}
		return m
	}
	if got := aRecords(unpack(dpA.ExchangeDNS(buildQuery("cache", dnsmessage.TypeA), "tcp"))); !slices.Equal(got, []string{"10.89.1.4"}) {
		t.Fatalf("dpA DNS cache (tcp) = %v, want [10.89.1.4]", got)
	}
	if got := aRecords(unpack(dpA.ExchangeDNS(buildQuery("example.com", dnsmessage.TypeA), "udp"))); !slices.Equal(got, []string{"192.0.2.1"}) {
		t.Fatalf("dpA DNS example.com = %v, want [192.0.2.1]", got)
	}
	if m := unpack(dpB.ExchangeDNS(buildQuery("example.com", dnsmessage.TypeA), "udp")); m.RCode != dnsmessage.RCodeRefused {
		t.Fatalf("isolated dpB DNS example.com RCode = %v, want REFUSED", m.RCode)
	}
	if got := aRecords(unpack(dpB.ExchangeDNS(buildQuery("api", dnsmessage.TypeA), "udp"))); !slices.Equal(got, []string{"10.89.1.2"}) {
		t.Fatalf("isolated dpB DNS api = %v, want [10.89.1.2]", got)
	}

	// 5. SetAddrs / RevertAddrs / Leave on DirectPeer.
	upd, gen, ok, err := f.SetAddrs("mvm-a", []netip.Prefix{pfx("10.89.1.2/24"), pfx("10.89.3.5/24")})
	if err != nil || !ok || gen == 0 || len(upd.Addresses) != 2 {
		t.Fatalf("SetAddrs direct = (%+v, gen=%d, ok=%v, err=%v)", upd, gen, ok, err)
	}
	if !f.RevertAddrs("mvm-a", gen, []netip.Prefix{pfx("10.89.1.2/24")}) {
		t.Fatal("RevertAddrs direct failed")
	}
	f.Leave("mvm-a")
	if _, ok := dpA.Recv(); ok {
		t.Fatal("dpA.Recv should return ok=false after Leave")
	}
}
