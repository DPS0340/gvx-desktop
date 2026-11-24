package localproxy

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type routeTable struct {
	byHost map[string]Route
	sorted []Route
}

// Server is an embedded, loopback-only local HTTP and HTTPS reverse proxy that
// routes *.localhost hostnames to local upstream targets using a persisted
// local development CA and dynamically minted leaf certificates.
type Server struct {
	dir          string
	cfgHTTPPort  int
	cfgHTTPSPort int

	// lifecycleMu serializes Start and Stop transitions.
	lifecycleMu sync.Mutex

	// srvMu guards running state and bound server fields.
	srvMu          sync.Mutex
	running        bool
	httpSrv        *http.Server
	httpsSrv       *http.Server
	boundHTTPAddr  string
	boundHTTPSAddr string
	boundHTTPPort  int
	boundHTTPSPort int
	httpDone       chan struct{}
	httpsDone      chan struct{}
	closeNewConns  func()

	// routes stores an immutable snapshot of active routes for lock-free lookups.
	routes atomic.Pointer[routeTable]

	// transport is shared across reverse proxy requests to local upstreams.
	transport *http.Transport
	discard   *log.Logger
}

// New constructs a stopped localproxy Server using cfg.
func New(cfg Config) *Server {
	s := &Server{
		dir:          cfg.Dir,
		cfgHTTPPort:  cfg.HTTPPort,
		cfgHTTPSPort: cfg.HTTPSPort,
		transport: &http.Transport{
			Proxy: nil, // Never send loopback traffic through system HTTP_PROXY.
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:          128,
			MaxIdleConnsPerHost:   32,
			IdleConnTimeout:       30 * time.Second,
			ResponseHeaderTimeout: 0, // Allow long-running/streaming dev requests.
		},
		discard: log.New(io.Discard, "", 0),
	}
	s.routes.Store(&routeTable{
		byHost: make(map[string]Route),
		sorted: []Route{},
	})
	return s
}

// Start ensures the local dev CA exists, binds loopback HTTP and HTTPS
// listeners on 127.0.0.1, and begins serving requests. Start is idempotent if
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

	_, _, caCert, caKey, err := EnsureCA(s.dir)
	if err != nil {
		return err
	}
	cache := newCertCache(caCert, caKey)

	httpPort := resolveBindPort(s.cfgHTTPPort, DefaultHTTPPort)
	httpsPort := resolveBindPort(s.cfgHTTPSPort, DefaultHTTPSPort)

	httpLn, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", httpPort))
	if err != nil {
		return fmt.Errorf("localproxy: listen http on 127.0.0.1:%d: %w", httpPort, err)
	}
	httpTCPAddr, ok := httpLn.Addr().(*net.TCPAddr)
	if !ok {
		_ = httpLn.Close()
		return fmt.Errorf("localproxy: unexpected http listener address %T", httpLn.Addr())
	}

	httpsRawLn, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", httpsPort))
	if err != nil {
		_ = httpLn.Close()
		return fmt.Errorf("localproxy: listen https on 127.0.0.1:%d: %w", httpsPort, err)
	}
	httpsTCPAddr, ok := httpsRawLn.Addr().(*net.TCPAddr)
	if !ok {
		_ = httpsRawLn.Close()
		_ = httpLn.Close()
		return fmt.Errorf("localproxy: unexpected https listener address %T", httpsRawLn.Addr())
	}

	tlsCfg := &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: cache.GetCertificate,
	}
	httpsLn := tls.NewListener(httpsRawLn, tlsCfg)

	var newConnsMu sync.Mutex
	newConns := make(map[net.Conn]struct{})
	connState := func(c net.Conn, st http.ConnState) {
		newConnsMu.Lock()
		if st == http.StateNew {
			newConns[c] = struct{}{}
		} else {
			delete(newConns, c)
		}
		newConnsMu.Unlock()
	}
	closeNewConns := func() {
		newConnsMu.Lock()
		now := time.Now()
		for c := range newConns {
			_ = c.SetReadDeadline(now)
		}
		newConnsMu.Unlock()
	}

	handler := http.HandlerFunc(s.serveHTTP)
	httpSrv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		ConnState:         connState,
		ErrorLog:          s.discard,
	}
	httpsSrv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		ConnState:         connState,
		ErrorLog:          s.discard,
	}

	httpDone := make(chan struct{})
	httpsDone := make(chan struct{})

	s.srvMu.Lock()
	s.httpSrv = httpSrv
	s.httpsSrv = httpsSrv
	s.boundHTTPPort = httpTCPAddr.Port
	s.boundHTTPSPort = httpsTCPAddr.Port
	s.boundHTTPAddr = fmt.Sprintf("127.0.0.1:%d", httpTCPAddr.Port)
	s.boundHTTPSAddr = fmt.Sprintf("127.0.0.1:%d", httpsTCPAddr.Port)
	s.httpDone = httpDone
	s.httpsDone = httpsDone
	s.closeNewConns = closeNewConns
	s.running = true
	s.srvMu.Unlock()

	go func() {
		defer close(httpDone)
		_ = httpSrv.Serve(httpLn)
	}()
	go func() {
		defer close(httpsDone)
		_ = httpsSrv.Serve(httpsLn)
	}()

	return nil
}

// Stop gracefully shuts down both the HTTP and HTTPS servers and releases
// their listening ports. Stop is idempotent.
func (s *Server) Stop(ctx context.Context) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	s.srvMu.Lock()
	if !s.running {
		s.srvMu.Unlock()
		return nil
	}
	httpSrv := s.httpSrv
	httpsSrv := s.httpsSrv
	httpDone := s.httpDone
	httpsDone := s.httpsDone
	closeNewConns := s.closeNewConns
	s.running = false
	s.httpSrv = nil
	s.httpsSrv = nil
	s.boundHTTPAddr = ""
	s.boundHTTPSAddr = ""
	s.boundHTTPPort = 0
	s.boundHTTPSPort = 0
	s.httpDone = nil
	s.httpsDone = nil
	s.closeNewConns = nil
	s.srvMu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}

	// Go's http.Server.Shutdown waits 5+ seconds before closing idle StateNew
	// connections (e.g. speculative connections dialed by http.Transport or
	// browsers). Expire silent pre-shutdown StateNew connections after a short
	// loopback grace so Stop always completes promptly.
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

	var wg sync.WaitGroup
	var httpErr, httpsErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		httpErr = httpSrv.Shutdown(ctx)
	}()
	go func() {
		defer wg.Done()
		httpsErr = httpsSrv.Shutdown(ctx)
	}()
	wg.Wait()

	close(stopReaper)
	<-reaperDone

	if httpErr != nil {
		_ = httpSrv.Close()
	}
	if httpsErr != nil {
		_ = httpsSrv.Close()
	}

	<-httpDone
	<-httpsDone
	s.transport.CloseIdleConnections()

	return errors.Join(httpErr, httpsErr)
}

// Running reports whether the proxy server is currently listening.
func (s *Server) Running() bool {
	s.srvMu.Lock()
	defer s.srvMu.Unlock()
	return s.running
}

// HTTPAddr returns the bound HTTP "127.0.0.1:<port>" while running, or "" when stopped.
func (s *Server) HTTPAddr() string {
	s.srvMu.Lock()
	defer s.srvMu.Unlock()
	if !s.running {
		return ""
	}
	return s.boundHTTPAddr
}

// HTTPSAddr returns the bound HTTPS "127.0.0.1:<port>" while running, or "" when stopped.
func (s *Server) HTTPSAddr() string {
	s.srvMu.Lock()
	defer s.srvMu.Unlock()
	if !s.running {
		return ""
	}
	return s.boundHTTPSAddr
}

// HTTPPort returns the bound HTTP port while running, or the configured/default
// port when stopped (DefaultHTTPPort when Config.HTTPPort == 0, or 0 when < 0).
func (s *Server) HTTPPort() int {
	s.srvMu.Lock()
	defer s.srvMu.Unlock()
	if s.running {
		return s.boundHTTPPort
	}
	return resolveBindPort(s.cfgHTTPPort, DefaultHTTPPort)
}

// HTTPSPort returns the bound HTTPS port while running, or the configured/default
// port when stopped (DefaultHTTPSPort when Config.HTTPSPort == 0, or 0 when < 0).
func (s *Server) HTTPSPort() int {
	s.srvMu.Lock()
	defer s.srvMu.Unlock()
	if s.running {
		return s.boundHTTPSPort
	}
	return resolveBindPort(s.cfgHTTPSPort, DefaultHTTPSPort)
}

// CAPath returns the path to the persisted local dev CA certificate (ca.crt),
// or "" if Config.Dir is empty.
func (s *Server) CAPath() string {
	if strings.TrimSpace(s.dir) == "" {
		return ""
	}
	return filepath.Join(s.dir, caCertFilename)
}

// TrustCommand returns the platform-appropriate command for trusting the
// server's local development CA certificate, or "" if Config.Dir is empty.
func (s *Server) TrustCommand() string {
	caPath := s.CAPath()
	if caPath == "" {
		return ""
	}
	return TrustCommand(caPath)
}

// HTTPURL formats the HTTP URL for host using the server's active/configured HTTP port.
func (s *Server) HTTPURL(host string) string {
	return FormatURL("http", host, s.HTTPPort())
}

// HTTPSURL formats the HTTPS URL for host using the server's active/configured HTTPS port.
func (s *Server) HTTPSURL(host string) string {
	return FormatURL("https", host, s.HTTPSPort())
}

// SetRoutes atomically replaces the active route table with routes.
// Hostnames are normalized to lowercase without ports, and a deterministically
// sorted copy is retained for Routes() and introspection responses.
func (s *Server) SetRoutes(routes []Route) {
	byHost := make(map[string]Route, len(routes))
	for _, rt := range routes {
		host := normalizeHost(rt.Host)
		target := strings.TrimSpace(rt.Target)
		if host == "" || target == "" {
			continue
		}
		rt.Host = host
		rt.Target = target
		byHost[host] = rt
	}

	sorted := make([]Route, 0, len(byHost))
	for _, rt := range byHost {
		sorted = append(sorted, rt)
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Host < sorted[j].Host
	})

	s.routes.Store(&routeTable{
		byHost: byHost,
		sorted: sorted,
	})
}

// Routes returns a deterministically sorted copy of the active routes.
func (s *Server) Routes() []Route {
	tbl := s.routes.Load()
	if tbl == nil || len(tbl.sorted) == 0 {
		return []Route{}
	}
	return slices.Clone(tbl.sorted)
}

type routeInfo struct {
	Host      string `json:"host"`
	Target    string `json:"target"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	GuestPort int    `json:"guestPort"`
	HTTPURL   string `json:"httpUrl"`
	HTTPSURL  string `json:"httpsUrl"`
}

func (s *Server) buildRouteInfos(routes []Route) []routeInfo {
	httpPort := s.HTTPPort()
	httpsPort := s.HTTPSPort()
	out := make([]routeInfo, 0, len(routes))
	for _, rt := range routes {
		out = append(out, routeInfo{
			Host:      rt.Host,
			Target:    rt.Target,
			Kind:      rt.Kind,
			Name:      rt.Name,
			GuestPort: rt.GuestPort,
			HTTPURL:   FormatURL("http", rt.Host, httpPort),
			HTTPSURL:  FormatURL("https", rt.Host, httpsPort),
		})
	}
	return out
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	host := normalizeHost(r.Host)

	if (host == "localhost" || host == "127.0.0.1") && r.URL.Path == "/_gvx/routes" {
		s.handleRoutesJSON(w)
		return
	}

	tbl := s.routes.Load()
	if tbl != nil {
		if rt, ok := tbl.byHost[host]; ok {
			s.proxyRequest(w, r, rt)
			return
		}
	}

	s.handleNotFound(w, r, host, tbl)
}

func (s *Server) handleRoutesJSON(w http.ResponseWriter) {
	routes := s.Routes()
	resp := struct {
		Routes []routeInfo `json:"routes"`
	}{
		Routes: s.buildRouteInfos(routes),
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) proxyRequest(w http.ResponseWriter, r *http.Request, rt Route) {
	targetURL := &url.URL{
		Scheme: "http",
		Host:   rt.Target,
	}
	proxy := &httputil.ReverseProxy{
		Transport:     s.transport,
		FlushInterval: -1,
		ErrorLog:      s.discard,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(targetURL)
			pr.SetXForwarded()
			// Preserve the incoming Host header so backend applications know
			// their *.localhost hostname.
			pr.Out.Host = pr.In.Host
			if pr.In.TLS != nil {
				pr.Out.Header.Set("X-Forwarded-Proto", "https")
			} else {
				pr.Out.Header.Set("X-Forwarded-Proto", "http")
			}
		},
		ErrorHandler: func(rw http.ResponseWriter, _ *http.Request, err error) {
			rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
			rw.WriteHeader(http.StatusBadGateway)
			_, _ = fmt.Fprintf(rw, "502 Bad Gateway: upstream %s (%s) unavailable: %v\n", rt.Host, rt.Target, err)
		},
	}
	proxy.ServeHTTP(w, r)
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request, host string, tbl *routeTable) {
	var routes []Route
	if tbl != nil {
		routes = tbl.sorted
	}
	infos := s.buildRouteInfos(routes)

	if strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		var b strings.Builder
		b.WriteString("<!doctype html>\n<html lang=\"en\"><head><meta charset=\"utf-8\">")
		b.WriteString("<title>404 Route Not Found — GVX Local Proxy</title>")
		b.WriteString("<style>body{font-family:system-ui,-apple-system,sans-serif;max-width:720px;margin:2.5rem auto;padding:0 1rem;color:#1f2937}h1{font-size:1.25rem}code{background:#f3f4f6;padding:.15rem .35rem;border-radius:4px}ul{padding-left:1.25rem}li{margin:.5rem 0}a{color:#2563eb;text-decoration:none}a:hover{text-decoration:underline}.meta{color:#6b7280;font-size:.875rem}</style>")
		b.WriteString("</head><body>")
		fmt.Fprintf(&b, "<h1>Route not found: <code>%s</code></h1>", html.EscapeString(host))
		if len(infos) == 0 {
			b.WriteString("<p class=\"meta\">No active local proxy routes are registered.</p>")
		} else {
			b.WriteString("<p>Active local routes:</p><ul>")
			for _, info := range infos {
				fmt.Fprintf(
					&b,
					"<li><strong>%s</strong> (<a href=\"%s\">HTTPS</a> · <a href=\"%s\">HTTP</a>) <span class=\"meta\">→ %s",
					html.EscapeString(info.Host),
					html.EscapeString(info.HTTPSURL),
					html.EscapeString(info.HTTPURL),
					html.EscapeString(info.Target),
				)
				if info.Kind != "" || info.Name != "" {
					fmt.Fprintf(&b, " [%s %s]", html.EscapeString(info.Kind), html.EscapeString(info.Name))
				}
				b.WriteString("</span></li>")
			}
			b.WriteString("</ul>")
		}
		b.WriteString("</body></html>\n")
		_, _ = io.WriteString(w, b.String())
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(struct {
		Error  string      `json:"error"`
		Host   string      `json:"host"`
		Routes []routeInfo `json:"routes"`
	}{
		Error:  "route not found",
		Host:   host,
		Routes: infos,
	})
}

func resolveBindPort(cfgPort, defaultPort int) int {
	switch {
	case cfgPort == 0:
		return defaultPort
	case cfgPort < 0:
		return 0
	default:
		return cfgPort
	}
}

func normalizeHost(raw string) string {
	h := strings.TrimSpace(raw)
	if h == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	} else if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}
