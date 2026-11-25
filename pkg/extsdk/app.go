package extsdk

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	maxRequestBodyBytes = 1 << 20 // 1 MiB
	maxEventValueBytes  = 8 << 10 // 8 KiB
	maxDragPayloadBytes = 256
)

// Config configures App.RunWithConfig.
type Config struct {
	ExtID            string
	Version          string
	HostAPI          string
	HostToken        string
	ListenAddr       string
	ExtToken         string
	DataDir          string
	DockerHost       string
	Listener         net.Listener
	CoalesceInterval time.Duration
}

// Handlers maps action names (from OnTap, OnChange, etc.) to event handler functions.
// Supported signatures:
//   - func(*EventContext) error
//   - func(*EventContext)
//   - func(*EventContext, string) error
//   - func(*EventContext, string)
//   - func(*EventContext, bool) error
//   - func(*EventContext, bool)
//   - func(*EventContext, []json.RawMessage) error
//   - func(*EventContext, json.RawMessage) error
//   - func(*EventContext, any) error
type Handlers map[string]any

// View defines one declarative UI view served over SSE and interactive events.
type View struct {
	Title  string
	Chrome string
	Render func(ctx *ViewContext, ui *UI) Node
	On     Handlers
}

type registeredView struct {
	view     View
	handlers map[string]func(*EventContext) error
}

type toolEntry struct {
	call func(*ToolContext) (any, error)
}

type settingsEntry struct {
	call func(*Context, map[string]any) error
}

type eventEntry struct {
	call func(*Context, HostEvent)
}

// App is the core extension server and lifecycle runtime.
type App struct {
	mu               sync.RWMutex
	cfg              Config
	version          string
	host             *Host
	views            map[string]*registeredView
	commands         map[string]func(*CommandContext) error
	tools            map[string]toolEntry
	settingsHandlers []settingsEntry
	eventHandlers    []eventEntry
	broadcasters     map[string]*viewBroadcaster
	viewIndexes      map[string]*viewIndex

	listenAddr string
	stopping   chan struct{}
	stopOnce   sync.Once
}

// New constructs a new Extension SDK App.
func New() *App {
	return &App{
		version:      "0.1.0",
		views:        make(map[string]*registeredView),
		commands:     make(map[string]func(*CommandContext) error),
		tools:        make(map[string]toolEntry),
		broadcasters: make(map[string]*viewBroadcaster),
		viewIndexes:  make(map[string]*viewIndex),
		stopping:     make(chan struct{}),
	}
}

// SetVersion sets the version string returned by GET /v1/health.
func (a *App) SetVersion(v string) *App {
	a.mu.Lock()
	a.version = v
	a.mu.Unlock()
	return a
}

// View registers a named view and validates its On handler signatures.
func (a *App) View(id string, v View) *App {
	if id == "" {
		panic("extsdk.App.View: view id must not be empty")
	}
	wrapped := make(map[string]func(*EventContext) error, len(v.On))
	for name, fn := range v.On {
		w, err := wrapViewHandler(fn)
		if err != nil {
			panic(fmt.Sprintf("extsdk.App.View(%q) handler %q: %v", id, name, err))
		}
		wrapped[name] = w
	}
	a.mu.Lock()
	a.views[id] = &registeredView{view: v, handlers: wrapped}
	if _, ok := a.broadcasters[id]; !ok {
		a.broadcasters[id] = newViewBroadcaster()
	}
	a.mu.Unlock()
	return a
}

// Command registers a command palette handler for POST /v1/commands/{cmdId}.
func (a *App) Command(id string, fn func(*CommandContext) error) *App {
	if id == "" {
		panic("extsdk.App.Command: command id must not be empty")
	}
	if fn == nil {
		panic("extsdk.App.Command: handler must not be nil")
	}
	a.mu.Lock()
	a.commands[id] = fn
	a.mu.Unlock()
	return a
}

// Tool registers an MCP tool handler for POST /v1/tools/{toolId}.
// Supported signatures:
//   - func(*ToolContext, json.RawMessage) (any, error)
//   - func(*ToolContext, map[string]any) (any, error)
//   - func(*ToolContext) (any, error)
//   - func(*ToolContext, json.RawMessage) error
//   - func(*ToolContext, map[string]any) error
//   - func(*ToolContext) error
func (a *App) Tool(id string, fn any) *App {
	if id == "" {
		panic("extsdk.App.Tool: tool id must not be empty")
	}
	w, err := wrapToolHandler(fn)
	if err != nil {
		panic(fmt.Sprintf("extsdk.App.Tool(%q): %v", id, err))
	}
	a.mu.Lock()
	a.tools[id] = toolEntry{call: w}
	a.mu.Unlock()
	return a
}

// OnSettingsChanged registers a callback invoked on POST /v1/settings/changed.
// Supported signatures:
//   - func(*Context)
//   - func(*Context) error
//   - func(*Context, map[string]any) error
//   - func(*Context, map[string]any)
func (a *App) OnSettingsChanged(fn any) *App {
	w, err := wrapSettingsHandler(fn)
	if err != nil {
		panic(fmt.Sprintf("extsdk.App.OnSettingsChanged: %v", err))
	}
	a.mu.Lock()
	a.settingsHandlers = append(a.settingsHandlers, settingsEntry{call: w})
	a.mu.Unlock()
	return a
}

// OnEvent registers a callback invoked when Host SSE events arrive from GET /v1/events.
// Supported signatures:
//   - func(*Context, HostEvent)
//   - func(HostEvent)
func (a *App) OnEvent(fn any) *App {
	w, err := wrapEventHandler(fn)
	if err != nil {
		panic(fmt.Sprintf("extsdk.App.OnEvent: %v", err))
	}
	a.mu.Lock()
	a.eventHandlers = append(a.eventHandlers, eventEntry{call: w})
	a.mu.Unlock()
	return a
}

// NotifyChanged wakes active SSE streams for the given viewIDs (or all views if empty).
func (a *App) NotifyChanged(viewIDs ...string) {
	a.mu.RLock()
	var targets []*viewBroadcaster
	if len(viewIDs) == 0 {
		targets = make([]*viewBroadcaster, 0, len(a.broadcasters))
		for _, b := range a.broadcasters {
			targets = append(targets, b)
		}
	} else {
		for _, id := range viewIDs {
			if b, ok := a.broadcasters[id]; ok {
				targets = append(targets, b)
			}
		}
	}
	a.mu.RUnlock()
	for _, b := range targets {
		b.notify()
	}
}

// Host returns the configured Host API client (may be a failing placeholder if HostAPI was empty).
func (a *App) Host() *Host {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.host
}

// ListenAddr returns the resolved listener address once Run/RunWithConfig has bound it.
func (a *App) ListenAddr() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.listenAddr
}

func (a *App) stoppingCh() <-chan struct{} {
	return a.stopping
}

func (a *App) triggerShutdown() {
	a.stopOnce.Do(func() {
		close(a.stopping)
	})
}

func (a *App) coalesceInterval() time.Duration {
	a.mu.RLock()
	d := a.cfg.CoalesceInterval
	a.mu.RUnlock()
	if d <= 0 {
		return DefaultCoalesceInterval
	}
	return d
}

func (a *App) broadcasterFor(viewID string) *viewBroadcaster {
	a.mu.Lock()
	defer a.mu.Unlock()
	b, ok := a.broadcasters[viewID]
	if !ok {
		b = newViewBroadcaster()
		a.broadcasters[viewID] = b
	}
	return b
}

func (a *App) setViewIndex(viewID string, idx *viewIndex) {
	a.mu.Lock()
	a.viewIndexes[viewID] = idx
	a.mu.Unlock()
}

func (a *App) getViewIndex(viewID string) *viewIndex {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.viewIndexes[viewID]
}

// Run reads GVX_* environment variables and serves the Extension API until ctx cancels,
// SIGTERM/SIGINT arrives, or POST /v1/shutdown is called.
func (a *App) Run(ctx context.Context) error {
	if v := os.Getenv("GVX_API_VERSION"); v != "" && v != "1" {
		return fmt.Errorf("extsdk: unsupported GVX_API_VERSION %q (want \"1\")", v)
	}
	cfg := Config{
		ExtID:      os.Getenv("GVX_EXT_ID"),
		Version:    os.Getenv("GVX_EXT_VERSION"),
		HostAPI:    os.Getenv("GVX_HOST_API"),
		HostToken:  os.Getenv("GVX_HOST_TOKEN"),
		ListenAddr: os.Getenv("GVX_EXT_LISTEN"),
		ExtToken:   os.Getenv("GVX_EXT_TOKEN"),
		DataDir:    os.Getenv("GVX_EXT_DATA_DIR"),
		DockerHost: os.Getenv("GVX_DOCKER_HOST"),
	}
	return a.RunWithConfig(ctx, cfg)
}

// RunWithConfig starts the Extension API server using explicit Config.
func (a *App) RunWithConfig(ctx context.Context, cfg Config) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.ListenAddr == "" && cfg.Listener == nil {
		return errors.New("extsdk: GVX_EXT_LISTEN (or Config.ListenAddr/Listener) is required")
	}

	var hostClient *Host
	if cfg.HostAPI != "" {
		h, err := NewHost(cfg.HostAPI, cfg.HostToken)
		if err != nil {
			return fmt.Errorf("extsdk: invalid HostAPI: %w", err)
		}
		hostClient = h
	} else {
		hostClient = &Host{initErr: errors.New("extsdk: host API is not configured")}
	}

	a.mu.Lock()
	a.cfg = cfg
	if cfg.Version != "" {
		a.version = cfg.Version
	}
	a.host = hostClient
	hasEventHandlers := len(a.eventHandlers) > 0
	a.mu.Unlock()

	ln := cfg.Listener
	var cleanup func()
	if ln == nil {
		var err error
		ln, cleanup, err = listenEndpoint(cfg.ListenAddr)
		if err != nil {
			return err
		}
	}
	if cleanup != nil {
		defer cleanup()
	}

	a.mu.Lock()
	a.listenAddr = ln.Addr().String()
	a.mu.Unlock()

	srv := &http.Server{
		Handler:           a.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	sigCtx, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	var wg sync.WaitGroup
	if cfg.HostAPI != "" && hasEventHandlers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.runEventLoop(sigCtx, hostClient)
		}()
	}

	srvErrCh := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErrCh <- err
		} else {
			srvErrCh <- nil
		}
	}()

	var serveErr error
	select {
	case serveErr = <-srvErrCh:
		a.triggerShutdown()
	case <-sigCtx.Done():
		a.triggerShutdown()
	case <-a.stopping:
	}

	stopSignals()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	_ = ln.Close()
	wg.Wait()

	if serveErr != nil {
		return serveErr
	}
	select {
	case err := <-srvErrCh:
		return err
	default:
		return nil
	}
}

func (a *App) runEventLoop(ctx context.Context, host *Host) {
	backoff := 100 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.stopping:
			return
		default:
		}
		err := host.SubscribeEvents(ctx, func(ev HostEvent) error {
			backoff = 100 * time.Millisecond
			a.dispatchHostEvent(ctx, ev)
			return nil
		})
		if err == nil || ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-a.stopping:
			return
		case <-time.After(backoff):
			if backoff < 2*time.Second {
				backoff *= 2
			}
		}
	}
}

func (a *App) dispatchHostEvent(ctx context.Context, ev HostEvent) {
	a.mu.RLock()
	handlers := slices.Clone(a.eventHandlers)
	a.mu.RUnlock()
	baseCtx := a.newBaseContext(ctx, "")
	for _, h := range handlers {
		func() {
			defer func() { _ = recover() }()
			h.call(baseCtx, ev)
		}()
	}
}

// Handler returns the Extension API http.Handler (with Bearer auth middleware if ExtToken is configured).
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", a.handleHealth)
	mux.HandleFunc("GET /v1/views/{viewId}/stream", a.handleViewStream)
	mux.HandleFunc("POST /v1/views/{viewId}/event", a.handleViewEvent)
	mux.HandleFunc("POST /v1/commands/{cmdId}", a.handleCommand)
	mux.HandleFunc("POST /v1/tools/{toolId}", a.handleTool)
	mux.HandleFunc("POST /v1/settings/changed", a.handleSettingsChanged)
	mux.HandleFunc("POST /v1/shutdown", a.handleShutdown)

	return a.requireBearer(mux)
}

func (a *App) requireBearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.RLock()
		token := a.cfg.ExtToken
		a.mu.RUnlock()
		if token != "" {
			auth := r.Header.Get("Authorization")
			const prefix = "Bearer "
			if !strings.HasPrefix(auth, prefix) {
				writeJSONError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
				return
			}
			got := auth[len(prefix):]
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				writeJSONError(w, http.StatusUnauthorized, "unauthorized", "invalid bearer token")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) newBaseContext(parent context.Context, locale string) *Context {
	if parent == nil {
		parent = context.Background()
	}
	a.mu.RLock()
	cfg := a.cfg
	host := a.host
	a.mu.RUnlock()
	if host == nil {
		host = &Host{initErr: errors.New("extsdk: host API is not configured")}
	}
	if locale == "" {
		locale = "en"
	}
	return &Context{
		Context:    parent,
		Host:       host,
		ExtID:      cfg.ExtID,
		DataDir:    cfg.DataDir,
		DockerHost: cfg.DockerHost,
		Locale:     locale,
		notifyFn:   a.NotifyChanged,
	}
}

func (a *App) renderViewSafe(v View, ctx *ViewContext) (node Node, err error) {
	if v.Render == nil {
		return Node{}, errors.New("view has nil Render function")
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("view render panic: %v", r)
		}
	}()
	return v.Render(ctx, &UI{}), nil
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	ver := a.version
	a.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"version": ver,
	})
}

func (a *App) handleViewStream(w http.ResponseWriter, r *http.Request) {
	viewID := r.PathValue("viewId")
	a.mu.RLock()
	rv, ok := a.views[viewID]
	a.mu.RUnlock()
	if !ok {
		writeJSONError(w, http.StatusNotFound, "not_found", fmt.Sprintf("unknown view %q", viewID))
		return
	}
	a.serveViewStream(w, r, viewID, rv.view)
}

type viewEventRequest struct {
	NodeID  string            `json:"node_id"`
	Node    string            `json:"node"`
	Kind    string            `json:"kind"`
	Name    string            `json:"name"`
	Handler string            `json:"handler"`
	Args    []json.RawMessage `json:"args"`
	Value   string            `json:"value"`
	Values  map[string]string `json:"values"`
	Locale  string            `json:"locale"`
}

func (a *App) handleViewEvent(w http.ResponseWriter, r *http.Request) {
	viewID := r.PathValue("viewId")
	a.mu.RLock()
	rv, ok := a.views[viewID]
	a.mu.RUnlock()
	if !ok {
		writeJSONError(w, http.StatusNotFound, "not_found", fmt.Sprintf("unknown view %q", viewID))
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes+1))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "failed to read request body")
		return
	}
	if len(body) > maxRequestBodyBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds 1 MiB")
		return
	}
	if !utf8.Valid(body) || hasLoneSurrogateEscape(body) {
		writeJSONError(w, http.StatusBadRequest, "invalid_json", "event JSON must be valid UTF-8 without unpaired surrogates")
		return
	}
	var req viewEventRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_json", fmt.Sprintf("invalid event JSON: %v", err))
		return
	}

	ctx := WithGestureID(r.Context(), r.Header.Get("X-GVX-Gesture-ID"))
	locale := req.Locale
	if locale == "" {
		locale = "en"
	}

	idx := a.getViewIndex(viewID)
	if idx == nil {
		vctx := &ViewContext{
			Context: a.newBaseContext(ctx, locale),
			ViewID:  viewID,
		}
		raw, err := a.renderViewSafe(rv.view, vctx)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "render_error", err.Error())
			return
		}
		_, idx = normalizeAndIndex(raw)
		a.setViewIndex(viewID, idx)
	}

	nodeID := req.NodeID
	if nodeID == "" {
		nodeID = req.Node
	}

	handlerName := req.Name
	if handlerName == "" {
		handlerName = req.Handler
	}
	args := req.Args
	if handlerName == "" {
		var rErr error
		handlerName, args, rErr = resolveViewMessage(idx, viewID, nodeID, req.Kind, req.Value)
		if rErr != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_event", rErr.Error())
			return
		}
	}

	handler, ok := rv.handlers[handlerName]
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "unknown_handler", fmt.Sprintf("view %q has no handler %q", viewID, handlerName))
		return
	}

	filteredValues, keyMap := filterFormValues(idx, req.Values)
	evCtx := &EventContext{
		Context:  a.newBaseContext(ctx, locale),
		ViewID:   viewID,
		NodeID:   nodeID,
		Kind:     req.Kind,
		RawValue: req.Value,
		Args:     args,
		values:   filteredValues,
		keyMap:   keyMap,
	}

	if err := invokeSafe(func() error { return handler(evCtx) }); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "handler_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func resolveViewMessage(idx *viewIndex, viewID, nodeID, kind, value string) (string, []json.RawMessage, error) {
	if len(value) > maxEventValueBytes {
		return "", nil, fmt.Errorf("event value exceeds %d KiB", maxEventValueBytes>>10)
	}
	node, found := idx.nodes[nodeID]
	if !found {
		return "", nil, fmt.Errorf("unknown node %q in view %q", nodeID, viewID)
	}
	var rawMsg json.RawMessage
	switch kind {
	case "tap":
		rawMsg = node.onTap
	case "change":
		rawMsg = node.onChange
	case "submit":
		if node.kind == "input" || node.kind == "textarea" {
			rawMsg = node.onSubmit
			if !hasRawMessage(rawMsg) {
				rawMsg = node.onChange
			}
		}
	case "drop":
		if value == "" || len(value) > maxDragPayloadBytes || !utf8.ValidString(value) {
			return "", nil, fmt.Errorf("invalid drop payload for %q", nodeID)
		}
		rawMsg = node.onDrop
	case "check":
		if value != "on" && value != "off" {
			return "", nil, fmt.Errorf("invalid check value %q for %q", value, nodeID)
		}
		rawMsg = node.onCheck
	default:
		return "", nil, fmt.Errorf("unsupported event kind %q", kind)
	}
	if !hasRawMessage(rawMsg) {
		return "", nil, fmt.Errorf("node %q of view %q has no %s handler", nodeID, viewID, kind)
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(rawMsg, &parts); err != nil || len(parts) == 0 {
		return "", nil, errors.New("invalid node action message")
	}
	var name string
	if err := json.Unmarshal(parts[0], &name); err != nil || name == "" {
		return "", nil, errors.New("invalid node action name")
	}
	args := slices.Clone(parts[1:])

	switch kind {
	case "tap":
		if menuKey, ok := idx.itemContextMenu[nodeID]; ok {
			allowed := idx.contextArgs[menuKey]
			if !allowed[value] {
				return "", nil, fmt.Errorf("context menu %q has no opener for arg %q", menuKey, value)
			}
			b, _ := json.Marshal(value)
			args = append(args, b)
		}
	case "change":
		var dynVal any = value
		switch node.kind {
		case "toggle", "collapsible", "tree_item":
			dynVal = value == "true"
		case "checkbox":
			if value != "on" && value != "off" {
				return "", nil, fmt.Errorf("invalid checkbox value %q for %q", value, nodeID)
			}
		case "dialog":
			if value != "dismiss" {
				return "", nil, fmt.Errorf("invalid dialog change value %q for %q", value, nodeID)
			}
		case "dag":
			if !node.dagRows[value] {
				return "", nil, fmt.Errorf("dag %q has no row %q", nodeID, value)
			}
		}
		b, _ := json.Marshal(dynVal)
		args = append(args, b)
	default:
		b, _ := json.Marshal(value)
		args = append(args, b)
	}
	return name, args, nil
}

func filterFormValues(idx *viewIndex, in map[string]string) (map[string]string, map[string]string) {
	if len(in) == 0 {
		return nil, nil
	}
	values := make(map[string]string, len(in))
	for id, v := range in {
		if idx != nil {
			if _, ok := idx.inputKinds[id]; !ok {
				continue
			}
		}
		values[id] = v
	}
	var keyMap map[string]string
	if idx != nil && len(idx.keyUnique) > 0 {
		keyMap = make(map[string]string)
		for k, fullID := range idx.keyUnique {
			if v, ok := values[fullID]; ok {
				keyMap[k] = v
			}
		}
	}
	return values, keyMap
}

func (a *App) handleCommand(w http.ResponseWriter, r *http.Request) {
	cmdID := r.PathValue("cmdId")
	a.mu.RLock()
	fn, ok := a.commands[cmdID]
	a.mu.RUnlock()
	if !ok {
		writeJSONError(w, http.StatusNotFound, "not_found", fmt.Sprintf("unknown command %q", cmdID))
		return
	}

	var req struct {
		Locale string `json:"locale"`
	}
	if r.Body != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes))
		if err == nil && len(strings.TrimSpace(string(body))) > 0 {
			if !utf8.Valid(body) || hasLoneSurrogateEscape(body) {
				writeJSONError(w, http.StatusBadRequest, "invalid_json", "command JSON must be valid UTF-8 without unpaired surrogates")
				return
			}
			if err := json.Unmarshal(body, &req); err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid_json", fmt.Sprintf("invalid command JSON: %v", err))
				return
			}
		}
	}

	ctx := WithGestureID(r.Context(), r.Header.Get("X-GVX-Gesture-ID"))
	cctx := &CommandContext{
		Context:   a.newBaseContext(ctx, req.Locale),
		CommandID: cmdID,
	}
	if err := invokeSafe(func() error { return fn(cctx) }); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "command_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) handleTool(w http.ResponseWriter, r *http.Request) {
	toolID := r.PathValue("toolId")
	a.mu.RLock()
	entry, ok := a.tools[toolID]
	a.mu.RUnlock()
	if !ok {
		writeJSONError(w, http.StatusNotFound, "not_found", fmt.Sprintf("unknown tool %q", toolID))
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes+1))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "failed to read request body")
		return
	}
	if len(body) > maxRequestBodyBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds 1 MiB")
		return
	}

	var rawArgs json.RawMessage
	var locale string
	trimmed := strings.TrimSpace(string(body))
	if trimmed != "" {
		if !utf8.Valid(body) || hasLoneSurrogateEscape(body) {
			writeJSONError(w, http.StatusBadRequest, "invalid_json", "tool JSON must be valid UTF-8 without unpaired surrogates")
			return
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(body, &envelope); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_json", fmt.Sprintf("invalid tool JSON: %v", err))
			return
		}
		if locRaw, hasLoc := envelope["locale"]; hasLoc {
			_ = json.Unmarshal(locRaw, &locale)
		}
		if argsRaw, hasArgs := envelope["args"]; hasArgs {
			rawArgs = argsRaw
		} else {
			rawArgs = json.RawMessage(trimmed)
		}
	}

	ctx := WithGestureID(r.Context(), r.Header.Get("X-GVX-Gesture-ID"))
	tctx := &ToolContext{
		Context: a.newBaseContext(ctx, locale),
		ToolID:  toolID,
		RawArgs: rawArgs,
	}

	var res any
	if err := invokeSafe(func() error {
		var callErr error
		res, callErr = entry.call(tctx)
		return callErr
	}); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "tool_error", err.Error())
		return
	}
	if res == nil {
		res = struct{}{}
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleSettingsChanged(w http.ResponseWriter, r *http.Request) {
	var settings map[string]any
	if r.Body != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes))
		if err == nil && len(strings.TrimSpace(string(body))) > 0 {
			if !utf8.Valid(body) || hasLoneSurrogateEscape(body) {
				writeJSONError(w, http.StatusBadRequest, "invalid_json", "settings JSON must be valid UTF-8 without unpaired surrogates")
				return
			}
			if err := json.Unmarshal(body, &settings); err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid_json", fmt.Sprintf("invalid settings JSON: %v", err))
				return
			}
		}
	}

	a.mu.RLock()
	handlers := slices.Clone(a.settingsHandlers)
	a.mu.RUnlock()

	baseCtx := a.newBaseContext(r.Context(), "")
	for _, h := range handlers {
		if err := invokeSafe(func() error { return h.call(baseCtx, settings) }); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "settings_error", err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) handleShutdown(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	a.triggerShutdown()
}

func invokeSafe(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panic: %v", r)
		}
	}()
	return fn()
}

func wrapViewHandler(fn any) (func(*EventContext) error, error) {
	switch h := fn.(type) {
	case nil:
		return nil, errors.New("handler is nil")
	case func(*EventContext) error:
		return h, nil
	case func(*EventContext):
		return func(ctx *EventContext) error { h(ctx); return nil }, nil
	case func(*EventContext, string) error:
		return func(ctx *EventContext) error {
			val := ctx.RawValue
			if len(ctx.Args) > 0 {
				var s string
				if json.Unmarshal(ctx.Args[len(ctx.Args)-1], &s) == nil {
					val = s
				}
			}
			return h(ctx, val)
		}, nil
	case func(*EventContext, string):
		return func(ctx *EventContext) error {
			val := ctx.RawValue
			if len(ctx.Args) > 0 {
				var s string
				if json.Unmarshal(ctx.Args[len(ctx.Args)-1], &s) == nil {
					val = s
				}
			}
			h(ctx, val)
			return nil
		}, nil
	case func(*EventContext, bool) error:
		return func(ctx *EventContext) error {
			b := ctx.RawValue == "true" || ctx.RawValue == "on"
			if len(ctx.Args) > 0 {
				var decoded bool
				if json.Unmarshal(ctx.Args[len(ctx.Args)-1], &decoded) == nil {
					b = decoded
				}
			}
			return h(ctx, b)
		}, nil
	case func(*EventContext, bool):
		return func(ctx *EventContext) error {
			b := ctx.RawValue == "true" || ctx.RawValue == "on"
			if len(ctx.Args) > 0 {
				var decoded bool
				if json.Unmarshal(ctx.Args[len(ctx.Args)-1], &decoded) == nil {
					b = decoded
				}
			}
			h(ctx, b)
			return nil
		}, nil
	case func(*EventContext, []json.RawMessage) error:
		return func(ctx *EventContext) error { return h(ctx, ctx.Args) }, nil
	case func(*EventContext, json.RawMessage) error:
		return func(ctx *EventContext) error {
			var first json.RawMessage
			if len(ctx.Args) > 0 {
				first = ctx.Args[0]
			}
			return h(ctx, first)
		}, nil
	case func(*EventContext, any) error:
		return func(ctx *EventContext) error {
			var v any
			if len(ctx.Args) > 0 {
				_ = json.Unmarshal(ctx.Args[len(ctx.Args)-1], &v)
			} else {
				v = ctx.RawValue
			}
			return h(ctx, v)
		}, nil
	default:
		return nil, fmt.Errorf("unsupported view handler signature %T", fn)
	}
}

func wrapToolHandler(fn any) (func(*ToolContext) (any, error), error) {
	switch h := fn.(type) {
	case nil:
		return nil, errors.New("handler is nil")
	case func(*ToolContext, json.RawMessage) (any, error):
		return func(ctx *ToolContext) (any, error) { return h(ctx, ctx.RawArgs) }, nil
	case func(*ToolContext, map[string]any) (any, error):
		return func(ctx *ToolContext) (any, error) {
			var m map[string]any
			if err := ctx.DecodeArgs(&m); err != nil {
				return nil, err
			}
			if m == nil {
				m = map[string]any{}
			}
			return h(ctx, m)
		}, nil
	case func(*ToolContext) (any, error):
		return h, nil
	case func(*ToolContext, json.RawMessage) error:
		return func(ctx *ToolContext) (any, error) { return nil, h(ctx, ctx.RawArgs) }, nil
	case func(*ToolContext, map[string]any) error:
		return func(ctx *ToolContext) (any, error) {
			var m map[string]any
			if err := ctx.DecodeArgs(&m); err != nil {
				return nil, err
			}
			if m == nil {
				m = map[string]any{}
			}
			return nil, h(ctx, m)
		}, nil
	case func(*ToolContext) error:
		return func(ctx *ToolContext) (any, error) { return nil, h(ctx) }, nil
	default:
		return nil, fmt.Errorf("unsupported tool handler signature %T", fn)
	}
}

func wrapSettingsHandler(fn any) (func(*Context, map[string]any) error, error) {
	switch h := fn.(type) {
	case nil:
		return nil, errors.New("handler is nil")
	case func(*Context):
		return func(ctx *Context, _ map[string]any) error { h(ctx); return nil }, nil
	case func(*Context) error:
		return func(ctx *Context, _ map[string]any) error { return h(ctx) }, nil
	case func(*Context, map[string]any) error:
		return h, nil
	case func(*Context, map[string]any):
		return func(ctx *Context, m map[string]any) error { h(ctx, m); return nil }, nil
	default:
		return nil, fmt.Errorf("unsupported OnSettingsChanged signature %T", fn)
	}
}

func wrapEventHandler(fn any) (func(*Context, HostEvent), error) {
	switch h := fn.(type) {
	case nil:
		return nil, errors.New("handler is nil")
	case func(*Context, HostEvent):
		return h, nil
	case func(HostEvent):
		return func(_ *Context, ev HostEvent) { h(ev) }, nil
	default:
		return nil, fmt.Errorf("unsupported OnEvent signature %T", fn)
	}
}

func listenEndpoint(addr string) (net.Listener, func(), error) {
	if strings.HasPrefix(addr, "unix://") {
		sockPath := strings.TrimPrefix(addr, "unix://")
		if sockPath == "" {
			return nil, nil, errors.New("extsdk: empty unix listen socket path")
		}
		if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
			return nil, nil, fmt.Errorf("extsdk: mkdir unix socket dir: %w", err)
		}
		if fi, err := os.Lstat(sockPath); err == nil {
			if fi.Mode()&os.ModeSocket == 0 {
				return nil, nil, fmt.Errorf("extsdk: %s exists and is not a socket", sockPath)
			}
			probe, dialErr := net.DialTimeout("unix", sockPath, 500*time.Millisecond)
			if dialErr == nil {
				_ = probe.Close()
				return nil, nil, fmt.Errorf("extsdk: socket %s already in use", sockPath)
			}
			if !errors.Is(dialErr, syscall.ECONNREFUSED) {
				return nil, nil, fmt.Errorf("extsdk: probe unix socket %s: %w", sockPath, dialErr)
			}
			if err := os.Remove(sockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, nil, fmt.Errorf("extsdk: remove stale socket %s: %w", sockPath, err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("extsdk: stat unix socket %s: %w", sockPath, err)
		}

		ln, err := net.Listen("unix", sockPath)
		if err != nil {
			return nil, nil, fmt.Errorf("extsdk: listen unix %s: %w", sockPath, err)
		}
		if err := os.Chmod(sockPath, 0o600); err != nil {
			_ = ln.Close()
			_ = os.Remove(sockPath)
			return nil, nil, fmt.Errorf("extsdk: chmod unix socket %s: %w", sockPath, err)
		}
		cleanup := func() {
			_ = os.Remove(sockPath)
		}
		return ln, cleanup, nil
	}

	tcpAddr := strings.TrimPrefix(addr, "tcp://")
	ln, err := net.Listen("tcp", tcpAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("extsdk: listen tcp %s: %w", tcpAddr, err)
	}
	return ln, nil, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{
		"code":    code,
		"message": msg,
	})
}
