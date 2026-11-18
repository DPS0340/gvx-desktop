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
// registry once; a reference clears the results; and a question asked
// before is answered from memory.
func TestSearchAsksOnceTheTypingPauses(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.Query().Get("query"))
		mu.Unlock()
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

	m.OpenPull("")
	p.Frame()
	for _, typed := range []string{"n", "ng", "ngi", "ngin", "nginx"} {
		m.PullRef.Set(typed)
		p.Frame()
	}
	if !ggui.Peek(m.SearchBusy) {
		t.Error("not busy while the search waits")
	}
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

	// A search replaced before it was asked is never asked.
	m.PullRef.Set("redis")
	p.Frame()
	m.PullRef.Set("nginx") // answered from memory, at once
	p.Frame()
	if ggui.Peek(m.SearchFor) != "nginx" || ggui.Peek(m.SearchBusy) {
		t.Errorf("remembered answer: for %q, busy %v", ggui.Peek(m.SearchFor), ggui.Peek(m.SearchBusy))
	}
	time.Sleep(searchDelay + 100*time.Millisecond)
	p.Frame()
	if got := questions(); len(got) != 1 {
		t.Errorf("asked %v after going back, want only the first", got)
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
	if rs := ggui.Peek(m.SearchResults); rs != nil {
		t.Errorf("results after closing: %v", rs)
	}
}
