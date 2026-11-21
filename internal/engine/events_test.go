package engine

// Owner: workstream C.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestEventsReplayFiltersAndLiveStream(t *testing.T) {
	te := newTestEngine(t)

	t0 := time.Now().UTC().Add(-10 * time.Second)
	te.emit(Event{
		Type:       "image",
		Action:     "pull",
		ID:         "alpine:3.20",
		Attributes: map[string]string{"name": "alpine:3.20"},
		Time:       t0.Add(1 * time.Second),
	})
	te.emit(Event{
		Type:       "container",
		Action:     "start",
		ID:         "abcdef123456",
		Attributes: map[string]string{"name": "web", "image": "alpine:3.20", "app": "demo"},
		Time:       t0.Add(2 * time.Second),
	})

	// Past until replays matching events and closes immediately.
	q := url.Values{
		"since":   {fmt.Sprintf("%d", t0.Unix())},
		"until":   {fmt.Sprintf("%d", t0.Add(5*time.Second).Unix())},
		"filters": {`{"type":{"container":true},"label":{"app=demo":true}}`},
	}
	st, body := te.do(t, http.MethodGet, "/events?"+q.Encode(), nil)
	if st != http.StatusOK {
		t.Fatalf("GET /events replay = %d %s", st, body)
	}
	var msgs []eventMessage
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		var m eventMessage
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		msgs = append(msgs, m)
	}
	if len(msgs) != 1 || msgs[0].Type != "container" || msgs[0].Action != "start" || msgs[0].Actor.ID != "abcdef123456" || msgs[0].From != "alpine:3.20" {
		t.Fatalf("replay got %+v", msgs)
	}

	// Live stream receives container start emitted after connecting.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, te.srv.URL+"/events?filters="+url.QueryEscape(`{"event":{"start":true}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := te.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	// Emit a non-matching event followed by a matching container start.
	te.emit(Event{Type: "volume", Action: "create", ID: "v1"})
	te.emit(Event{Type: "container", Action: "start", ID: "live-ctr-1", Attributes: map[string]string{"name": "live"}})

	lineCh := make(chan string, 1)
	go func() {
		s := bufio.NewScanner(res.Body)
		if s.Scan() {
			lineCh <- s.Text()
		}
	}()
	select {
	case line := <-lineCh:
		var m eventMessage
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		if m.Type != "container" || m.Action != "start" || m.Actor.ID != "live-ctr-1" {
			t.Fatalf("unexpected live event: %+v", m)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for live container start event")
	}
}

func TestEventsRingCapNonBlocking(t *testing.T) {
	te := newTestEngine(t)
	for i := range maxEvents + 50 {
		te.emit(Event{Type: "container", Action: "create", ID: fmt.Sprintf("c%d", i)})
	}
	te.evt.mu.Lock()
	n := len(te.evt.ring)
	first := te.evt.ring[0].ID
	te.evt.mu.Unlock()
	if n != maxEvents {
		t.Fatalf("ring size = %d, want %d", n, maxEvents)
	}
	if first != "c50" {
		t.Fatalf("oldest retained event = %q, want c50", first)
	}
}

func TestEventsEndsOnEngineClose(t *testing.T) {
	te := newTestEngine(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, te.srv.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := te.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	// Confirm the stream is live by receiving one event.
	te.emit(Event{Type: "container", Action: "start", ID: "before-close"})
	sc := bufio.NewScanner(res.Body)
	if !sc.Scan() {
		t.Fatalf("expected event before close, err=%v", sc.Err())
	}

	// Closing the engine must terminate the /events stream cleanly.
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, res.Body)
		done <- err
	}()

	if err := te.Engine.Close(time.Second); err != nil {
		t.Fatalf("Engine.Close: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("/events ended with error after Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("/events stream hung after Engine.Close")
	}
}
