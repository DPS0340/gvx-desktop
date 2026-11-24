package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/gvx-desktop/internal/imagefs"
	"github.com/ironpark/gvx-desktop/internal/overlay/guestcfg"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

const (
	defaultHealthInterval      = 30 * time.Second
	defaultHealthTimeout       = 30 * time.Second
	defaultHealthStartInterval = 5 * time.Second
	defaultHealthRetries       = 3
	maxHealthLogEntries        = 5
	maxHealthOutputBytes       = 4096
)

type healthState struct {
	mu     sync.Mutex
	byID   map[string]*healthMonitor
	closed bool
}

type healthMonitor struct {
	gen    uint64
	cancel context.CancelFunc
	done   chan struct{}
}

func (hc *HealthConfig) enabled() bool {
	if hc == nil || len(hc.Test) == 0 {
		return false
	}
	switch hc.Test[0] {
	case "CMD":
		return len(hc.Test) > 1
	case "CMD-SHELL":
		return len(hc.Test) > 1 && strings.TrimSpace(hc.Test[1]) != ""
	default:
		return false
	}
}

func (hc *HealthConfig) argv() []string {
	if !hc.enabled() {
		return nil
	}
	switch hc.Test[0] {
	case "CMD":
		return slices.Clone(hc.Test[1:])
	case "CMD-SHELL":
		return []string{"/bin/sh", "-c", hc.Test[1]}
	default:
		return nil
	}
}

func (hc *HealthConfig) probeInterval() time.Duration {
	if hc.Interval > 0 {
		return hc.Interval
	}
	return defaultHealthInterval
}

func (hc *HealthConfig) probeTimeout() time.Duration {
	if hc.Timeout > 0 {
		return hc.Timeout
	}
	return defaultHealthTimeout
}

func (hc *HealthConfig) probeStartInterval() time.Duration {
	if hc.StartInterval > 0 {
		return hc.StartInterval
	}
	return defaultHealthStartInterval
}

func (hc *HealthConfig) probeRetries() int {
	if hc.Retries > 0 {
		return hc.Retries
	}
	return defaultHealthRetries
}

func parseHealthConfig(raw json.RawMessage) (*HealthConfig, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var hc HealthConfig
	if err := json.Unmarshal(trimmed, &hc); err != nil {
		return nil, errInvalid("invalid Healthcheck config: %v", err)
	}
	if err := validateHealthConfig(&hc); err != nil {
		return nil, err
	}
	if len(hc.Test) == 0 && hc.Interval == 0 && hc.Timeout == 0 && hc.StartPeriod == 0 && hc.StartInterval == 0 && hc.Retries == 0 {
		return nil, nil
	}
	return &hc, nil
}

func validateHealthConfig(hc *HealthConfig) error {
	if hc == nil {
		return nil
	}
	if hc.Interval < 0 {
		return errInvalid("Interval in Healthcheck cannot be negative")
	}
	if hc.Timeout < 0 {
		return errInvalid("Timeout in Healthcheck cannot be negative")
	}
	if hc.StartPeriod < 0 {
		return errInvalid("StartPeriod in Healthcheck cannot be negative")
	}
	if hc.StartInterval < 0 {
		return errInvalid("StartInterval in Healthcheck cannot be negative")
	}
	if hc.Retries < 0 {
		return errInvalid("Retries in Healthcheck cannot be negative")
	}
	if len(hc.Test) > 0 {
		switch hc.Test[0] {
		case "NONE", "CMD", "CMD-SHELL":
		default:
			return errInvalid("Unknown healthcheck type %q (expected 'CMD', 'CMD-SHELL', or 'NONE')", hc.Test[0])
		}
	}
	return nil
}

// mergeHealthConfig merges the container request's Healthcheck onto the image's
// following Docker's per-field override semantics.
func mergeHealthConfig(imgRaw, reqRaw json.RawMessage) (json.RawMessage, *HealthConfig, error) {
	imgHC, _ := parseHealthConfig(imgRaw)
	reqHC, err := parseHealthConfig(reqRaw)
	if err != nil {
		return nil, nil, err
	}
	if reqHC == nil {
		if imgHC == nil {
			return nil, nil, nil
		}
		b, err := json.Marshal(imgHC)
		if err != nil {
			return nil, nil, err
		}
		return b, imgHC, nil
	}
	merged := *reqHC
	merged.Test = slices.Clone(reqHC.Test)
	if imgHC != nil {
		if len(merged.Test) == 0 {
			merged.Test = slices.Clone(imgHC.Test)
		}
		if merged.Interval == 0 {
			merged.Interval = imgHC.Interval
		}
		if merged.Timeout == 0 {
			merged.Timeout = imgHC.Timeout
		}
		if merged.StartPeriod == 0 {
			merged.StartPeriod = imgHC.StartPeriod
		}
		if merged.StartInterval == 0 {
			merged.StartInterval = imgHC.StartInterval
		}
		if merged.Retries == 0 {
			merged.Retries = imgHC.Retries
		}
	}
	if len(merged.Test) > 0 && merged.Test[0] == "NONE" {
		merged = HealthConfig{Test: []string{"NONE"}}
	}
	b, err := json.Marshal(&merged)
	if err != nil {
		return nil, nil, err
	}
	return b, &merged, nil
}

// enrichImageHealthcheck populates img.Config.Healthcheck from the cached OCI
// image config when rungv inspect omitted it.
func (e *Engine) enrichImageHealthcheck(ctx context.Context, img *ImageInspect) {
	if img == nil || img.ID == "" || len(bytes.TrimSpace(img.Config.Healthcheck)) > 0 {
		return
	}
	info, err := e.runner.Info(ctx)
	if err != nil || info.LayerCacheDir == "" {
		return
	}
	rawCfg, err := imagefs.ReadGVXImageConfig(info.LayerCacheDir, img.ID)
	if err != nil || len(rawCfg) == 0 {
		return
	}
	var doc struct {
		Config struct {
			Healthcheck json.RawMessage `json:"Healthcheck"`
		} `json:"config"`
	}
	if json.Unmarshal(rawCfg, &doc) != nil {
		return
	}
	trimmed := bytes.TrimSpace(doc.Config.Healthcheck)
	if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		img.Config.Healthcheck = slices.Clone(trimmed)
	}
}

func (e *Engine) openHealthMonitors() {
	e.hlt.mu.Lock()
	e.hlt.closed = false
	if e.hlt.byID == nil {
		e.hlt.byID = map[string]*healthMonitor{}
	}
	e.hlt.mu.Unlock()
}

func (e *Engine) closeHealthMonitors() {
	e.hlt.mu.Lock()
	e.hlt.closed = true
	mons := make([]*healthMonitor, 0, len(e.hlt.byID))
	for _, m := range e.hlt.byID {
		mons = append(mons, m)
	}
	clear(e.hlt.byID)
	e.hlt.mu.Unlock()

	for _, m := range mons {
		m.cancel()
	}
	for _, m := range mons {
		<-m.done
	}
}

func (e *Engine) startHealthMonitor(id string, gen uint64, hc *HealthConfig, startedAt time.Time) {
	if !hc.enabled() || (!e.isMicroVMContainer(id) && !e.runner.Features().Control) {
		return
	}
	hcCopy := *hc
	hcCopy.Test = slices.Clone(hc.Test)

	ctx, cancel := context.WithCancel(context.Background())
	mon := &healthMonitor{
		gen:    gen,
		cancel: cancel,
		done:   make(chan struct{}),
	}

	var prev *healthMonitor
	e.hlt.mu.Lock()
	if e.hlt.closed {
		e.hlt.mu.Unlock()
		cancel()
		close(mon.done)
		return
	}
	if e.hlt.byID == nil {
		e.hlt.byID = map[string]*healthMonitor{}
	}
	prev = e.hlt.byID[id]
	e.hlt.byID[id] = mon
	e.hlt.mu.Unlock()

	if prev != nil {
		prev.cancel()
	}

	go func() {
		defer close(mon.done)
		defer func() {
			cancel()
			e.hlt.mu.Lock()
			if e.hlt.byID[id] == mon {
				delete(e.hlt.byID, id)
			}
			e.hlt.mu.Unlock()
		}()
		e.runHealthMonitor(ctx, id, gen, &hcCopy, startedAt)
	}()
}

func (e *Engine) stopHealthMonitor(id string) {
	e.hlt.mu.Lock()
	mon := e.hlt.byID[id]
	delete(e.hlt.byID, id)
	e.hlt.mu.Unlock()
	if mon != nil {
		mon.cancel()
	}
}

func (e *Engine) runHealthMonitor(ctx context.Context, id string, gen uint64, hc *HealthConfig, startedAt time.Time) {
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	argv := hc.argv()
	if len(argv) == 0 {
		return
	}
	probeInterval := hc.probeInterval()
	probeTimeout := hc.probeTimeout()
	startPeriod := hc.StartPeriod
	startInterval := hc.probeStartInterval()
	retries := hc.probeRetries()

	timer := time.NewTimer(0)
	defer timer.Stop()
	// Drain initial immediate tick before setting the first real delay.
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}

	for {
		c, ok := e.healthLookupRun(id, gen)
		if !ok {
			return
		}
		inStartPeriod := startPeriod > 0 && time.Since(startedAt) < startPeriod
		curStatus := HealthStarting
		if c.State.Health != nil && c.State.Health.Status != "" {
			curStatus = c.State.Health.Status
		}
		wait := probeInterval
		if inStartPeriod && curStatus == HealthStarting {
			wait = startInterval
		}
		timer.Reset(wait)

		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		c, ok = e.healthLookupRun(id, gen)
		if !ok {
			return
		}
		if e.isContainerPaused(c) {
			continue
		}

		res, ran := e.runOneHealthProbe(ctx, c, argv, probeTimeout)
		if !ran {
			if ctx.Err() != nil {
				return
			}
			continue
		}

		inStartPeriod = startPeriod > 0 && time.Since(startedAt) < startPeriod
		if !e.recordHealthResult(id, gen, res, inStartPeriod, retries) {
			return
		}
	}
}

func (e *Engine) healthLookupRun(id string, gen uint64) (Container, bool) {
	e.ctr.mu.Lock()
	defer e.ctr.mu.Unlock()
	c := e.ctr.byID[id]
	if c == nil || c.runGen != gen || c.State.Status != StatusRunning {
		return Container{}, false
	}
	return c.clone(), true
}

func (e *Engine) runOneHealthProbe(ctx context.Context, c Container, argv []string, timeout time.Duration) (HealthcheckResult, bool) {
	start := time.Now().UTC()
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Build environment mirroring execStart so GVX_NET never leaks into probes.
	baseEnv := slices.Clone(c.Config.Env)
	if e.startedWithNetinit(c.ID) {
		baseEnv = append(baseEnv, guestcfg.EnvVar+"=")
	}
	env := mergeExecEnv(baseEnv, nil)
	opts := sandbox.ExecOptions{
		ExecOptions: rungv.ExecOptions{
			Argv:    argv,
			Env:     env,
			Workdir: c.Config.WorkingDir,
			User:    c.Config.User,
		},
	}

	// Wait briefly if the sandbox control socket is still booting right after start.
	var (
		proc sandbox.ExecProcess
		err  error
	)
	for {
		ctl := e.ctrControl(c)
		if ctl != nil {
			proc, err = ctl.Exec(probeCtx, opts)
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return HealthcheckResult{}, false
			}
			if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
				return HealthcheckResult{
					Start:    start,
					End:      time.Now().UTC(),
					ExitCode: -1,
					Output:   fmt.Sprintf("Health check exceeded timeout (%v)", timeout),
				}, true
			}
			if !isControlUnreachable(err) {
				return HealthcheckResult{
					Start:    start,
					End:      time.Now().UTC(),
					ExitCode: 1,
					Output:   truncateHealthOutput(execErrorMessage(err)),
				}, true
			}
		}
		if _, ok := e.healthLookupRun(c.ID, c.runGen); !ok {
			return HealthcheckResult{}, false
		}
		retryTimer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			retryTimer.Stop()
			return HealthcheckResult{}, false
		case <-probeCtx.Done():
			retryTimer.Stop()
			return HealthcheckResult{
				Start:    start,
				End:      time.Now().UTC(),
				ExitCode: -1,
				Output:   fmt.Sprintf("Health check exceeded timeout (%v)", timeout),
			}, true
		case <-retryTimer.C:
		}
	}

	outBuf := &healthLimitedBuffer{max: maxHealthOutputBytes}
	var ioWG sync.WaitGroup
	drain := func(r io.Reader) {
		if r == nil {
			return
		}
		ioWG.Add(1)
		go func() {
			defer ioWG.Done()
			_, _ = io.Copy(outBuf, r)
		}()
	}
	drain(proc.Stdout())
	drain(proc.Stderr())

	waitDone := make(chan struct{})
	go func() {
		select {
		case <-probeCtx.Done():
			_ = proc.Close()
		case <-waitDone:
		}
	}()

	code, waitErr := proc.Wait()
	close(waitDone)
	_ = proc.Close()
	ioWG.Wait()

	if ctx.Err() != nil {
		return HealthcheckResult{}, false
	}
	end := time.Now().UTC()
	if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
		return HealthcheckResult{
			Start:    start,
			End:      end,
			ExitCode: -1,
			Output:   fmt.Sprintf("Health check exceeded timeout (%v)", timeout),
		}, true
	}
	out := outBuf.String()
	if waitErr != nil {
		if code == 0 {
			code = 1
		}
		if out == "" {
			out = truncateHealthOutput(execErrorMessage(waitErr))
		}
	}
	return HealthcheckResult{
		Start:    start,
		End:      end,
		ExitCode: code,
		Output:   out,
	}, true
}

func (e *Engine) recordHealthResult(id string, gen uint64, res HealthcheckResult, inStartPeriod bool, retries int) bool {
	var (
		statusChanged bool
		newStatus     string
		attrs         map[string]string
	)
	e.ctr.mu.Lock()
	c := e.ctr.byID[id]
	if c == nil || c.runGen != gen || c.State.Status != StatusRunning {
		e.ctr.mu.Unlock()
		return false
	}
	if c.State.Health == nil {
		c.State.Health = &ContainerHealth{
			Status: HealthStarting,
			Log:    []HealthcheckResult{},
		}
	}
	h := c.State.Health
	prevStatus := h.Status
	h.Log = append(h.Log, res)
	if len(h.Log) > maxHealthLogEntries {
		h.Log = slices.Clone(h.Log[len(h.Log)-maxHealthLogEntries:])
	}

	if res.ExitCode == 0 {
		h.FailingStreak = 0
		h.Status = HealthHealthy
	} else {
		if inStartPeriod && prevStatus == HealthStarting {
			// Failures during start_period do not count toward retries.
		} else {
			h.FailingStreak++
			if h.FailingStreak >= retries {
				h.Status = HealthUnhealthy
			}
		}
	}
	if h.Status != prevStatus {
		statusChanged = true
		newStatus = h.Status
		attrs = containerAttrs(c)
		attrs["health_status"] = newStatus
	}
	e.ctr.mu.Unlock()

	if statusChanged {
		e.emit(Event{
			Type:       "container",
			Action:     "health_status: " + newStatus,
			ID:         id,
			Attributes: attrs,
		})
	}
	e.notify()
	return true
}

// healthLimitedBuffer captures up to max bytes from concurrent stdout/stderr
// readers and discards the remainder so child processes never block.
type healthLimitedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (b *healthLimitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rem := b.max - b.buf.Len()
	if rem > 0 {
		if len(p) <= rem {
			_, _ = b.buf.Write(p)
		} else {
			_, _ = b.buf.Write(p[:rem])
		}
	}
	return len(p), nil
}

func (b *healthLimitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func truncateHealthOutput(s string) string {
	if len(s) <= maxHealthOutputBytes {
		return s
	}
	return s[:maxHealthOutputBytes]
}

// parseHealthcheckChange parses a Dockerfile HEALTHCHECK instruction used by
// `docker commit --change "HEALTHCHECK ..."`.
func parseHealthcheckChange(rest string) (json.RawMessage, error) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return nil, errInvalid("HEALTHCHECK requires at least one argument")
	}
	if strings.EqualFold(rest, "NONE") {
		return json.RawMessage(`{"Test":["NONE"]}`), nil
	}
	var hc HealthConfig
	cur := rest
	for strings.HasPrefix(cur, "--") {
		tok, after, hasSpace := strings.Cut(cur, " ")
		k, v, hasEq := strings.Cut(tok, "=")
		if !hasEq {
			return nil, errInvalid("HEALTHCHECK flag %q requires a value", tok)
		}
		switch strings.ToLower(k) {
		case "--interval":
			d, err := time.ParseDuration(v)
			if err != nil {
				return nil, errInvalid("invalid HEALTHCHECK --interval %q: %v", v, err)
			}
			hc.Interval = d
		case "--timeout":
			d, err := time.ParseDuration(v)
			if err != nil {
				return nil, errInvalid("invalid HEALTHCHECK --timeout %q: %v", v, err)
			}
			hc.Timeout = d
		case "--start-period":
			d, err := time.ParseDuration(v)
			if err != nil {
				return nil, errInvalid("invalid HEALTHCHECK --start-period %q: %v", v, err)
			}
			hc.StartPeriod = d
		case "--start-interval":
			d, err := time.ParseDuration(v)
			if err != nil {
				return nil, errInvalid("invalid HEALTHCHECK --start-interval %q: %v", v, err)
			}
			hc.StartInterval = d
		case "--retries":
			n, err := strconv.Atoi(v)
			if err != nil {
				return nil, errInvalid("invalid HEALTHCHECK --retries %q: %v", v, err)
			}
			hc.Retries = n
		default:
			return nil, errInvalid("unknown HEALTHCHECK flag %q", k)
		}
		if !hasSpace {
			cur = ""
			break
		}
		cur = strings.TrimSpace(after)
	}
	kw, cmdRest, _ := strings.Cut(cur, " ")
	cmdRest = strings.TrimSpace(cmdRest)
	if !strings.EqualFold(kw, "CMD") || cmdRest == "" {
		return nil, errInvalid("HEALTHCHECK requires NONE or CMD")
	}
	if a, ok := jsonArray(cmdRest); ok {
		hc.Test = append([]string{"CMD"}, a...)
	} else {
		hc.Test = []string{"CMD-SHELL", cmdRest}
	}
	if err := validateHealthConfig(&hc); err != nil {
		return nil, err
	}
	b, err := json.Marshal(&hc)
	if err != nil {
		return nil, err
	}
	return b, nil
}
