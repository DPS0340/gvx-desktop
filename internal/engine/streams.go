package engine

// Owner: workstream A (supervisor stdio, attach, logs, wait, stats).
// Workstream A may add files named stream_*.go, attach_*.go and logs_*.go.

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

const streamSubBuffer = 256

// streamExitRecord remembers the last observed exit code of a container so
// wait?condition=removed reports the right exit code even if AutoRemove
// deletes the container record before the waiter wakes.
type streamExitRecord struct {
	code  int
	exits int
}

// streamState is what the engine keeps of the containers' output: the
// logs of each, and the clients attached to it.
type streamState struct {
	mu       sync.Mutex
	byID     map[string]*streamContainer
	lastExit map[string]streamExitRecord
	closed   bool

	// lookupOverride is a test-only seam override so workstream A unit tests
	// can exercise handlers without depending on workstream B's store.
	lookupOverride func(ref string) (Container, error)
}

type streamSub struct {
	ch chan logsEntry
}

// streamContainer holds one container's log ring, persisted file handle, and
// live subscribers. Lock order: fileMu -> mu; never hold e.str.mu while
// acquiring fileMu or mu.
type streamContainer struct {
	id string

	fileMu     sync.Mutex
	file       *os.File
	fileBytes  int64
	fileLoaded bool
	dropped    bool

	mu      sync.Mutex
	entries []logsEntry
	bytes   int64
	subs    map[*streamSub]struct{}
	running bool
	stateCh chan struct{}
}

func streamNewContainer(id string) *streamContainer {
	return &streamContainer{
		id:      id,
		subs:    map[*streamSub]struct{}{},
		stateCh: make(chan struct{}),
	}
}

func (e *Engine) streamLookup(ref string) (Container, error) {
	if e.str.lookupOverride != nil {
		return e.str.lookupOverride(ref)
	}
	return e.lookup(ref)
}

func (e *Engine) streamGetOrCreate(id string) *streamContainer {
	e.str.mu.Lock()
	defer e.str.mu.Unlock()
	if e.str.closed {
		return nil
	}
	if e.str.byID == nil {
		e.str.byID = map[string]*streamContainer{}
	}
	cs := e.str.byID[id]
	if cs == nil {
		cs = streamNewContainer(id)
		e.str.byID[id] = cs
	}
	return cs
}

func (e *Engine) streamGet(id string) *streamContainer {
	e.str.mu.Lock()
	defer e.str.mu.Unlock()
	return e.str.byID[id]
}

func (e *Engine) logsFilePath(id string) string {
	if e.cfg.Dir == "" {
		return ""
	}
	return e.path("logs", id+".log")
}

// logsEnsureLoadedLocked loads logs/<id>.log once. Caller must hold cs.fileMu.
func (e *Engine) logsEnsureLoadedLocked(cs *streamContainer) {
	if cs.fileLoaded || cs.dropped {
		return
	}
	cs.fileLoaded = true
	p := e.logsFilePath(cs.id)
	if p == "" {
		return
	}
	disk, size, err := logsReadFile(p)
	if err != nil || len(disk) == 0 {
		cs.fileBytes = size
		return
	}
	cs.fileBytes = size
	cs.mu.Lock()
	var total int64
	for _, ent := range disk {
		total += ent.approxBytes()
	}
	cs.entries = append(disk, cs.entries...)
	cs.bytes += total
	cs.trimLocked()
	cs.mu.Unlock()
}

// trimLocked drops the oldest half of entries when the in-memory ring
// exceeds logsMaxFileBytes. Caller must hold cs.mu.
func (cs *streamContainer) trimLocked() {
	if cs.bytes <= logsMaxFileBytes || len(cs.entries) < 2 {
		return
	}
	drop := len(cs.entries) / 2
	cs.entries = append([]logsEntry(nil), cs.entries[drop:]...)
	var total int64
	for _, ent := range cs.entries {
		total += ent.approxBytes()
	}
	cs.bytes = total
}

// logsAppendToDiskLocked appends entries to logs/<id>.log, compacting when the
// file exceeds logsMaxFileBytes. Caller must hold cs.fileMu.
func (e *Engine) logsAppendToDiskLocked(cs *streamContainer, entries []logsEntry) {
	if cs.dropped {
		return
	}
	p := e.logsFilePath(cs.id)
	if p == "" {
		return
	}
	if cs.file == nil {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return
		}
		if st, err := f.Stat(); err == nil {
			cs.fileBytes = st.Size()
		}
		cs.file = f
	}
	for _, ent := range entries {
		line, err := logsEncodeEntry(ent)
		if err != nil {
			continue
		}
		n, err := cs.file.Write(line)
		cs.fileBytes += int64(n)
		if err != nil {
			break
		}
	}
	if cs.fileBytes > logsMaxFileBytes {
		_ = cs.file.Close()
		cs.file = nil
		if sz, err := logsCompactFile(p); err == nil {
			cs.fileBytes = sz
		}
	}
}

func (cs *streamContainer) stateChanges() <-chan struct{} {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.stateCh
}

func (cs *streamContainer) signalStateChangeLocked() {
	close(cs.stateCh)
	cs.stateCh = make(chan struct{})
}

func (cs *streamContainer) closeAllSubsLocked() {
	for sub := range cs.subs {
		delete(cs.subs, sub)
		close(sub.ch)
	}
}

// streamReadAndSubscribe returns a snapshot of the container's log history
// and, when subscribe is true, a live subscriber registered atomically under
// the same lock so no output is lost or duplicated between history and live.
func (e *Engine) streamReadAndSubscribe(id string, subscribe bool) (hist []logsEntry, sub *streamSub, unsub func()) {
	noop := func() {}
	cs := e.streamGetOrCreate(id)
	if cs == nil {
		return nil, nil, noop
	}
	cs.fileMu.Lock()
	e.logsEnsureLoadedLocked(cs)
	cs.fileMu.Unlock()

	cs.mu.Lock()
	if len(cs.entries) > 0 {
		hist = append([]logsEntry(nil), cs.entries...)
	}
	if subscribe {
		sub = &streamSub{ch: make(chan logsEntry, streamSubBuffer)}
		cs.subs[sub] = struct{}{}
	}
	cs.mu.Unlock()

	if sub == nil {
		return hist, nil, noop
	}
	return hist, sub, func() {
		cs.mu.Lock()
		delete(cs.subs, sub)
		cs.mu.Unlock()
	}
}

func (e *Engine) streamLastExit(id string) (streamExitRecord, bool) {
	e.str.mu.Lock()
	defer e.str.mu.Unlock()
	rec, ok := e.str.lastExit[id]
	return rec, ok
}

// ---- seams: called by engine.go and the other workstreams; frozen ----

// onRunnerOutput takes what a container's guest wrote: it goes to the
// container's log and to the clients attached to it. It must not block.
func (e *Engine) onRunnerOutput(id string, o sandbox.Output) {
	if len(o.Data) == 0 {
		return
	}
	stream := "stdout"
	if o.Stream == sandbox.Stderr {
		stream = "stderr"
	}
	entries := logsSplitEntries(stream, o.Data, time.Now().UTC())
	if len(entries) == 0 {
		return
	}
	cs := e.streamGetOrCreate(id)
	if cs == nil {
		return
	}
	cs.fileMu.Lock()
	e.logsEnsureLoadedLocked(cs)
	e.logsAppendToDiskLocked(cs, entries)
	cs.mu.Lock()
	for _, ent := range entries {
		cs.entries = append(cs.entries, ent)
		cs.bytes += ent.approxBytes()
		for sub := range cs.subs {
			select {
			case sub.ch <- ent:
			default:
				// Drop slow subscriber so the supervisor pump never blocks.
				delete(cs.subs, sub)
				close(sub.ch)
			}
		}
	}
	cs.trimLocked()
	cs.mu.Unlock()
	cs.fileMu.Unlock()
}

// streamsEvent takes the runner's events after the containers did: an
// ended run ends the streams of its attached clients.
func (e *Engine) streamsEvent(ev sandbox.Event) {
	switch ev.Kind {
	case sandbox.EventChanged:
		if ev.State.Status.Active() {
			if cs := e.streamGet(ev.ID); cs != nil {
				cs.mu.Lock()
				if !cs.running {
					cs.running = true
					cs.signalStateChangeLocked()
				}
				cs.mu.Unlock()
			}
		}
	case sandbox.EventExited:
		code := ev.State.ExitCode
		if code == -1 && ev.State.Signal != 0 {
			code = 128 + ev.State.Signal
		}
		e.str.mu.Lock()
		if e.str.lastExit == nil {
			e.str.lastExit = map[string]streamExitRecord{}
		}
		rec := e.str.lastExit[ev.ID]
		rec.code = code
		rec.exits++
		e.str.lastExit[ev.ID] = rec
		if len(e.str.lastExit) > 512 {
			for k := range e.str.lastExit {
				if k != ev.ID {
					delete(e.str.lastExit, k)
					break
				}
			}
		}
		cs := e.str.byID[ev.ID]
		e.str.mu.Unlock()

		if cs != nil {
			cs.mu.Lock()
			cs.running = false
			cs.closeAllSubsLocked()
			cs.signalStateChangeLocked()
			cs.mu.Unlock()

			cs.fileMu.Lock()
			if cs.file != nil {
				_ = cs.file.Close()
				cs.file = nil
			}
			cs.fileMu.Unlock()
		}
		// Wake waiters (for example wait?condition=removed after AutoRemove)
		// now that the final exit code is recorded.
		e.notify()
	}
}

// dropLogs deletes the logs of a removed container.
func (e *Engine) dropLogs(id string) {
	e.str.mu.Lock()
	cs := e.str.byID[id]
	delete(e.str.byID, id)
	e.str.mu.Unlock()

	if cs != nil {
		cs.mu.Lock()
		cs.closeAllSubsLocked()
		cs.signalStateChangeLocked()
		cs.mu.Unlock()

		cs.fileMu.Lock()
		cs.dropped = true
		if cs.file != nil {
			_ = cs.file.Close()
			cs.file = nil
		}
		cs.fileMu.Unlock()
	}
	if p := e.logsFilePath(id); p != "" {
		_ = os.Remove(p)
	}
}

// openStreams resets the closed state at Open so a reopened Engine captures
// output, serves logs, and accepts attach streams again.
func (e *Engine) openStreams() {
	e.str.mu.Lock()
	e.str.closed = false
	e.str.mu.Unlock()
}

// closeStreams ends every attached client's stream, at Close.
func (e *Engine) closeStreams() {
	e.str.mu.Lock()
	e.str.closed = true
	streams := make([]*streamContainer, 0, len(e.str.byID))
	for _, cs := range e.str.byID {
		streams = append(streams, cs)
	}
	clear(e.str.byID)
	e.str.mu.Unlock()

	for _, cs := range streams {
		cs.mu.Lock()
		cs.closeAllSubsLocked()
		cs.signalStateChangeLocked()
		cs.mu.Unlock()

		cs.fileMu.Lock()
		if cs.file != nil {
			_ = cs.file.Close()
			cs.file = nil
		}
		cs.fileMu.Unlock()
	}
}
