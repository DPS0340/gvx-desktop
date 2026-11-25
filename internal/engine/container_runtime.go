package engine

// Owner: W2 (sandbox engine, docs/design/gvx-v020.md §5.4).

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ironpark/gvx-desktop-vm/vmproto"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

const minContainerMemoryBytes = 6 * 1024 * 1024

func (e *Engine) hasRunFlag(name string) bool {
	if r, ok := e.runner.(interface{ HasRunFlag(string) bool }); ok {
		return r.HasRunFlag(name)
	}
	return false
}

// isContainerPaused reports whether c is currently paused.
func (e *Engine) isContainerPaused(c Container) bool {
	if c.State.Status != StatusRunning {
		return false
	}
	return c.State.Paused || e.ctrState(c).Paused
}

func (e *Engine) requireControl(c Container, what string) (sandbox.Control, error) {
	if c.State.Status == StatusRestarting {
		return nil, errConflict("Container %s is restarting, wait until the container is running", c.ID)
	}
	if c.State.Status != StatusRunning {
		return nil, errConflict("Container %s is not running", c.ID)
	}
	ctl := e.ctrControl(c)
	if ctl == nil {
		return nil, errConflict("Container %s was started without a control socket; restart it to use %s", c.ID, what)
	}
	return ctl, nil
}

// controlCode extracts rungv's protocol error code, falling back to stderr
// parsing if rungv.ControlCode has not been implemented yet.
func controlCode(err error) string {
	if err == nil {
		return ""
	}
	if ve, ok := errors.AsType[*vmproto.Error](err); ok {
		switch ve.Code {
		case vmproto.CodeNotFound:
			return rungv.CodeNotFound
		case vmproto.CodeAlreadyPaused, vmproto.CodePaused:
			return rungv.CodeAlreadyPaused
		case vmproto.CodeNotPaused:
			return rungv.CodeNotPaused
		case vmproto.CodeBadRequest:
			return rungv.CodeInvalidArgument
		}
	}
	if code := rungv.ControlCode(err); code != "" {
		return code
	}
	re, ok := errors.AsType[*rungv.Error](err)
	if !ok {
		return ""
	}
	line, ok := rungv.CutProgram(lastNonEmptyLine(re.Stderr), " ")
	if !ok {
		return ""
	}
	_, rest, ok := strings.Cut(line, ": ")
	if !ok {
		return ""
	}
	code, _, ok := strings.Cut(rest, ": ")
	if !ok || code == "" {
		return ""
	}
	for _, r := range code {
		if (r < 'A' || r > 'Z') && r != '_' {
			return ""
		}
	}
	return code
}

func isControlUnreachable(err error) bool {
	if err == nil {
		return false
	}
	if rungv.IsUnreachable(err) {
		return true
	}
	msg := strings.ToLower(execErrorMessage(err))
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such file or directory") ||
		strings.Contains(msg, "connect:")
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

// ---- pause / unpause ----

func (e *Engine) containerPause(w http.ResponseWriter, r *http.Request) {
	if !e.hasControlFor(r.PathValue("id")) {
		writeError(w, r, errNotSupported("Pausing a container"))
		return
	}
	c, err := e.lookup(r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	if _, err := e.requireControl(c, "pause"); err != nil {
		writeError(w, r, err)
		return
	}
	if e.isContainerPaused(c) {
		writeError(w, r, errConflict("Container %s is already paused", c.ID))
		return
	}
	if err := e.ctrPause(r.Context(), c); err != nil {
		if errors.Is(err, sandbox.ErrNoControl) {
			writeError(w, r, errConflict("Container %s was started without a control socket; restart it to use pause", c.ID))
			return
		}
		if controlCode(err) == rungv.CodeAlreadyPaused {
			writeError(w, r, errConflict("Container %s is already paused", c.ID))
			return
		}
		writeError(w, r, err)
		return
	}
	e.ctr.mu.Lock()
	var attrs map[string]string
	if cur := e.ctr.byID[c.ID]; cur != nil && cur.State.Status == StatusRunning {
		cur.State.Paused = true
		attrs = containerAttrs(cur)
	}
	e.ctr.mu.Unlock()
	if attrs == nil {
		attrs = containerAttrs(&c)
	}
	e.emit(Event{Type: "container", Action: "pause", ID: c.ID, Attributes: attrs})
	e.notify()
	w.WriteHeader(http.StatusNoContent)
}

func (e *Engine) containerUnpause(w http.ResponseWriter, r *http.Request) {
	if !e.hasControlFor(r.PathValue("id")) {
		writeError(w, r, errNotSupported("Pausing a container"))
		return
	}
	c, err := e.lookup(r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	if _, err := e.requireControl(c, "unpause"); err != nil {
		writeError(w, r, err)
		return
	}
	if !e.isContainerPaused(c) {
		writeError(w, r, errConflict("Container %s is not paused", c.ID))
		return
	}
	if err := e.ctrUnpause(r.Context(), c); err != nil {
		if errors.Is(err, sandbox.ErrNoControl) {
			writeError(w, r, errConflict("Container %s was started without a control socket; restart it to use unpause", c.ID))
			return
		}
		if controlCode(err) == rungv.CodeNotPaused {
			writeError(w, r, errConflict("Container %s is not paused", c.ID))
			return
		}
		writeError(w, r, err)
		return
	}
	e.ctr.mu.Lock()
	var attrs map[string]string
	if cur := e.ctr.byID[c.ID]; cur != nil {
		cur.State.Paused = false
		attrs = containerAttrs(cur)
	}
	e.ctr.mu.Unlock()
	if attrs == nil {
		attrs = containerAttrs(&c)
	}
	e.emit(Event{Type: "container", Action: "unpause", ID: c.ID, Attributes: attrs})
	e.notify()
	w.WriteHeader(http.StatusNoContent)
}

// ---- top ----

type containerTopResponse struct {
	Titles    []string   `json:"Titles"`
	Processes [][]string `json:"Processes"`
}

func formatTopCPUTime(cpuNanos int64) string {
	if cpuNanos < 0 {
		cpuNanos = 0
	}
	totalSec := cpuNanos / int64(time.Second)
	h := totalSec / 3600
	m := (totalSec % 3600) / 60
	s := totalSec % 60
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

func (e *Engine) containerTop(w http.ResponseWriter, r *http.Request) {
	if !e.hasControlFor(r.PathValue("id")) {
		writeError(w, r, errNotSupported("Listing the processes of a container"))
		return
	}
	c, err := e.lookup(r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	ctl, err := e.requireControl(c, "top")
	if err != nil {
		writeError(w, r, err)
		return
	}
	procs, err := ctl.Top(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	rows := make([][]string, 0, len(procs))
	for _, p := range procs {
		cmd := strings.Join(p.Cmdline, " ")
		if cmd == "" && p.Comm != "" {
			cmd = "[" + p.Comm + "]"
		}
		rows = append(rows, []string{
			strconv.FormatUint(uint64(p.UID), 10),
			strconv.FormatInt(int64(p.PID), 10),
			strconv.FormatInt(int64(p.PPID), 10),
			"0",
			"-",
			"?",
			formatTopCPUTime(p.CPUNanos),
			cmd,
		})
	}
	writeJSON(w, http.StatusOK, containerTopResponse{
		Titles:    []string{"UID", "PID", "PPID", "C", "STIME", "TTY", "TIME", "CMD"},
		Processes: rows,
	})
}

// ---- update ----

type containerUpdateRequest struct {
	Memory             int64          `json:"Memory"`
	NanoCpus           int64          `json:"NanoCpus"`
	CpuQuota           int64          `json:"CpuQuota"`
	CpuPeriod          int64          `json:"CpuPeriod"`
	PidsLimit          *int64         `json:"PidsLimit"`
	RestartPolicy      *RestartPolicy `json:"RestartPolicy"`
	CpuShares          int64          `json:"CpuShares"`
	BlkioWeight        uint16         `json:"BlkioWeight"`
	CpusetCpus         string         `json:"CpusetCpus"`
	CpusetMems         string         `json:"CpusetMems"`
	MemorySwap         int64          `json:"MemorySwap"`
	MemoryReservation  int64          `json:"MemoryReservation"`
	KernelMemory       int64          `json:"KernelMemory"`
	KernelMemoryTCP    int64          `json:"KernelMemoryTCP"`
	CpuRealtimePeriod  int64          `json:"CpuRealtimePeriod"`
	CpuRealtimeRuntime int64          `json:"CpuRealtimeRuntime"`
	IOMaximumIOps      uint64         `json:"IOMaximumIOps"`
	IOMaximumBandwidth uint64         `json:"IOMaximumBandwidth"`
}

func (e *Engine) containerUpdate(w http.ResponseWriter, r *http.Request) {
	if !e.hasControlFor(r.PathValue("id")) {
		writeError(w, r, errNotSupported("Updating a container"))
		return
	}
	c, err := e.lookup(r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	var req containerUpdateRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, r, err)
		return
	}

	if req.Memory != 0 && req.Memory < minContainerMemoryBytes {
		writeError(w, r, errInvalid("Minimum memory limit allowed is 6MB"))
		return
	}
	if req.NanoCpus < 0 {
		writeError(w, r, errInvalid("CPU limit cannot be negative"))
		return
	}
	if req.NanoCpus > 0 && (req.CpuQuota != 0 || req.CpuPeriod != 0) {
		writeError(w, r, errInvalid("Conflicting options: Nano CPUs cannot be combined with CPU quota or period"))
		return
	}
	if req.RestartPolicy != nil && req.RestartPolicy.Name != "" {
		testHC := c.HostConfig
		testHC.RestartPolicy = *req.RestartPolicy
		if err := validateRestartPolicy(testHC); err != nil {
			writeError(w, r, err)
			return
		}
	}

	warnings := []string{}
	warnUnsupported := func(field string) {
		warnings = append(warnings, field+" is not supported and was ignored")
	}
	if req.CpuShares != 0 {
		warnUnsupported("CpuShares")
	}
	if req.BlkioWeight != 0 {
		warnUnsupported("BlkioWeight")
	}
	if req.CpusetCpus != "" {
		warnUnsupported("CpusetCpus")
	}
	if req.CpusetMems != "" {
		warnUnsupported("CpusetMems")
	}
	if req.MemorySwap != 0 {
		warnUnsupported("MemorySwap")
	}
	if req.MemoryReservation != 0 {
		warnUnsupported("MemoryReservation")
	}
	if req.KernelMemory != 0 {
		warnUnsupported("KernelMemory")
	}
	if req.KernelMemoryTCP != 0 {
		warnUnsupported("KernelMemoryTCP")
	}
	if req.CpuRealtimePeriod != 0 {
		warnUnsupported("CpuRealtimePeriod")
	}
	if req.CpuRealtimeRuntime != 0 {
		warnUnsupported("CpuRealtimeRuntime")
	}
	if req.IOMaximumIOps != 0 {
		warnUnsupported("IOMaximumIOps")
	}
	if req.IOMaximumBandwidth != 0 {
		warnUnsupported("IOMaximumBandwidth")
	}

	var u rungv.Update
	if req.Memory > 0 {
		u.Memory = strconv.FormatInt(req.Memory, 10) + "b"
	}
	if req.NanoCpus > 0 {
		u.CPUs = strconv.FormatFloat(float64(req.NanoCpus)/1e9, 'f', -1, 64)
	}
	if req.CpuQuota != 0 {
		u.CPUQuota = req.CpuQuota
	}
	if req.CpuPeriod != 0 {
		u.CPUPeriod = req.CpuPeriod
	}
	if req.PidsLimit != nil {
		v := *req.PidsLimit
		u.PidsLimit = &v
	}

	if c.State.Status == StatusRunning && !u.IsZero() {
		if c.isMicroVM() {
			warnings = append(warnings, "Live resource updates are not supported for running micro-VM containers; new limits apply at the next start")
		} else if e.runner.Control(c.ID) != nil {
			if _, err := e.runner.Update(r.Context(), c.ID, u); err != nil {
				writeError(w, r, err)
				return
			}
		} else {
			warnings = append(warnings, "The new limits apply at the next start: the container runs without a control socket")
		}
	}

	e.ctr.mu.Lock()
	cur, ok := e.ctr.byID[c.ID]
	if !ok {
		e.ctr.mu.Unlock()
		writeError(w, r, errNotFound("No such container: %s", c.ID))
		return
	}
	if req.Memory > 0 {
		cur.HostConfig.Memory = req.Memory
		cur.Spec.Memory = strconv.FormatInt(req.Memory, 10) + "b"
	}
	if req.NanoCpus > 0 {
		cur.HostConfig.NanoCpus = req.NanoCpus
		cur.HostConfig.CpuQuota = 0
		cur.HostConfig.CpuPeriod = 0
		cur.Spec.CPUs = strconv.FormatFloat(float64(req.NanoCpus)/1e9, 'f', -1, 64)
	}
	if req.CpuPeriod != 0 {
		cur.HostConfig.CpuPeriod = req.CpuPeriod
		if req.NanoCpus == 0 && req.CpuQuota == 0 && cur.HostConfig.CpuQuota > 0 && req.CpuPeriod > 0 {
			cur.Spec.CPUs = strconv.FormatFloat(float64(cur.HostConfig.CpuQuota)/float64(req.CpuPeriod), 'f', -1, 64)
		}
	}
	if req.CpuQuota != 0 {
		cur.HostConfig.CpuQuota = req.CpuQuota
		if req.NanoCpus == 0 {
			cur.HostConfig.NanoCpus = 0
			if req.CpuQuota < 0 {
				cur.Spec.CPUs = ""
			} else {
				period := cur.HostConfig.CpuPeriod
				if period <= 0 {
					period = 100000
				}
				cur.Spec.CPUs = strconv.FormatFloat(float64(req.CpuQuota)/float64(period), 'f', -1, 64)
			}
		}
	}
	if req.PidsLimit != nil {
		v := *req.PidsLimit
		cur.HostConfig.PidsLimit = &v
	}
	if req.RestartPolicy != nil && req.RestartPolicy.Name != "" {
		cur.HostConfig.RestartPolicy = *req.RestartPolicy
	}
	attrs := containerAttrs(cur)
	if err := e.saveContainersLocked(); err != nil {
		e.logf("engine: save containers.json: %v", err)
	}
	e.ctr.mu.Unlock()

	e.emit(Event{Type: "container", Action: "update", ID: c.ID, Attributes: attrs})
	e.notify()

	writeJSON(w, http.StatusOK, struct {
		Warnings []string `json:"Warnings"`
	}{Warnings: warnings})
}
