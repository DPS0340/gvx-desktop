package sandbox

import (
	"slices"
	"testing"

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
	if !slices.Equal(st.Busy, []PublishedPort{{Host: "127.0.0.1:5432", Guest: 5432, Proto: "tcp", Source: "auto"}}) {
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

// rungvx, as gvx calls rungv since v0.23, writes the ports it publishes
// at start under its own name.
func TestPortLinesFromRungvx(t *testing.T) {
	var st State
	if _, ok := st.portLine("rungvx: publishing 127.0.0.1:9000 -> 9000/tcp"); !ok {
		t.Fatal("not a port line")
	}
	if !slices.Equal(st.Ports, []PublishedPort{{Host: "127.0.0.1:9000", Guest: 9000, Proto: "tcp", Source: "static"}}) {
		t.Errorf("ports %+v", st.Ports)
	}
	if _, ok := st.portLine("rungvx: platform=hvf"); ok {
		t.Error("a line about something else")
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

func TestStateEqualBusy(t *testing.T) {
	busy := []PublishedPort{{Host: "127.0.0.1:80", Guest: 80, Proto: "tcp", Source: "auto"}}
	a := State{Busy: busy}
	if a.Equal(State{}) || !a.Equal(State{Busy: slices.Clone(busy)}) {
		t.Error("Busy is not compared")
	}
}

func TestLogBufferSince(t *testing.T) {
	b := NewLogBuffer(10)
	for i := range 25 {
		b.Add(Stdout, string(rune('a'+i)))
	}
	got := b.Since(22)
	if len(got) != 3 || got[0].Seq != 23 || got[2].Text != "y" {
		t.Errorf("since 22: %+v", got)
	}
	if len(b.Since(0)) != len(b.Snapshot()) || len(b.Since(25)) != 0 {
		t.Error("ends")
	}
}
