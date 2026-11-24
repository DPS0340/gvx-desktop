package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const serverInstructions = "GVX Desktop MCP server for managing gVisor sandboxes, checkpoint/restore snapshots, Docker containers, container images, and *.localhost HTTPS routes. Sandboxes created via create_sandbox enable checkpointing and start automatically by default so you can immediately execute commands and snapshot/rollback state."

// Server is an embedded, loopback-only Streamable HTTP MCP server for GVX Desktop.
type Server struct {
	cfgPort    int
	version    string
	backend    Backend
	onActivity func(Stats)
	mcpSrv     *mcp.Server

	// lifecycleMu serializes Start and Stop transitions.
	lifecycleMu sync.Mutex

	// srvMu guards running state, bound address fields, and the active HTTP handler.
	srvMu         sync.Mutex
	running       bool
	httpSrv       *http.Server
	handler       http.Handler
	boundAddr     string
	boundPort     int
	serveDone     chan struct{}
	closeNewConns func()

	// statsMu guards live tool activity statistics.
	statsMu sync.Mutex
	stats   Stats
}

// New constructs a stopped MCP Server configured by cfg.
func New(cfg Config) *Server {
	version := strings.TrimSpace(cfg.Version)
	if version == "" {
		version = "dev"
	}

	s := &Server{
		cfgPort:    cfg.Port,
		version:    version,
		backend:    cfg.Backend,
		onActivity: cfg.OnActivity,
	}

	s.mcpSrv = mcp.NewServer(
		&mcp.Implementation{
			Name:        "gvx-desktop",
			Title:       "GVX Desktop",
			Description: "Built-in GVX Desktop AI Agent MCP server for sandboxes, snapshots, containers, images, and local routes.",
			Version:     version,
		},
		&mcp.ServerOptions{
			Instructions: serverInstructions,
		},
	)
	s.registerTools(s.mcpSrv)
	s.handler = s.newHTTPHandler()
	return s
}

func (s *Server) newHTTPHandler() http.Handler {
	mux := http.NewServeMux()

	// StreamableHTTPOptions{} keeps DisableLocalhostProtection == false so the
	// SDK rejects non-localhost Host headers with 403 Forbidden (DNS rebinding protection).
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return s.mcpSrv
	}, &mcp.StreamableHTTPOptions{})

	cop := http.NewCrossOriginProtection()
	mux.Handle("/mcp", cop.Handler(streamable))
	mux.HandleFunc("/healthz", s.handleHealthz)
	return mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_ = json.NewEncoder(w).Encode(struct {
		Status  string `json:"status"`
		Server  string `json:"server"`
		Version string `json:"version"`
	}{
		Status:  "ok",
		Server:  "gvx-desktop",
		Version: s.version,
	})
}

// Start binds strictly to 127.0.0.1:<port> and starts serving MCP Streamable HTTP
// at /mcp and health checks at /healthz. Start is idempotent when already running.
func (s *Server) Start() error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	s.srvMu.Lock()
	if s.running {
		s.srvMu.Unlock()
		return nil
	}
	s.srvMu.Unlock()

	port := s.cfgPort
	switch {
	case port == 0:
		port = DefaultPort
	case port < 0:
		port = 0
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		return fmt.Errorf("mcpserver: unexpected listener address %T", ln.Addr())
	}

	var newConnsMu sync.Mutex
	newConns := make(map[net.Conn]struct{})

	handler := s.newHTTPHandler()
	httpSrv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		ConnState: func(c net.Conn, st http.ConnState) {
			newConnsMu.Lock()
			if st == http.StateNew {
				newConns[c] = struct{}{}
			} else {
				delete(newConns, c)
			}
			newConnsMu.Unlock()
		},
	}
	closeNewConns := func() {
		newConnsMu.Lock()
		now := time.Now()
		for c := range newConns {
			_ = c.SetReadDeadline(now)
		}
		newConnsMu.Unlock()
	}
	done := make(chan struct{})

	s.srvMu.Lock()
	s.httpSrv = httpSrv
	s.handler = handler
	s.boundPort = tcpAddr.Port
	s.boundAddr = fmt.Sprintf("127.0.0.1:%d", tcpAddr.Port)
	s.serveDone = done
	s.closeNewConns = closeNewConns
	s.running = true
	s.srvMu.Unlock()

	go func() {
		defer close(done)
		_ = httpSrv.Serve(ln)
	}()

	return nil
}

// Stop gracefully shuts down active MCP sessions and the HTTP listener,
// releasing the bound port. Stop is idempotent.
func (s *Server) Stop(ctx context.Context) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	s.srvMu.Lock()
	if !s.running {
		s.srvMu.Unlock()
		return nil
	}
	httpSrv := s.httpSrv
	done := s.serveDone
	closeNewConns := s.closeNewConns
	s.running = false
	s.httpSrv = nil
	s.boundAddr = ""
	s.boundPort = 0
	s.serveDone = nil
	s.closeNewConns = nil
	s.srvMu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}

	// Gracefully close any active MCP sessions so long-lived standalone SSE
	// GET /mcp streams wind down cleanly during HTTP shutdown.
	for ss := range s.mcpSrv.Sessions() {
		go func(sess *mcp.ServerSession) {
			_ = sess.Close()
		}(ss)
	}

	// Go's http.Server.Shutdown waits 5+ seconds before closing idle StateNew
	// connections (e.g. speculative connections dialed by http.Transport).
	// Expire silent pre-shutdown StateNew connections after a short loopback grace.
	stopReaper := make(chan struct{})
	reaperDone := make(chan struct{})
	go func() {
		defer close(reaperDone)
		timer := time.NewTimer(100 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-stopReaper:
			return
		case <-ctx.Done():
			return
		case <-timer.C:
			if closeNewConns != nil {
				closeNewConns()
			}
		}
	}()

	err := httpSrv.Shutdown(ctx)
	close(stopReaper)
	<-reaperDone
	if err != nil {
		_ = httpSrv.Close()
	}
	<-done
	return err
}

// Running reports whether the MCP server is currently listening.
func (s *Server) Running() bool {
	s.srvMu.Lock()
	defer s.srvMu.Unlock()
	return s.running
}

// Addr returns the bound "127.0.0.1:<port>" while running, or "" when stopped.
func (s *Server) Addr() string {
	s.srvMu.Lock()
	defer s.srvMu.Unlock()
	if !s.running {
		return ""
	}
	return s.boundAddr
}

// Port returns the actual bound TCP port while running, or the configured port
// (DefaultPort when Config.Port == 0, or 0 when Config.Port < 0) when stopped.
func (s *Server) Port() int {
	s.srvMu.Lock()
	defer s.srvMu.Unlock()
	if s.running {
		return s.boundPort
	}
	if s.cfgPort == 0 {
		return DefaultPort
	}
	if s.cfgPort < 0 {
		return 0
	}
	return s.cfgPort
}

// EndpointURL returns "http://127.0.0.1:<port>/mcp" using the bound port when
// running, or the configured/default port when stopped.
func (s *Server) EndpointURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/mcp", s.Port())
}

// Stats returns a snapshot of the server's live tool call activity.
func (s *Server) Stats() Stats {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	return s.stats
}

// Handler returns the HTTP handler serving /mcp and /healthz.
func (s *Server) Handler() http.Handler {
	s.srvMu.Lock()
	defer s.srvMu.Unlock()
	return s.handler
}

func (s *Server) recordCall(toolName string, err error) {
	s.statsMu.Lock()
	s.stats.CallCount++
	s.stats.LastTool = toolName
	s.stats.LastCalledAt = time.Now()
	if err != nil {
		s.stats.LastErr = err.Error()
	} else {
		s.stats.LastErr = ""
	}
	snap := s.stats
	cb := s.onActivity
	s.statsMu.Unlock()

	if cb != nil {
		cb(snap)
	}
}

// ClientConfigJSON returns a formatted MCP client configuration snippet
// pointing to endpoint (defaulting to http://127.0.0.1:18787/mcp if empty).
func ClientConfigJSON(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		endpoint = fmt.Sprintf("http://127.0.0.1:%d/mcp", DefaultPort)
	}
	type serverEntry struct {
		URL string `json:"url"`
	}
	type configDoc struct {
		MCPServers map[string]serverEntry `json:"mcpServers"`
	}
	data, err := json.MarshalIndent(configDoc{
		MCPServers: map[string]serverEntry{
			"gvx-desktop": {URL: endpoint},
		},
	}, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(data)
}

// ClaudeAddCommand returns the Claude Code CLI command for registering this
// MCP server over Streamable HTTP.
func ClaudeAddCommand(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		endpoint = fmt.Sprintf("http://127.0.0.1:%d/mcp", DefaultPort)
	}
	return "claude mcp add --transport http gvx-desktop " + endpoint
}
