package sandbox

import (
	"cmp"
	"context"
	"net"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// What rungv writes of the ports it publishes: -p as it starts, and
// -auto-publish as the guest listens on a port and lets it go.
var (
	publishRe     = regexp.MustCompile(`^rungv: publishing (\S+) -> (\d+)/(\w+)`)
	autoPublishRe = regexp.MustCompile(`^gvx: auto-published (\S+) -> (\d+)/(\w+)`)
	unpublishRe   = regexp.MustCompile(`^gvx: unpublished (\S+) -> (\d+)/(\w+)`)
	// busyRe is what -auto-publish says of a port the guest listens on
	// that this computer has taken already: it tries it once, and again
	// only after the guest lets it go.
	busyRe = regexp.MustCompile(`^gvx: warning: could not auto-publish (\S+)/\w+: `)
)

// portLine applies a line of rungv's about its ports to the state. ok
// reports a line about ports; busy is the host address -auto-publish could
// not take, for EventPortBusy.
func (st *State) portLine(line string) (busy string, ok bool) {
	if m := busyRe.FindStringSubmatch(line); m != nil {
		if !slices.Contains(st.Busy, m[1]) {
			st.Busy = append(st.Busy, m[1])
		}
		return m[1], true
	}
	add := func(m []string, source string) {
		guest, _ := strconv.Atoi(m[2])
		p := PublishedPort{Host: m[1], Guest: guest, Proto: m[3], Source: source}
		st.Ports = slices.DeleteFunc(st.Ports, func(q PublishedPort) bool { return q.Host == p.Host && q.Proto == p.Proto })
		st.Ports = append(st.Ports, p)
		sortPorts(st.Ports)
		st.Busy = slices.DeleteFunc(st.Busy, func(h string) bool { return h == p.Host })
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
		st.Ports = slices.DeleteFunc(st.Ports, func(q PublishedPort) bool { return q.Host == m[1] && q.Proto == m[3] })
		return "", true
	}
	return "", false
}

// sortPorts orders ports as the app lists them: those published at start
// first, then by host port.
func sortPorts(ports []PublishedPort) {
	rank := map[string]int{"static": 0, "auto": 1, "dynamic": 2}
	slices.SortStableFunc(ports, func(a, b PublishedPort) int {
		return cmp.Or(
			cmp.Compare(rank[a.Source], rank[b.Source]),
			cmp.Compare(hostPort(a.Host), hostPort(b.Host)),
			cmp.Compare(a.Host, b.Host),
			cmp.Compare(a.Proto, b.Proto),
		)
	})
}

func hostPort(host string) int {
	_, port, err := net.SplitHostPort(host)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(port)
	return n
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

// syncPortsDelay gathers the lines of a burst of ports into one read.
var syncPortsDelay = 250 * time.Millisecond

func (s *Supervisor) syncPorts(id string, p *proc) {
	changed := false
	for {
		time.Sleep(syncPortsDelay)
		s.mu.Lock()
		active := s.procs[id] == p && p.state.Status.Active() && !p.stopping
		target := rungv.Target{Socket: p.paths.ControlSocket, TokenFile: p.paths.TokenFile}
		s.mu.Unlock()
		var rules []rungv.PortRule
		var err error = errNotRunning
		if client := s.clientFor(p); active && client != nil && client.Path != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			rules, err = client.Ports(ctx, target)
			cancel()
		}
		s.mu.Lock()
		if err == nil && s.procs[id] == p && p.state.Status.Active() {
			if ports := portsFromRules(rules); !slices.Equal(ports, p.state.Ports) {
				p.state.Ports, changed = ports, true
			}
		}
		again := p.portAgain && active
		p.portAgain = false
		if !again {
			p.portSync = false
		}
		s.mu.Unlock()
		if !again {
			break
		}
	}
	if changed {
		s.notify(Event{ID: id, Kind: EventChanged, State: s.State(id)})
	}
}
