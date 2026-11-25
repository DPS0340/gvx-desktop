package sandbox

import (
	"cmp"
	"context"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// What rungv writes of the ports it publishes: -p as it starts, and
// -auto-publish as the guest listens on a port and lets it go.
var (
	publishRe     = regexp.MustCompile(`^rungvx?: publishing (\S+) -> (\d+)/(\w+)`)
	autoPublishRe = regexp.MustCompile(`^gvx: auto-published (\S+) -> (\d+)/(\w+)`)
	unpublishRe   = regexp.MustCompile(`^gvx: unpublished (\S+) -> (\d+)/(\w+)`)
	// busyRe is what -auto-publish says of a port the guest listens on
	// that this computer has taken already: it tries it once, and again
	// only after the guest lets it go.
	busyRe = regexp.MustCompile(`^gvx: warning: could not auto-publish (\S+)/(\w+): `)
)

// HostPort is the port number of the host address, 0 for a socket.
func (p PublishedPort) HostPort() int {
	_, port, err := net.SplitHostPort(p.Host)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(port)
	return n
}

func (p PublishedPort) sameHost(q PublishedPort) bool { return p.Host == q.Host && p.Proto == q.Proto }

// recordBusy adds b to st.Busy when no matching host/proto entry is already
// recorded, returning true only when newly added so stderr warnings and control
// events never double-notify EventPortBusy.
func (st *State) recordBusy(b PublishedPort) bool {
	if b.Host == "" {
		return false
	}
	if b.Proto == "" {
		b.Proto = "tcp"
	}
	if b.Source == "" {
		b.Source = "auto"
	}
	if b.Guest == 0 {
		b.Guest = b.HostPort()
	}
	if slices.ContainsFunc(st.Busy, b.sameHost) {
		return false
	}
	st.Busy = append(st.Busy, b)
	return true
}

// clearBusy removes any entry in st.Busy matching host and proto, returning
// true if st.Busy changed.
func (st *State) clearBusy(host, proto string) bool {
	if host == "" {
		return false
	}
	if proto == "" {
		proto = "tcp"
	}
	target := PublishedPort{Host: host, Proto: proto}
	before := len(st.Busy)
	st.Busy = slices.DeleteFunc(st.Busy, target.sameHost)
	return len(st.Busy) != before
}

// portLine applies a line of rungv's about its ports to the state. ok
// reports a line about ports; busy is the host address -auto-publish could
// not take, for EventPortBusy (non-empty only when newly recorded).
func (st *State) portLine(line string) (busy string, ok bool) {
	if rest, own := rungv.CutProgram(line, ": "); !strings.HasPrefix(line, "gvx: ") && !(own && strings.HasPrefix(rest, "publishing ")) {
		return "", false
	}
	if m := busyRe.FindStringSubmatch(line); m != nil {
		b := PublishedPort{Host: m[1], Proto: m[2], Source: "auto"}
		b.Guest = b.HostPort()
		if st.recordBusy(b) {
			return m[1], true
		}
		return "", true
	}
	add := func(m []string, source string) {
		guest, _ := strconv.Atoi(m[2])
		p := PublishedPort{Host: m[1], Guest: guest, Proto: m[3], Source: source}
		st.Ports = append(slices.DeleteFunc(st.Ports, p.sameHost), p)
		sortPorts(st.Ports)
		st.Busy = slices.DeleteFunc(st.Busy, p.sameHost)
	}
	if m := publishRe.FindStringSubmatch(line); m != nil {
		add(m, "static")
		return "", true
	}
	if m := autoPublishRe.FindStringSubmatch(line); m != nil {
		add(m, "auto")
		return "", true
	}
	if m := unpublishRe.FindStringSubmatch(line); m != nil {
		st.Ports = slices.DeleteFunc(st.Ports, PublishedPort{Host: m[1], Proto: m[3]}.sameHost)
		return "", true
	}
	return "", false
}

func sourceRank(source string) int {
	switch source {
	case "static":
		return 0
	case "auto":
		return 1
	}
	return 2
}

// sortPorts orders ports as the app lists them: those published at start
// first, then by host port.
func sortPorts(ports []PublishedPort) {
	slices.SortStableFunc(ports, func(a, b PublishedPort) int {
		return cmp.Or(
			cmp.Compare(sourceRank(a.Source), sourceRank(b.Source)),
			cmp.Compare(a.HostPort(), b.HostPort()),
			cmp.Compare(a.Host, b.Host),
			cmp.Compare(a.Proto, b.Proto),
		)
	})
}

// portsFromRules are the ports rungv port ls lists, as State has them.
func portsFromRules(rules []rungv.PortRule) []PublishedPort {
	out := make([]PublishedPort, 0, len(rules))
	for _, r := range rules {
		host := r.HostSocket
		if host == "" {
			host = net.JoinHostPort(r.HostIP, strconv.Itoa(int(r.HostPort)))
		}
		out = append(out, PublishedPort{Host: host, Guest: int(r.GuestPort), Proto: r.Proto, Source: r.Source})
	}
	sortPorts(out)
	return out
}

// syncPortsLocked has a run with a control socket read its ports from
// rungv (port ls) soon, which knows them all, those published by port add
// too; a run without one keeps what rungv's lines told. s.mu is held.
func (s *Supervisor) syncPortsLocked(id string, p *proc) {
	if !p.state.Control || p.paths.ControlSocket == "" {
		return
	}
	if p.portSync {
		p.portAgain = true
		return
	}
	p.portSync = true
	go s.syncPorts(id, p)
}

// syncPortsDelayNS gathers the lines of a burst of ports into one read.
var syncPortsDelayNS atomic.Int64

func init() {
	syncPortsDelayNS.Store(int64(250 * time.Millisecond))
}

func getSyncPortsDelay() time.Duration {
	return time.Duration(syncPortsDelayNS.Load())
}

func setSyncPortsDelay(d time.Duration) time.Duration {
	return time.Duration(syncPortsDelayNS.Swap(int64(d)))
}

func (s *Supervisor) syncPorts(id string, p *proc) {
	changed := false
	for again := true; again; {
		time.Sleep(getSyncPortsDelay())
		s.mu.Lock()
		p.portAgain = false // the read below sees what came before it
		s.mu.Unlock()
		q, client, target, err := s.controlTarget(id)
		var rules []rungv.PortRule
		if err == nil && q == p {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			rules, err = client.Ports(ctx, target)
			cancel()
		}
		s.mu.Lock()
		active := s.procs[id] == p && p.state.Status.Active() && !p.stopping
		if err == nil && active {
			ports := portsFromRules(rules)
			if !slices.Equal(ports, p.state.Ports) {
				p.state.Ports, changed = ports, true
			}
			for _, pp := range ports {
				if p.state.clearBusy(pp.Host, pp.Proto) {
					changed = true
				}
			}
		}
		again = p.portAgain && active
		p.portSync = again
		s.mu.Unlock()
	}
	if changed {
		s.notify(Event{ID: id, Kind: EventChanged, State: s.State(id)})
	}
}
