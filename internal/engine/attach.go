package engine

// Owner: workstream A.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	streamContentTypeRaw = "application/vnd.docker.raw-stream"
	streamContentTypeMux = "application/vnd.docker.multiplexed-stream"
)

// ---- logs ----

func (e *Engine) containerLogs(w http.ResponseWriter, r *http.Request) {
	c, err := e.streamLookup(r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	wantStdout := queryBool(r, "stdout")
	wantStderr := queryBool(r, "stderr")
	if !wantStdout && !wantStderr {
		writeError(w, r, errInvalid("You must choose at least one stream"))
		return
	}
	follow := queryBool(r, "follow")
	timestamps := queryBool(r, "timestamps")

	tailN := -1
	if ts := strings.TrimSpace(r.URL.Query().Get("tail")); ts != "" && !strings.EqualFold(ts, "all") {
		if n, perr := strconv.Atoi(ts); perr == nil && n >= 0 {
			tailN = n
		}
	}
	since, err := logsParseTime(r.URL.Query().Get("since"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	until, err := logsParseTime(r.URL.Query().Get("until"))
	if err != nil {
		writeError(w, r, err)
		return
	}

	tty := c.Config.Tty
	wantEntry := func(ent logsEntry) bool {
		if !tty {
			if ent.Stream == "stdout" && !wantStdout {
				return false
			}
			if ent.Stream == "stderr" && !wantStderr {
				return false
			}
		}
		if !since.IsZero() && ent.Time.Before(since) {
			return false
		}
		if !until.IsZero() && ent.Time.After(until) {
			return false
		}
		return true
	}

	hist, sub, unsub := e.streamReadAndSubscribe(c.ID, follow)
	defer unsub()

	ct := streamContentTypeMux
	if tty {
		ct = streamContentTypeRaw
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	if fl != nil {
		fl.Flush()
	}

	atStart := map[string]bool{"stdout": true, "stderr": true}
	writeEntry := func(ent logsEntry) error {
		key := ent.Stream
		if tty {
			key = "stdout"
		}
		payload := []byte(ent.Log)
		if timestamps && atStart[key] {
			prefix := ent.Time.UTC().Format(time.RFC3339Nano) + " "
			buf := make([]byte, len(prefix)+len(payload))
			copy(buf, prefix)
			copy(buf[len(prefix):], payload)
			payload = buf
		}
		atStart[key] = strings.HasSuffix(ent.Log, "\n")
		if tty {
			_, err := w.Write(payload)
			return err
		}
		var stream byte = 1
		if ent.Stream == "stderr" {
			stream = 2
		}
		return streamWriteMuxFrame(w, stream, payload)
	}

	filtered := make([]logsEntry, 0, len(hist))
	for _, ent := range hist {
		if wantEntry(ent) {
			filtered = append(filtered, ent)
		}
	}
	for _, ent := range logsTailEntries(filtered, tailN) {
		if err := writeEntry(ent); err != nil {
			return
		}
	}
	if fl != nil {
		fl.Flush()
	}
	if !follow {
		return
	}
	if !until.IsZero() && !time.Now().Before(until) {
		return
	}

	// If the container is not active now, drain any buffered entries and finish.
	cur, err := e.streamLookup(c.ID)
	if err != nil || (cur.State.Status != StatusRunning && cur.State.Status != StatusRestarting) {
		for {
			select {
			case ent, ok := <-sub.ch:
				if !ok {
					return
				}
				if wantEntry(ent) {
					if err := writeEntry(ent); err != nil {
						return
					}
					if fl != nil {
						fl.Flush()
					}
				}
			default:
				return
			}
		}
	}

	var untilCh <-chan time.Time
	if !until.IsZero() {
		timer := time.NewTimer(time.Until(until))
		defer timer.Stop()
		untilCh = timer.C
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-untilCh:
			return
		case ent, ok := <-sub.ch:
			if !ok {
				if !e.isOpen() {
					return
				}
				cur, err := e.streamLookup(c.ID)
				if err != nil || cur.State.Status != StatusRestarting {
					return
				}
				// Re-subscribe before waiting for the restarted run so no output is missed.
				unsub()
				_, sub, unsub = e.streamReadAndSubscribe(c.ID, true)
				if sub == nil {
					return
				}
				for {
					ch := e.changes()
					cur, err = e.streamLookup(c.ID)
					if err != nil || cur.State.Status == StatusExited || cur.State.Status == StatusDead {
						return
					}
					if cur.State.Status == StatusRunning {
						break
					}
					select {
					case <-r.Context().Done():
						return
					case <-untilCh:
						return
					case <-ch:
					}
				}
				continue
			}
			if !until.IsZero() && ent.Time.After(until) {
				return
			}
			if !wantEntry(ent) {
				continue
			}
			if err := writeEntry(ent); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
	}
}

// ---- attach ----

func (e *Engine) containerAttach(w http.ResponseWriter, r *http.Request) {
	c, err := e.streamLookup(r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	wantStream := queryBool(r, "stream")
	wantLogs := queryBool(r, "logs")
	wantStdin := queryBool(r, "stdin")
	wantStdout := queryBool(r, "stdout")
	wantStderr := queryBool(r, "stderr")

	var detachSeq []byte
	if c.Config.Tty {
		rawDetach := r.URL.Query().Get("detachKeys")
		if rawDetach == "" {
			detachSeq = []byte{16, 17} // ctrl-p, ctrl-q
		} else {
			detachSeq, err = attachParseDetachKeys(rawDetach)
			if err != nil {
				writeError(w, r, err)
				return
			}
		}
	}

	// Subscribe before writing response headers so an attach-before-start
	// (docker run) cannot miss the start of the container's output.
	var (
		hist  []logsEntry
		sub   *streamSub
		unsub = func() {}
	)
	if wantLogs || wantStream {
		hist, sub, unsub = e.streamReadAndSubscribe(c.ID, wantStream)
	}
	defer unsub()

	rc := http.NewResponseController(w)
	conn, rw, err := rc.Hijack()
	if err != nil {
		writeError(w, r, fmt.Errorf("hijack failed: %w", err))
		return
	}
	defer conn.Close()

	tty := c.Config.Tty
	multiplex := !tty
	ct := streamContentTypeRaw
	if multiplex && versionAtLeast(requestVersion(r), "1.42") {
		ct = streamContentTypeMux
	}
	upgraded := strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "tcp")
	var head string
	if upgraded {
		head = "HTTP/1.1 101 UPGRADED\r\nContent-Type: " + ct + "\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"
	} else {
		head = "HTTP/1.1 200 OK\r\nContent-Type: " + ct + "\r\n\r\n"
	}
	if _, err := conn.Write([]byte(head)); err != nil {
		return
	}

	writeOut := func(ent logsEntry) error {
		if tty {
			if !wantStdout && !wantStderr {
				return nil
			}
			_, err := conn.Write([]byte(ent.Log))
			return err
		}
		if ent.Stream == "stdout" {
			if !wantStdout {
				return nil
			}
			return streamWriteMuxFrame(conn, 1, []byte(ent.Log))
		}
		if !wantStderr {
			return nil
		}
		return streamWriteMuxFrame(conn, 2, []byte(ent.Log))
	}

	if wantLogs {
		for _, ent := range hist {
			if err := writeOut(ent); err != nil {
				return
			}
		}
	}
	if !wantStream || sub == nil {
		attachCloseWriteConn(conn)
		return
	}

	done := make(chan struct{})
	var closeDone sync.Once
	stop := func() {
		closeDone.Do(func() {
			close(done)
			// Unblock any pending read on conn.
			_ = conn.SetReadDeadline(time.Now())
		})
	}

	cs := e.streamGetOrCreate(c.ID)

	var wg sync.WaitGroup
	// Output pump: container -> client.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			attachCloseWriteConn(conn)
			stop()
		}()
		for {
			select {
			case <-done:
				return
			case ent, ok := <-sub.ch:
				if !ok {
					return
				}
				if err := writeOut(ent); err != nil {
					return
				}
			}
		}
	}()

	// Input pump / peer-close watcher: only copy stdin when requested AND the
	// container has OpenStdin (otherwise a docker attach to a `run -d`
	// container would block forever waiting for a stdin writer). Either way,
	// detect peer hangup so a disconnected client on a silent container never
	// leaks the connection, streamSub, or goroutines.
	if wantStdin && c.Config.OpenStdin {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.attachCopyStdin(c, cs, conn, rw.Reader, detachSeq, done, stop)
		}()
	} else {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if attachWaitPeerClose(conn, done) {
				stop()
			}
		}()
	}

	wg.Wait()
}

func (e *Engine) attachCopyStdin(c Container, cs *streamContainer, conn net.Conn, br *bufio.Reader, detachSeq []byte, done <-chan struct{}, stop func()) {
	var (
		stdinW  io.Writer
		matched int
		buf     = make([]byte, 32*1024)
	)
	ensureStdin := func() io.Writer {
		if stdinW != nil {
			return stdinW
		}
		stdinW = e.attachWaitForStdin(c.ID, cs, done)
		return stdinW
	}
	writeData := func(p []byte) bool {
		if len(p) == 0 {
			return true
		}
		w := ensureStdin()
		if w == nil {
			return false
		}
		if _, err := w.Write(p); err != nil {
			return false
		}
		return true
	}

	for {
		n, err := br.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if len(detachSeq) > 0 {
				var out []byte
				for _, b := range chunk {
					if b == detachSeq[matched] {
						matched++
						if matched == len(detachSeq) {
							// Detach cleanly without stopping or closing stdin.
							if len(out) > 0 {
								_ = writeData(out)
							}
							stop()
							return
						}
						continue
					}
					if matched > 0 {
						out = append(out, detachSeq[:matched]...)
						matched = 0
						if b == detachSeq[0] {
							matched = 1
							continue
						}
					}
					out = append(out, b)
				}
				if !writeData(out) {
					return
				}
			} else if !writeData(chunk) {
				return
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if matched > 0 {
					_ = writeData(detachSeq[:matched])
				}
				if c.Config.StdinOnce {
					// If EOF arrived before the container started, wait for the run
					// so CloseStdin reaches the guest.
					if w := ensureStdin(); w != nil {
						_ = e.runner.CloseStdin(c.ID)
					}
				}
				// Client closed its write side; keep output flowing if it only
				// half-closed, or stop promptly once the peer closes its read end.
				if attachWaitPeerClose(conn, done) {
					stop()
				}
				return
			}
			stop()
			return
		}
	}
}

func (e *Engine) attachWaitForStdin(id string, cs *streamContainer, done <-chan struct{}) io.Writer {
	for {
		var stateCh <-chan struct{}
		if cs != nil {
			stateCh = cs.stateChanges()
		}
		changeCh := e.changes()
		if w := e.runner.Stdin(id); w != nil {
			return w
		}
		if !e.isOpen() {
			return nil
		}
		if _, err := e.streamLookup(id); err != nil {
			return nil
		}
		select {
		case <-done:
			return nil
		case <-stateCh:
		case <-changeCh:
		}
	}
}

func attachCloseWriteConn(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// attachParseDetachKeys parses a comma-separated Docker detach key sequence
// (such as "ctrl-p,ctrl-q") into its raw bytes.
func attachParseDetachKeys(raw string) ([]byte, error) {
	parts := strings.Split(raw, ",")
	out := make([]byte, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, errInvalid("invalid detach keys: %q", raw)
		}
		if len(p) == 1 {
			out = append(out, p[0])
			continue
		}
		lower := strings.ToLower(p)
		rest, ok := strings.CutPrefix(lower, "ctrl-")
		if !ok || len(rest) != 1 {
			return nil, errInvalid("invalid detach key: %q", p)
		}
		b := rest[0]
		switch {
		case b >= 'a' && b <= 'z':
			out = append(out, b-'a'+1)
		case b == '@':
			out = append(out, 0)
		case b == '[':
			out = append(out, 27)
		case b == '\\':
			out = append(out, 28)
		case b == ']':
			out = append(out, 29)
		case b == '^':
			out = append(out, 30)
		case b == '_':
			out = append(out, 31)
		default:
			return nil, errInvalid("invalid detach key: %q", p)
		}
	}
	return out, nil
}

// ---- wait ----

type streamWaitResponse struct {
	StatusCode int              `json:"StatusCode"`
	Error      *streamWaitError `json:"Error"`
}

type streamWaitError struct {
	Message string `json:"Message"`
}

func (e *Engine) containerWait(w http.ResponseWriter, r *http.Request) {
	cond := strings.TrimSpace(r.URL.Query().Get("condition"))
	if cond == "" {
		cond = "not-running"
	}
	switch cond {
	case "not-running", "next-exit", "removed":
	default:
		writeError(w, r, errInvalid("invalid condition: %q", cond))
		return
	}

	c, err := e.streamLookup(r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	id := c.ID
	startExits := c.State.Exits
	startStreamRec, _ := e.streamLastExit(id)
	lastCode := c.State.ExitCode
	lastErr := c.State.Error
	seenExit := false
	autoRemove := c.HostConfig.AutoRemove

	// Flush the 200 header immediately: the docker CLI calls wait before start
	// and waits for the response header before starting the container.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	if fl != nil {
		fl.Flush()
	}

	respond := func(code int, msg string) {
		resp := streamWaitResponse{StatusCode: code}
		if msg != "" {
			resp.Error = &streamWaitError{Message: msg}
		}
		var buf bytes.Buffer
		_ = json.NewEncoder(&buf).Encode(resp)
		_, _ = w.Write(buf.Bytes())
		if fl != nil {
			fl.Flush()
		}
	}

	resolveRemovedCode := func() int {
		if seenExit {
			return lastCode
		}
		if rec, ok := e.streamLastExit(id); ok && rec.exits > startStreamRec.exits {
			return rec.code
		}
		if !autoRemove {
			return lastCode
		}
		// AutoRemove deletes the record inside containerEvent right before
		// streamsEvent records the exit code on the same goroutine; wait
		// briefly for streamsEvent to land if it has not yet.
		deadline := time.NewTimer(100 * time.Millisecond)
		defer deadline.Stop()
		for {
			ch := e.changes()
			if rec, ok := e.streamLastExit(id); ok && rec.exits > startStreamRec.exits {
				return rec.code
			}
			select {
			case <-r.Context().Done():
				return lastCode
			case <-deadline.C:
				return lastCode
			case <-ch:
			}
		}
	}

	for {
		ch := e.changes()
		cur, err := e.streamLookup(id)
		if err != nil {
			// Container was removed.
			respond(resolveRemovedCode(), lastErr)
			return
		}
		lastCode = cur.State.ExitCode
		lastErr = cur.State.Error
		if cur.State.Exits > startExits {
			seenExit = true
		}

		switch cond {
		case "not-running":
			if !cur.State.Running() {
				respond(lastCode, lastErr)
				return
			}
		case "next-exit":
			if cur.State.Exits > startExits {
				respond(lastCode, lastErr)
				return
			}
		case "removed":
			// Wait until streamLookup returns 404.
		}

		if !e.isOpen() {
			respond(lastCode, lastErr)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ch:
		}
	}
}

// ---- resize ----

func (e *Engine) containerResize(w http.ResponseWriter, r *http.Request) {
	c, err := e.streamLookup(r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	h, err := queryInt(r, "h", 0)
	if err != nil || h < 0 || h > 65535 {
		writeError(w, r, errInvalid("invalid height"))
		return
	}
	width, err := queryInt(r, "w", 0)
	if err != nil || width < 0 || width > 65535 {
		writeError(w, r, errInvalid("invalid width"))
		return
	}
	if !c.State.Running() {
		writeError(w, r, errConflict("Container %s is not running", c.ID))
		return
	}
	if !c.Config.Tty {
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := e.runner.Resize(c.ID, uint16(h), uint16(width)); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}
