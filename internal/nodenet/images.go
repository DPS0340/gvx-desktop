package nodenet

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// importArgv imports an image archive from standard input into the
// containerd namespace the kubelet runs pods from. k3s's ctr finds its
// containerd by itself.
var importArgv = []string{"ctr", "-n", "k8s.io", "images", "import", "-"}

// ImportImage puts the image archive r reads, as docker save writes it,
// into the node ctl runs, as kind load docker-image does: its pods run it
// without pulling it, while their pull policy lets them (IfNotPresent, the
// default for a tag other than latest).
func ImportImage(ctx context.Context, ctl sandbox.Control, r io.Reader) error {
	p, err := ctl.Exec(ctx, sandbox.ExecOptions{ExecOptions: rungv.ExecOptions{Argv: importArgv, Interactive: true}})
	if err != nil {
		return err
	}
	defer p.Close()
	var wg sync.WaitGroup
	var out bytes.Buffer
	var mu sync.Mutex
	for _, s := range []io.Reader{p.Stdout(), p.Stderr()} {
		if s == nil {
			continue
		}
		wg.Go(func() {
			b, _ := io.ReadAll(io.LimitReader(s, 64<<10))
			_, _ = io.Copy(io.Discard, s)
			mu.Lock()
			out.Write(b)
			mu.Unlock()
		})
	}
	_, copyErr := io.Copy(p.Stdin(), r)
	closeErr := p.Stdin().Close()
	wg.Wait()
	code, err := p.Wait()
	switch {
	case err != nil:
		return err
	case code != 0:
		return fmt.Errorf("ctr images import: %s", lastLine(out.String(), code))
	case copyErr != nil:
		return copyErr
	}
	return closeErr
}

// lastLine is the last line of what a command said, or its exit code.
func lastLine(s string, code int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if l := strings.TrimSpace(lines[len(lines)-1]); l != "" {
		return l
	}
	return fmt.Sprintf("exit code %d", code)
}
