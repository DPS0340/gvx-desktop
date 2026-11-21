package engine

// Owner: workstream A (supervisor stdio, attach, logs, wait).

import (
	"io"
	"syscall"
	"time"

	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

func (r *rungvRunner) Start(spec sandbox.Spec) error { return r.sup.Start(spec) }

func (r *rungvRunner) Stop(id string, timeout time.Duration) error { return r.sup.Stop(id, timeout) }

func (r *rungvRunner) Signal(id string, sig syscall.Signal) error { return r.sup.Signal(id, sig) }

func (r *rungvRunner) State(id string) sandbox.State { return r.sup.State(id) }

func (r *rungvRunner) Stdin(id string) io.Writer { return r.sup.Stdin(id) }

func (r *rungvRunner) CloseStdin(id string) error { return r.sup.CloseStdin(id) }

func (r *rungvRunner) Resize(id string, rows, cols uint16) error {
	return r.sup.Resize(id, rows, cols)
}

func (r *rungvRunner) Forget(id string) { r.sup.Forget(id) }
