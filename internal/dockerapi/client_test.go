package dockerapi

import (
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// engine serves handler as a dockerd on TCP would, and connects to it.
func engine(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// frame is a run of bytes of a multiplexed log stream behind its header.
func frame(stream byte, s string) []byte {
	h := make([]byte, 8, 8+len(s))
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	return append(h, s...)
}

func TestContainersAndActions(t *testing.T) {
	var calls []string
	c := engine(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		switch {
		case r.URL.Path == "/containers/json":
			_, _ = w.Write([]byte(`[{"Id":"3f2a9c1b7d4e5f60aa","Names":["/web"],"Image":"nginx","State":"running","Status":"Up 2 hours",
				"Ports":[{"PrivatePort":80,"PublicPort":9000,"Type":"tcp"},{"PrivatePort":80,"PublicPort":9000,"Type":"tcp"}]}]`))
		case r.URL.Path == "/containers/gone/start":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"No such container: gone"}`))
		case r.URL.Path == "/containers/web/stop":
			w.WriteHeader(http.StatusNotModified) // already stopped
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	ctx := context.Background()
	list, err := c.Containers(ctx, true)
	if err != nil || len(list) != 1 {
		t.Fatalf("Containers: %v, %v", list, err)
	}
	if ctr := list[0]; ctr.Name() != "web" || ctr.ShortID() != "3f2a9c1b7d4e" || ctr.PortsText() != "9000→80/tcp" {
		t.Errorf("container %q %q %q", ctr.Name(), ctr.ShortID(), ctr.PortsText())
	}
	if err := c.Stop(ctx, "web"); err != nil {
		t.Errorf("a 304 is no error: %v", err)
	}
	if err := c.Start(ctx, "gone"); err == nil || err.Error() != "No such container: gone" {
		t.Errorf("the engine's message: %v", err)
	}
	if err := c.Remove(ctx, "web", true); err != nil {
		t.Error(err)
	}
	want := []string{"GET /containers/json?all=1", "POST /containers/web/stop?t=10", "POST /containers/gone/start", "DELETE /containers/web?force=1"}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests\n%s\nwant\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}

func TestLogsOfAContainerWithoutATerminal(t *testing.T) {
	var query string
	c := engine(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Path + "?" + r.URL.RawQuery
		var b []byte
		b = append(b, frame(1, "2026-11-16T06:24:33.000000001Z ready\n")...)
		b = append(b, frame(2, "2026-11-16T06:24:34.5Z warn: slow")...) // a line in two runs
		b = append(b, frame(2, " disk\n")...)
		b = append(b, frame(1, "2026-11-16T06:24:35Z \n")...) // an empty line
		b = append(b, frame(1, "2026-11-16T06:24:36Z last, unterminated")...)
		_, _ = w.Write(b)
	})
	lines, err := c.Logs(context.Background(), "web", 200)
	if err != nil {
		t.Fatal(err)
	}
	if query != "/containers/web/logs?stdout=1&stderr=1&timestamps=1&tail=200" {
		t.Errorf("asked for %s", query)
	}
	want := []LogLine{
		{Time: time.Date(2026, 11, 16, 6, 24, 33, 1, time.UTC), Text: "ready"},
		{Stderr: true, Time: time.Date(2026, 11, 16, 6, 24, 34, 5e8, time.UTC), Text: "warn: slow disk"},
		{Time: time.Date(2026, 11, 16, 6, 24, 35, 0, time.UTC), Text: ""},
		{Time: time.Date(2026, 11, 16, 6, 24, 36, 0, time.UTC), Text: "last, unterminated"},
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines: %+v", len(lines), lines)
	}
	for i := range want {
		if g, w := lines[i], want[i]; g.Stderr != w.Stderr || !g.Time.Equal(w.Time) || g.Text != w.Text {
			t.Errorf("line %d: %+v, want %+v", i, g, w)
		}
	}
}

func TestLogsOfAContainerWithATerminal(t *testing.T) {
	c := engine(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("2026-11-16T06:24:33Z $ ls\r\n2026-11-16T06:24:33Z bin  etc\r\nno time here\n"))
	})
	lines, err := c.Logs(context.Background(), "tty", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 || lines[0].Text != "$ ls" || lines[1].Text != "bin  etc" || lines[1].Stderr {
		t.Fatalf("lines %+v", lines)
	}
	if lines[2].Text != "no time here" || !lines[2].Time.IsZero() {
		t.Errorf("a line with no time: %+v", lines[2])
	}
}

func TestLogsOfAMissingContainer(t *testing.T) {
	c := engine(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"No such container: x"}`))
	})
	if _, err := c.Logs(context.Background(), "x", 10); err == nil || !strings.Contains(err.Error(), "No such container") {
		t.Fatalf("err %v", err)
	}
}

func TestExitCode(t *testing.T) {
	for _, tc := range []struct {
		state, status string
		code          int
		ok            bool
	}{
		{"exited", "Exited (0) 2 hours ago", 0, true},
		{"exited", "Exited (137) About a minute ago", 137, true},
		{"running", "Up 2 hours", 0, false},
		{"created", "Created", 0, false},
		{"exited", "something else", 0, false},
	} {
		code, ok := Container{State: tc.state, Status: tc.status}.ExitCode()
		if code != tc.code || ok != tc.ok {
			t.Errorf("%s %q: %d %v", tc.state, tc.status, code, ok)
		}
	}
}
