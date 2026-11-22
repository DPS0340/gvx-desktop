package recipes

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// ErrNoReloadWord reports a dockerd that logged nothing of a reload in
// dockerdReloadWait.
var ErrNoReloadWord = errors.New("dockerd said nothing of a reload")

// dockerdReloadWait is how long ReloadDockerd waits for dockerd to say how
// the reload went.
var dockerdReloadWait = 8 * time.Second

// ReloadDockerd has the engine's dockerd read daemon.json again: SIGHUP to
// the guest's first process (docker-init, which passes it on), then what
// dockerd logs of the reload, which is the only place it says so.
func ReloadDockerd(ctx context.Context, ctl sandbox.Control, logs *sandbox.LogBuffer) error {
	seq := logs.Seq()
	if err := ctl.Kill(ctx, rungv.KillOptions{Signal: "SIGHUP"}); err != nil {
		return err
	}
	deadline := time.Now().Add(dockerdReloadWait)
	for time.Now().Before(deadline) {
		for _, l := range logs.Since(seq) {
			seq = l.Seq
			if l.Stream == sandbox.System {
				continue
			}
			if ok, err := dockerdReloadResult(l.Text); ok || err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return ErrNoReloadWord
}

// dockerdReloadResult reads a line of dockerd's after a SIGHUP: ok when it
// applied the file, an error when it would not.
func dockerdReloadResult(line string) (ok bool, err error) {
	switch {
	case strings.Contains(line, "Reloaded configuration"):
		return true, nil
	case strings.Contains(line, "level=error") || strings.Contains(line, "level=fatal"):
		// time=… level=error msg="Error reloading configuration" error="unable to …"
		for _, key := range []string{"error", "msg"} {
			if v, ok := logfmtValue(line, key); ok {
				return false, errors.New(v)
			}
		}
		return false, errors.New(line)
	}
	return false, nil
}

// logfmtValue is the value of key in a logfmt line, as dockerd writes its
// log: key=word or key="quoted \"text\"".
func logfmtValue(line, key string) (string, bool) {
	j := strings.Index(" "+line, " "+key+"=")
	if j < 0 {
		return "", false
	}
	rest := line[j+len(key)+1:]
	if !strings.HasPrefix(rest, `"`) {
		v, _, _ := strings.Cut(rest, " ")
		return v, true
	}
	if q, err := strconv.QuotedPrefix(rest); err == nil {
		v, _ := strconv.Unquote(q)
		return v, true
	}
	return strings.Trim(rest, `"`), true
}
