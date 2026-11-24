package engine

// Owner: workstream C.

import (
	"encoding/json"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxEvents = 1000

// eventState is the engine's recent events and the clients of /events.
type eventState struct {
	mu   sync.Mutex
	ring []Event
	subs map[*eventSub]struct{}
}

type eventSub struct {
	ch chan Event
}

// Event is one event of /events, as docker writes them.
type Event struct {
	Type   string // container, image, volume, network
	Action string // create, start, die, destroy, pull, delete, ...
	ID     string // the container's or volume's ID, the image's reference
	// Attributes are the actor's: name, image, exitCode, and a container's
	// labels.
	Attributes map[string]string
	Time       time.Time // now when zero
}

type eventMessage struct {
	Status   string     `json:"status,omitempty"`
	ID       string     `json:"id,omitempty"`
	From     string     `json:"from,omitempty"`
	Type     string     `json:"Type"`
	Action   string     `json:"Action"`
	Actor    eventActor `json:"Actor"`
	Scope    string     `json:"scope"`
	Time     int64      `json:"time"`
	TimeNano int64      `json:"timeNano"`
}

type eventActor struct {
	ID         string            `json:"ID"`
	Attributes map[string]string `json:"Attributes"`
}

// ---- seams: called by every workstream; frozen ----

// emit records an event and sends it to the clients of /events. It does
// not block.
func (e *Engine) emit(ev Event) {
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	if ev.Attributes != nil {
		ev.Attributes = maps.Clone(ev.Attributes)
	}
	e.evt.mu.Lock()
	e.evt.ring = append(e.evt.ring, ev)
	if len(e.evt.ring) > maxEvents {
		copy(e.evt.ring, e.evt.ring[len(e.evt.ring)-maxEvents:])
		e.evt.ring = e.evt.ring[:maxEvents]
	}
	for sub := range e.evt.subs {
		select {
		case sub.ch <- ev:
		default:
			delete(e.evt.subs, sub)
			close(sub.ch)
		}
	}
	e.evt.mu.Unlock()
}

// ---- handlers ----

func (e *Engine) events(w http.ResponseWriter, r *http.Request) {
	f, err := queryFilters(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	now := time.Now().UTC()
	since, err := parseEventTime(r.URL.Query().Get("since"), now)
	if err != nil {
		writeError(w, r, err)
		return
	}
	until, err := parseEventTime(r.URL.Query().Get("until"), now)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !since.IsZero() && !until.IsZero() && until.Before(since) {
		writeError(w, r, errInvalid("until timestamp cannot be before since timestamp"))
		return
	}

	var replay []Event
	var sub *eventSub
	live := until.IsZero() || until.After(now)

	e.evt.mu.Lock()
	if !since.IsZero() {
		for _, ev := range e.evt.ring {
			if ev.Time.Before(since) {
				continue
			}
			if !until.IsZero() && ev.Time.After(until) {
				continue
			}
			if matchEvent(ev, f) {
				replay = append(replay, ev)
			}
		}
	}
	if live {
		sub = &eventSub{ch: make(chan Event, 128)}
		if e.evt.subs == nil {
			e.evt.subs = map[*eventSub]struct{}{}
		}
		e.evt.subs[sub] = struct{}{}
	}
	e.evt.mu.Unlock()

	if sub != nil {
		defer func() {
			e.evt.mu.Lock()
			if _, ok := e.evt.subs[sub]; ok {
				delete(e.evt.subs, sub)
				close(sub.ch)
			}
			e.evt.mu.Unlock()
		}()
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()

	writeEv := func(ev Event) bool {
		b, err := json.Marshal(toEventMessage(ev))
		if err != nil {
			return false
		}
		b = append(b, '\n')
		if _, err := w.Write(b); err != nil {
			return false
		}
		_ = rc.Flush()
		return true
	}

	for _, ev := range replay {
		if !writeEv(ev) {
			return
		}
	}
	if !live {
		return
	}

	var untilCh <-chan time.Time
	if !until.IsZero() {
		d := time.Until(until)
		if d <= 0 {
			return
		}
		timer := time.NewTimer(d)
		defer timer.Stop()
		untilCh = timer.C
	}

	ctx := r.Context()
	for {
		ch := e.changes()
		if !e.isOpen() {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-untilCh:
			return
		case <-ch:
			if !e.isOpen() {
				return
			}
		case ev, ok := <-sub.ch:
			if !ok {
				return
			}
			if !until.IsZero() && ev.Time.After(until) {
				return
			}
			if !matchEvent(ev, f) {
				continue
			}
			if !writeEv(ev) {
				return
			}
		}
	}
}

func toEventMessage(ev Event) eventMessage {
	attrs := maps.Clone(ev.Attributes)
	if attrs == nil {
		attrs = map[string]string{}
	}
	msg := eventMessage{
		Type:     ev.Type,
		Action:   ev.Action,
		Actor:    eventActor{ID: ev.ID, Attributes: attrs},
		Scope:    "local",
		Time:     ev.Time.Unix(),
		TimeNano: ev.Time.UnixNano(),
	}
	if ev.Type == "container" || ev.Type == "image" {
		msg.Status = ev.Action
		msg.ID = ev.ID
		if img := attrs["image"]; img != "" {
			msg.From = img
		}
	}
	return msg
}

func matchEvent(ev Event, f filters) bool {
	if len(f) == 0 {
		return true
	}
	if !f.matchAny("type", func(v string) bool { return ev.Type == v }) {
		return false
	}
	if !f.matchAny("event", func(v string) bool {
		return ev.Action == v || strings.HasPrefix(ev.Action, v+":")
	}) {
		return false
	}
	if !f.matchAny("container", func(v string) bool {
		if ev.Type == "container" {
			return ev.ID == v || strings.HasPrefix(ev.ID, v) || ev.Attributes["name"] == v
		}
		if cid := ev.Attributes["container"]; cid != "" {
			return cid == v || strings.HasPrefix(cid, v)
		}
		return false
	}) {
		return false
	}
	if !f.matchAny("image", func(v string) bool {
		if ev.Type == "image" {
			return ev.ID == v || strings.HasPrefix(ev.ID, v) || ev.Attributes["name"] == v
		}
		return ev.Attributes["image"] == v
	}) {
		return false
	}
	if !f.matchAny("volume", func(v string) bool {
		return ev.Type == "volume" && (ev.ID == v || ev.Attributes["name"] == v)
	}) {
		return false
	}
	if !f.matchAny("network", func(v string) bool {
		return ev.Type == "network" && (ev.ID == v || strings.HasPrefix(ev.ID, v) || ev.Attributes["name"] == v)
	}) {
		return false
	}
	if !f.matchLabels(ev.Attributes) {
		return false
	}
	return true
}

// parseEventTime parses a Docker since/until query parameter: RFC3339, unix
// seconds (int or float), or a Go duration relative to now (as "10m").
func parseEventTime(raw string, now time.Time) (time.Time, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, nil
	}
	if secs, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(secs, 0).UTC(), nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		sec := int64(f)
		nsec := int64((f - float64(sec)) * 1e9)
		return time.Unix(sec, nsec).UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	return time.Time{}, errInvalid("invalid timestamp: %q", raw)
}
