package sandbox

import (
	"context"
	"strings"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// eventRetryDelays bounds how long watchEvents waits for a freshly started
// sandbox's control socket to become reachable before giving up quietly.
var eventRetryDelays = []time.Duration{
	100 * time.Millisecond,
	200 * time.Millisecond,
	400 * time.Millisecond,
	800 * time.Millisecond,
	time.Second,
	time.Second,
}

func (s *Supervisor) watchEvents(ctx context.Context, id string, p *proc) {
	target := rungv.Target{Socket: p.paths.ControlSocket, TokenFile: p.paths.TokenFile}
	if !target.Valid() {
		return
	}
	client := s.clientFor(p)
	if client == nil || client.Path == "" {
		return
	}

	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return
		}
		received := false
		err := client.Events(ctx, target, func(ev rungv.Event) {
			received = true
			s.handleRuntimeEvent(id, p, ev)
		})
		if err == nil || ctx.Err() != nil || received || !rungv.IsUnreachable(err) {
			return
		}
		if attempt >= len(eventRetryDelays) {
			return
		}
		s.mu.Lock()
		active := s.procs[id] == p && p.state.Status.Active() && !p.stopping
		s.mu.Unlock()
		if !active {
			return
		}
		timer := time.NewTimer(eventRetryDelays[attempt])
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *Supervisor) handleRuntimeEvent(id string, p *proc, ev rungv.Event) {
	evType := strings.ToLower(strings.TrimSpace(ev.Type))
	action := strings.ToLower(strings.TrimSpace(ev.Action))

	switch evType {
	case "port":
		switch action {
		case "add", "added", "remove", "removed":
			s.mu.Lock()
			if s.procs[id] == p && p.state.Status.Active() {
				s.syncPortsLocked(id, p)
			}
			s.mu.Unlock()
		}

	case "lifecycle":
		var wantPaused bool
		switch action {
		case "pause", "paused":
			wantPaused = true
		case "unpause", "unpaused":
			wantPaused = false
		default:
			return
		}
		s.mu.Lock()
		changed := false
		if s.procs[id] == p && p.state.Status.Active() && p.state.Paused != wantPaused {
			p.state.Paused = wantPaused
			changed = true
		}
		s.mu.Unlock()
		if changed {
			s.notify(Event{ID: id, Kind: EventChanged, State: s.State(id)})
		}

	case "oom":
		s.mu.Lock()
		active := s.procs[id] == p && p.state.Status.Active()
		if active {
			p.state.Advice = "The guest hit its memory limit (OOM). Raise the memory limit in the sandbox settings."
		}
		s.mu.Unlock()
		if active {
			s.Logs(id).Add(System, "── Guest hit memory limit (OOM)")
			s.notify(Event{ID: id, Kind: EventChanged, State: s.State(id)})
		}
	}
}
