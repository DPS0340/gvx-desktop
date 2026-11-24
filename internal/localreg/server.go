package localreg

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

// Server is an embedded, disk-persistent, loopback-only OCI Distribution v1.1
// / Docker Registry HTTP API V2 server.
type Server struct {
	dir      string
	cfgPort  int
	onChange func()

	// lifecycleMu serializes Start and Stop transitions.
	lifecycleMu sync.Mutex

	// srvMu guards the running state and bound address fields.
	srvMu         sync.Mutex
	running       bool
	httpSrv       *http.Server
	boundAddr     string
	boundPort     int
	serveDone     chan struct{}
	closeNewConns func()

	// mu guards persistent storage mutations and consistent reads/GC.
	mu sync.RWMutex

	// uploadMu guards the active in-progress upload session map.
	uploadMu sync.Mutex
	uploads  map[string]*uploadSession
}

// New constructs a stopped local OCI registry Server using cfg.
func New(cfg Config) *Server {
	return &Server{
		dir:      cfg.Dir,
		cfgPort:  cfg.Port,
		onChange: cfg.OnChange,
		uploads:  make(map[string]*uploadSession),
	}
}

// Start creates the storage directory (0700), sweeps stale upload staging files,
// binds 127.0.0.1:<port>, and begins serving requests. Start is idempotent if
// the server is already running.
func (s *Server) Start() error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	s.srvMu.Lock()
	if s.running {
		s.srvMu.Unlock()
		return nil
	}
	s.srvMu.Unlock()

	if s.dir == "" {
		return errNoDir
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(s.uploadsDir(), 0o700); err != nil {
		return err
	}
	if err := s.sweepStaleFiles(); err != nil {
		return err
	}

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
		return fmt.Errorf("localreg: unexpected listener address %T", ln.Addr())
	}

	var newConnsMu sync.Mutex
	newConns := make(map[net.Conn]struct{})

	httpSrv := &http.Server{
		Handler:           s.Handler(),
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

// Stop gracefully shuts down the HTTP server, closes and removes active upload
// staging files, and releases the listening port. Stop is idempotent.
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
	s.closeAllUploads()
	return err
}

// Running reports whether the registry server is currently listening.
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

// Dir returns the configured persistent storage directory.
func (s *Server) Dir() string {
	return s.dir
}

// Handler returns the HTTP handler serving OCI Distribution v1.1 / Registry V2 endpoints.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.serveHTTP)
}

// notifyChange invokes Config.OnChange if set. Must only be called with no locks held.
func (s *Server) notifyChange() {
	if s.onChange != nil {
		s.onChange()
	}
}
