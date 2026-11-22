package engine

// Owner: workstream A (supervisor stdio, attach, logs, wait).

import (
	"context"
	"io"
	"syscall"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

func (r *rungvRunner) Start(spec sandbox.Spec) error { return r.sup.Start(spec) }

func (r *rungvRunner) Stop(id string, timeout time.Duration) error { return r.sup.Stop(id, timeout) }

func (r *rungvRunner) Signal(id string, sig syscall.Signal) error {
	if ctl, ok := r.sup.Control(id); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := ctl.Kill(ctx, rungv.KillOptions{Signal: signalName(sig)})
		if err == nil {
			return nil
		}
		if !isControlUnreachable(err) {
			return err
		}
	}
	return r.sup.Signal(id, sig)
}

func (r *rungvRunner) State(id string) sandbox.State { return r.sup.State(id) }

func (r *rungvRunner) Stdin(id string) io.Writer { return r.sup.Stdin(id) }

func (r *rungvRunner) CloseStdin(id string) error { return r.sup.CloseStdin(id) }

func (r *rungvRunner) Resize(id string, rows, cols uint16) error {
	return r.sup.Resize(id, rows, cols)
}

func (r *rungvRunner) Forget(id string) { r.sup.Forget(id) }

// ---- rungv v0.20 (W2; docs/design/gvx-v020.md §5) ----

func (r *rungvRunner) Features() rungv.Features {
	return r.client().Features(context.Background())
}

func (r *rungvRunner) Control(id string) sandbox.Control {
	if c, ok := r.sup.Control(id); ok {
		return c
	}
	return nil
}

func (r *rungvRunner) Pause(ctx context.Context, id string) error { return r.sup.Pause(ctx, id) }

func (r *rungvRunner) Unpause(ctx context.Context, id string) error { return r.sup.Unpause(ctx, id) }

func (r *rungvRunner) Update(ctx context.Context, id string, u rungv.Update) (rungv.Resources, error) {
	return r.sup.Update(ctx, id, u)
}

func (r *rungvRunner) Shutdown(id string, signal string, timeout time.Duration) error {
	return r.sup.Shutdown(id, signal, timeout)
}
