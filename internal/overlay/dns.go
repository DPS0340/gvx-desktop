package overlay

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// dnsServer answers one container's DNS queries, which its sandbox's DNS
// relay sends from the host (rungv -host-dns). It answers the names of the
// containers that share a network with it, and passes every other query
// to the host's resolvers, as Docker's embedded DNS server does.
type dnsServer struct {
	addr         netip.AddrPort
	udp          net.PacketConn
	tcp          net.Listener
	resolve      func(name string) []netip.Addr
	up           *upstreams // nil refuses other names
	transientErr func(error) bool
	wg           sync.WaitGroup
}

const answerTTL = 10

func startDNS(resolve func(string) []netip.Addr, up *upstreams) (*dnsServer, error) {
	var lastErr error
	for range 16 {
		udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		if sc, ok := udp.(syscall.Conn); ok {
			disableUDPConnReset(sc)
		}
		port := udp.LocalAddr().(*net.UDPAddr).Port
		tcp, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			udp.Close()
			lastErr = err
			continue
		}
		s := &dnsServer{
			addr:    netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), uint16(port)),
			udp:     udp,
			tcp:     tcp,
			resolve: resolve,
			up:      up,
		}
		s.wg.Add(2)
		go s.serveUDP()
		go s.serveTCP()
		return s, nil
	}
	return nil, lastErr
}

func (s *dnsServer) close() {
	s.udp.Close()
	s.tcp.Close()
	s.wg.Wait()
}

func (s *dnsServer) isTransient(err error) bool {
	if s.transientErr != nil {
		return s.transientErr(err)
	}
	return isPlatformTransientUDPErr(err)
}

func (s *dnsServer) serveUDP() {
	defer s.wg.Done()
	buf := make([]byte, 65535)
	for {
		n, addr, err := s.udp.ReadFrom(buf)
		if err != nil {
			if s.isTransient(err) {
				continue
			}
			return
		}
		q := append([]byte(nil), buf[:n]...)
		go func() {
			if out := s.answer(q, "udp"); out != nil {
				s.udp.WriteTo(out, addr)
			}
		}()
	}
}

func (s *dnsServer) serveTCP() {
	defer s.wg.Done()
	for {
		c, err := s.tcp.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			for {
				c.SetDeadline(time.Now().Add(10 * time.Second))
				var l [2]byte
				if _, err := io.ReadFull(c, l[:]); err != nil {
					return
				}
				q := make([]byte, binary.BigEndian.Uint16(l[:]))
				if _, err := io.ReadFull(c, q); err != nil {
					return
				}
				out := s.answer(q, "tcp")
				if out == nil {
					return
				}
				binary.BigEndian.PutUint16(l[:], uint16(len(out)))
				if _, err := c.Write(append(l[:], out...)); err != nil {
					return
				}
			}
		}()
	}
}

// answer returns the response to the query q, or nil to drop it.
func (s *dnsServer) answer(q []byte, network string) []byte {
	return answerDNS(q, network, s.resolve, s.up)
}

func answerDNS(q []byte, network string, resolve func(string) []netip.Addr, up *upstreams) []byte {
	var p dnsmessage.Parser
	h, err := p.Start(q)
	if err != nil {
		return nil
	}
	question, err := p.Question()
	if err != nil {
		return nil
	}
	if h.Response {
		return nil
	}
	name := strings.ToLower(strings.TrimSuffix(question.Name.String(), "."))
	if addrs := resolve(name); len(addrs) > 0 {
		return local(h, question, addrs)
	}
	if up == nil {
		return failure(h, question, dnsmessage.RCodeRefused)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := up.exchange(ctx, q, network, question)
	if err != nil {
		return failure(h, question, dnsmessage.RCodeServerFailure)
	}
	return out
}

// local answers a name of the containers' own: A records for addresses,
// and an empty answer for other types, which tells resolvers the name
// exists so they do not ask on.
func local(h dnsmessage.Header, q dnsmessage.Question, addrs []netip.Addr) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:                 h.ID,
		Response:           true,
		Authoritative:      true,
		RecursionDesired:   h.RecursionDesired,
		RecursionAvailable: true,
	})
	b.EnableCompression()
	if b.StartQuestions() != nil || b.Question(q) != nil || b.StartAnswers() != nil {
		return nil
	}
	if q.Type == dnsmessage.TypeA && q.Class == dnsmessage.ClassINET {
		for _, a := range addrs {
			if !a.Is4() {
				continue
			}
			rh := dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: answerTTL}
			if b.AResource(rh, dnsmessage.AResource{A: a.As4()}) != nil {
				return nil
			}
		}
	}
	out, err := b.Finish()
	if err != nil {
		return nil
	}
	return out
}

func failure(h dnsmessage.Header, q dnsmessage.Question, rcode dnsmessage.RCode) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:                 h.ID,
		Response:           true,
		RecursionDesired:   h.RecursionDesired,
		RecursionAvailable: true,
		RCode:              rcode,
	})
	if b.StartQuestions() != nil || b.Question(q) != nil {
		return nil
	}
	out, _ := b.Finish()
	return out
}

// upstreams are the host's DNS servers, read again now and then so a
// change of network reaches the containers.
type upstreams struct {
	mu      sync.Mutex
	servers []string
	read    time.Time
	// discover returns the servers as host:port; tests replace it.
	discover func() []string
}

const upstreamsTTL = 30 * time.Second

func (u *upstreams) list() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.read.IsZero() || time.Since(u.read) > upstreamsTTL {
		u.servers = u.discover()
		u.read = time.Now()
	}
	return u.servers
}

// exchange sends q to the host's servers in turn. With none, it answers A
// and AAAA queries from the system resolver.
func (u *upstreams) exchange(ctx context.Context, q []byte, network string, question dnsmessage.Question) ([]byte, error) {
	servers := u.list()
	var lastErr error = errors.New("no DNS servers")
	for _, srv := range servers {
		out, err := exchangeWith(ctx, srv, q, network)
		if err == nil {
			return out, nil
		}
		lastErr = err
	}
	if len(servers) == 0 {
		return systemAnswer(ctx, q, question)
	}
	return nil, lastErr
}

func exchangeWith(ctx context.Context, server string, q []byte, network string) ([]byte, error) {
	var d net.Dialer
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	c, err := d.DialContext(ctx, network, server)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	if network == "tcp" {
		var l [2]byte
		binary.BigEndian.PutUint16(l[:], uint16(len(q)))
		if _, err := c.Write(append(l[:], q...)); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return nil, err
		}
		out := make([]byte, binary.BigEndian.Uint16(l[:]))
		if _, err := io.ReadFull(c, out); err != nil {
			return nil, err
		}
		return out, nil
	}
	if _, err := c.Write(q); err != nil {
		return nil, err
	}
	buf := make([]byte, 65535)
	for {
		n, err := c.Read(buf)
		if err != nil {
			return nil, err
		}
		if n >= 12 && buf[0] == q[0] && buf[1] == q[1] {
			return buf[:n], nil
		}
	}
}

// systemAnswer answers an A or AAAA query with the system resolver, for a
// host whose DNS servers could not be read.
func systemAnswer(ctx context.Context, q []byte, question dnsmessage.Question) ([]byte, error) {
	var p dnsmessage.Parser
	h, err := p.Start(q)
	if err != nil {
		return nil, err
	}
	network := ""
	switch question.Type {
	case dnsmessage.TypeA:
		network = "ip4"
	case dnsmessage.TypeAAAA:
		network = "ip6"
	default:
		return failure(h, question, dnsmessage.RCodeNotImplemented), nil
	}
	name := strings.TrimSuffix(question.Name.String(), ".")
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, network, name)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return failure(h, question, dnsmessage.RCodeNameError), nil
		}
		return nil, err
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, RecursionDesired: h.RecursionDesired, RecursionAvailable: true})
	b.EnableCompression()
	if b.StartQuestions() != nil || b.Question(question) != nil || b.StartAnswers() != nil {
		return nil, errors.New("dns: build")
	}
	for _, a := range addrs {
		a = a.Unmap()
		rh := dnsmessage.ResourceHeader{Name: question.Name, Class: dnsmessage.ClassINET, TTL: 30}
		switch {
		case question.Type == dnsmessage.TypeA && a.Is4():
			rh.Type = dnsmessage.TypeA
			b.AResource(rh, dnsmessage.AResource{A: a.As4()})
		case question.Type == dnsmessage.TypeAAAA && a.Is6():
			rh.Type = dnsmessage.TypeAAAA
			b.AAAAResource(rh, dnsmessage.AAAAResource{AAAA: a.As16()})
		}
	}
	return b.Finish()
}
