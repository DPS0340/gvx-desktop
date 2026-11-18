package ui

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/registry"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// A search waits for the typing to pause, so that a word typed asks the
// registry once; a reference clears the results; a search called off is
// asked again when its words come back; and a question asked before is
// answered from memory. The pause is measured on the probe's clock, which
// moves only when the test advances it.
func TestSearchAsksOnceTheTypingPauses(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	var held bool // the first search for "slow" answers only once it is called off
	calledOff := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		mu.Lock()
		asked = append(asked, q)
		hold := q == "slow" && !held
		held = held || hold
		mu.Unlock()
		if hold {
			<-r.Context().Done()
			close(calledOff)
			return
		}
		w.Write([]byte(`{"results":[{"repo_name":"` + r.URL.Query().Get("query") + `","is_official":true}]}`))
	}))
	defer srv.Close()
	defer registry.Endpoints(srv.URL, srv.URL)()
	questions := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), asked...)
	}

	dir := t.TempDir()
	var m *Model
	p := ggui.ProbeBuilder(func() ggui.Widget { return Build(m) }, ggui.Sz(900, 700))
	p.Setup(func() {
		m = NewModel(store.Store{Dir: dir}, dir)
		m.Attach(p)
	})
	defer p.Close()
	p.Frame()
	settle := func(done func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			p.Frame()
			if done() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("did not settle; asked %v, results %v, busy %v, err %q",
			questions(), ggui.Peek(m.SearchResults), ggui.Peek(m.SearchBusy), ggui.Peek(m.SearchErr))
	}

	p.Advance(0) // from here the clock moves only when the test moves it
	m.OpenPull("")
	p.Frame()
	for _, typed := range []string{"n", "ng", "ngi", "ngin", "nginx"} {
		m.PullRef.Set(typed)
		p.Frame()
		p.Advance(searchDelay / 3)
	}
	if !ggui.Peek(m.SearchBusy) {
		t.Error("not busy while the search waits")
	}
	p.Advance(searchDelay - searchDelay/3 - time.Millisecond)
	if got := questions(); len(got) != 0 || m.search.cancel != nil {
		t.Errorf("asked %v before the typing paused", got)
	}
	p.Advance(time.Millisecond)
	settle(func() bool { return ggui.Peek(m.SearchFor) == "nginx" })
	if got := questions(); len(got) != 1 || got[0] != "nginx" {
		t.Errorf("asked %v, want nginx once", got)
	}
	if rs := ggui.Peek(m.SearchResults); len(rs) != 1 || rs[0].Name != "nginx" || ggui.Peek(m.SearchBusy) {
		t.Errorf("results %v, busy %v", rs, ggui.Peek(m.SearchBusy))
	}

	// A reference is pulled as it stands, not searched for.
	m.PullRef.Set("ghcr.io/org/app:1.0")
	p.Frame()
	if rs := ggui.Peek(m.SearchResults); rs != nil || ggui.Peek(m.SearchFor) != "" {
		t.Errorf("a reference left results %v", rs)
	}

	// A search still waiting for its answer is called off by a newer one, and
	// asked again when the field comes back to its words, though the words
	// the pause ended on are the same as before.
	m.PullRef.Set("slow")
	p.Frame()
	p.Advance(searchDelay)
	settle(func() bool { return len(questions()) == 2 })
	m.PullRef.Set("slowe")
	p.Frame()
	if m.search.cancel != nil || !ggui.Peek(m.SearchBusy) {
		t.Errorf("a newer search left the old one asking (busy %v)", ggui.Peek(m.SearchBusy))
	}
	select {
	case <-calledOff:
	case <-time.After(5 * time.Second):
		t.Fatal("the search called off was not stopped")
	}
	for until := time.Now().Add(100 * time.Millisecond); time.Now().Before(until); time.Sleep(5 * time.Millisecond) {
		p.Frame() // the answer it failed with comes, and must change nothing
		if !ggui.Peek(m.SearchBusy) || ggui.Peek(m.SearchErr) != "" {
			t.Fatalf("the search called off answered: busy %v, err %q", ggui.Peek(m.SearchBusy), ggui.Peek(m.SearchErr))
		}
	}
	m.PullRef.Set("slow")
	p.Frame()
	p.Advance(searchDelay)
	settle(func() bool { return ggui.Peek(m.SearchFor) == "slow" && !ggui.Peek(m.SearchBusy) })
	if got := questions(); len(got) != 3 || got[1] != "slow" || got[2] != "slow" {
		t.Errorf("asked %v, want nginx, then slow twice", got)
	}
	if e := ggui.Peek(m.SearchErr); e != "" {
		t.Errorf("the answer of the search called off was shown: %q", e)
	}

	// A search replaced before it was asked is never asked.
	m.PullRef.Set("redis")
	p.Frame()
	m.PullRef.Set("nginx") // answered from memory, at once
	p.Frame()
	if ggui.Peek(m.SearchFor) != "nginx" || ggui.Peek(m.SearchBusy) {
		t.Errorf("remembered answer: for %q, busy %v", ggui.Peek(m.SearchFor), ggui.Peek(m.SearchBusy))
	}
	p.Advance(2 * searchDelay)
	if got := questions(); len(got) != 3 || m.search.cancel != nil || ggui.Peek(m.SearchBusy) {
		t.Errorf("asked %v after going back, want nothing more", got)
	}

	// Choosing a result keeps the list while a tag is added to it.
	m.PickResult(ggui.Peek(m.SearchResults)[0])
	p.Frame()
	m.PullRef.Set("nginx:alpine")
	p.Frame()
	if rs := ggui.Peek(m.SearchResults); len(rs) != 1 {
		t.Errorf("a tag after the picked repository cleared the results: %v", rs)
	}

	// Closing the dialog forgets what it showed.
	m.PullOpen.Set(false)
	p.Frame()
	if rs := ggui.Peek(m.SearchResults); rs != nil || m.search.cancel != nil || ggui.Peek(m.search.waiting) != nil {
		t.Errorf("results after closing: %v", rs)
	}
}
