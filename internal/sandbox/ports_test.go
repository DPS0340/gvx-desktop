package sandbox

import (
	"slices"
	"testing"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// What rungv writes of its ports keeps the state's ports and busy ports:
// -p at start, -auto-publish as the guest listens and lets go.
func TestPortLines(t *testing.T) {
	var st State
	for _, line := range []string{
		"rungv: publishing 127.0.0.1:9001 -> 9001/tcp",
		"rungv: publishing 127.0.0.1:9000 -> 9000/tcp",
		"gvx: warning: could not auto-publish 127.0.0.1:5432/tcp: listen tcp 127.0.0.1:5432: bind: address already in use",
		"gvx: auto-published 127.0.0.1:8080 -> 8080/tcp",
		"gvx: auto-published 127.0.0.1:53 -> 53/udp",
	} {
		if _, ok := st.portLine(line); !ok {
			t.Fatalf("not a port line: %q", line)
		}
	}
	want := []PublishedPort{
		{Host: "127.0.0.1:9000", Guest: 9000, Proto: "tcp", Source: "static"},
		{Host: "127.0.0.1:9001", Guest: 9001, Proto: "tcp", Source: "static"},
		{Host: "127.0.0.1:53", Guest: 53, Proto: "udp", Source: "auto"},
		{Host: "127.0.0.1:8080", Guest: 8080, Proto: "tcp", Source: "auto"},
	}
	if !slices.Equal(st.Ports, want) {
		t.Errorf("ports %+v", st.Ports)
	}
	if !slices.Equal(st.Busy, []string{"127.0.0.1:5432"}) {
		t.Errorf("busy %v", st.Busy)
	}

	// The guest lets 8080 go; 5432 is free again and published after all.
	st.portLine("gvx: unpublished 127.0.0.1:8080 -> 8080/tcp")
	st.portLine("gvx: auto-published 127.0.0.1:5432 -> 5432/tcp")
	if slices.ContainsFunc(st.Ports, func(p PublishedPort) bool { return p.Guest == 8080 }) {
		t.Errorf("8080 still published: %+v", st.Ports)
	}
	if len(st.Busy) != 0 || !slices.ContainsFunc(st.Ports, func(p PublishedPort) bool { return p.Host == "127.0.0.1:5432" }) {
		t.Errorf("5432: busy %v ports %+v", st.Busy, st.Ports)
	}
	// Published twice, it is listed once.
	st.portLine("gvx: auto-published 127.0.0.1:5432 -> 5432/tcp")
	if n := len(st.Ports); n != 4 {
		t.Errorf("%d ports: %+v", n, st.Ports)
	}
	if busy, ok := st.portLine("rungv: platform=hvf"); ok || busy != "" {
		t.Error("a line about something else")
	}
	if busy, _ := st.portLine("gvx: warning: could not auto-publish 127.0.0.1:80/tcp: denied"); busy != "127.0.0.1:80" {
		t.Errorf("busy %q", busy)
	}
}

func TestPortsFromRules(t *testing.T) {
	got := portsFromRules([]rungv.PortRule{
		{Proto: "tcp", HostIP: "127.0.0.1", HostPort: 18080, GuestPort: 80, Source: "dynamic"},
		{Proto: "tcp", HostIP: "127.0.0.1", HostPort: 3000, GuestPort: 3000, Source: "auto"},
		{Proto: "tcp", HostSocket: "/Users/me/.gvx/engine/dockerd.sock", GuestPort: 2375, Source: "static"},
		{Proto: "tcp", HostIP: "::1", HostPort: 9000, GuestPort: 9000, Source: "static"},
	})
	want := []PublishedPort{
		{Host: "/Users/me/.gvx/engine/dockerd.sock", Guest: 2375, Proto: "tcp", Source: "static"},
		{Host: "[::1]:9000", Guest: 9000, Proto: "tcp", Source: "static"},
		{Host: "127.0.0.1:3000", Guest: 3000, Proto: "tcp", Source: "auto"},
		{Host: "127.0.0.1:18080", Guest: 80, Proto: "tcp", Source: "dynamic"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("ports\n got %+v\nwant %+v", got, want)
	}
}

func TestGracefulStop(t *testing.T) {
	for _, tc := range []struct {
		recipe string
		in     time.Duration
		grace  time.Duration
		ok     bool
	}{
		{"", 15 * time.Second, 0, false},
		{"docker", 10 * time.Second, dockerdStopGrace, true},
		{"docker", 60 * time.Second, 60 * time.Second, true},
		{"k3s", 15 * time.Second, 15 * time.Second, true},
		{"registry", 5 * time.Second, 5 * time.Second, true},
	} {
		grace, ok := Spec{Recipe: tc.recipe}.gracefulStop(tc.in)
		if grace != tc.grace || ok != tc.ok {
			t.Errorf("%q %v: %v %v", tc.recipe, tc.in, grace, ok)
		}
	}
}

func TestStateEqualBusy(t *testing.T) {
	a := State{Busy: []string{"127.0.0.1:80"}}
	if a.Equal(State{}) || !a.Equal(State{Busy: []string{"127.0.0.1:80"}}) {
		t.Error("Busy is not compared")
	}
}
