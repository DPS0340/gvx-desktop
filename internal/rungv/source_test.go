package rungv

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSourceHeadSaysHowFarMainIsPast(t *testing.T) {
	const head = "e410633f24cf34fddfde473e48dde172593dc5af"
	compared := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/commits/main":
			w.Write([]byte(`{"sha":"` + head + `","commit":{"message":"fix: a thing\n\nand why"}}`))
		case "/compare/aaaa...main":
			compared++
			w.Write([]byte(`{"ahead_by":3}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := sourceAPI
	sourceAPI = srv.URL
	defer func() { sourceAPI = old }()

	for _, c := range []struct {
		installed string
		ahead     int
	}{{"", -1}, {head, 0}, {"aaaa", 3}, {"unknown", -1}} {
		h, err := SourceHead(context.Background(), c.installed)
		if err != nil {
			t.Fatal(err)
		}
		if h.SHA != head || h.Subject != "fix: a thing" || h.Ahead != c.ahead {
			t.Errorf("installed %q: %+v, want ahead %d", c.installed, h, c.ahead)
		}
	}
	if compared != 1 {
		t.Errorf("compared %d times, want once: the head itself needs no comparing", compared)
	}
}
