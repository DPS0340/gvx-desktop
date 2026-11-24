package localproxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSanitizeLabel(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"---", ""},
		{"___...///", ""},
		{"web", "web"},
		{"My_App/1.0", "my-app-1-0"},
		{"  Hello   World  ", "hello-world"},
		{"___hello___world___", "hello-world"},
		{"svc/frontend", "svc-frontend"},
		{"UPPER_Case-123", "upper-case-123"},
		{"café", "caf"},
		{strings.Repeat("A", 70), strings.Repeat("a", 63)},
		{strings.Repeat("a", 62) + "-b", strings.Repeat("a", 62)},
		{strings.Repeat("a", 62) + "___b", strings.Repeat("a", 62)},
	}
	for _, tc := range tests {
		if got := SanitizeLabel(tc.in); got != tc.want {
			t.Errorf("SanitizeLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBuildHost(t *testing.T) {
	tests := []struct {
		labels []string
		want   string
	}{
		{nil, ""},
		{[]string{""}, ""},
		{[]string{"---", "   "}, ""},
		{[]string{"web"}, "web.localhost"},
		{[]string{"api", "myproj"}, "api.myproj.localhost"},
		{[]string{"", "API_v2", "My Project", "---"}, "api-v2.my-project.localhost"},
	}
	for _, tc := range tests {
		if got := BuildHost(tc.labels...); got != tc.want {
			t.Errorf("BuildHost(%q) = %q, want %q", tc.labels, got, tc.want)
		}
	}
}

func TestFormatURL(t *testing.T) {
	tests := []struct {
		scheme string
		host   string
		port   int
		want   string
	}{
		{"http", "web.localhost", 80, "http://web.localhost"},
		{"http", "web.localhost", 18080, "http://web.localhost:18080"},
		{"https", "web.localhost", 443, "https://web.localhost"},
		{"https", "web.localhost", 18443, "https://web.localhost:18443"},
		{"HTTP", "web.localhost", 80, "http://web.localhost"},
		{"HTTPS", "web.localhost", 443, "https://web.localhost"},
		{"http", "web.localhost", 0, "http://web.localhost"},
		{"https", "web.localhost", -1, "https://web.localhost"},
		{"http", "", 18080, ""},
	}
	for _, tc := range tests {
		if got := FormatURL(tc.scheme, tc.host, tc.port); got != tc.want {
			t.Errorf("FormatURL(%q, %q, %d) = %q, want %q", tc.scheme, tc.host, tc.port, got, tc.want)
		}
	}
}

func TestRouteEqual(t *testing.T) {
	base := Route{
		Host:      "web.localhost",
		Target:    "127.0.0.1:8080",
		Kind:      "container",
		Name:      "web",
		GuestPort: 80,
	}
	if !base.Equal(base) {
		t.Fatal("expected Route.Equal(self) == true")
	}
	diffs := []Route{
		{Host: "api.localhost", Target: base.Target, Kind: base.Kind, Name: base.Name, GuestPort: base.GuestPort},
		{Host: base.Host, Target: "127.0.0.1:9090", Kind: base.Kind, Name: base.Name, GuestPort: base.GuestPort},
		{Host: base.Host, Target: base.Target, Kind: "k8s", Name: base.Name, GuestPort: base.GuestPort},
		{Host: base.Host, Target: base.Target, Kind: base.Kind, Name: "other", GuestPort: base.GuestPort},
		{Host: base.Host, Target: base.Target, Kind: base.Kind, Name: base.Name, GuestPort: 8080},
	}
	for i, d := range diffs {
		if base.Equal(d) {
			t.Errorf("case %d: expected Equal == false for %+v", i, d)
		}
	}
}

func TestTrustCommand(t *testing.T) {
	path := "/tmp/gvx/localproxy/ca.crt"
	if got := trustCommandForOS("darwin", path); got != `security add-trusted-cert -d -r trustRoot -k ~/Library/Keychains/login.keychain-db "/tmp/gvx/localproxy/ca.crt"` {
		t.Errorf("darwin TrustCommand = %q", got)
	}
	if got := trustCommandForOS("windows", path); got != `certutil -user -addstore Root "/tmp/gvx/localproxy/ca.crt"` {
		t.Errorf("windows TrustCommand = %q", got)
	}
	if got := trustCommandForOS("linux", path); got != `sudo cp "/tmp/gvx/localproxy/ca.crt" /usr/local/share/ca-certificates/gvx-desktop-dev-ca.crt && sudo update-ca-certificates` {
		t.Errorf("linux TrustCommand = %q", got)
	}
	if got := TrustCommand(path); !strings.Contains(got, path) {
		t.Errorf("TrustCommand(%q) = %q, missing cert path", path, got)
	}
}

func TestEnsureCAPersistenceAndRegeneration(t *testing.T) {
	if _, _, _, _, err := EnsureCA(""); err == nil {
		t.Fatal("expected error for empty CA dir")
	}

	dir := t.TempDir()
	certPath1, keyPath1, cert1, key1, err := EnsureCA(dir)
	if err != nil {
		t.Fatalf("EnsureCA first call: %v", err)
	}
	if certPath1 != filepath.Join(dir, "ca.crt") || keyPath1 != filepath.Join(dir, "ca.key") {
		t.Fatalf("unexpected paths: cert=%q key=%q", certPath1, keyPath1)
	}
	if !cert1.IsCA || !cert1.BasicConstraintsValid {
		t.Fatal("generated cert is not marked as CA")
	}
	if cert1.Subject.CommonName != "GVX Desktop Local Dev CA" {
		t.Errorf("CN = %q, want GVX Desktop Local Dev CA", cert1.Subject.CommonName)
	}
	if len(cert1.Subject.Organization) != 1 || cert1.Subject.Organization[0] != "GVX Desktop" {
		t.Errorf("Org = %v, want [GVX Desktop]", cert1.Subject.Organization)
	}
	if len(cert1.SubjectKeyId) == 0 {
		t.Error("expected non-empty SubjectKeyId")
	}
	if cert1.KeyUsage&(x509.KeyUsageCertSign|x509.KeyUsageCRLSign) != (x509.KeyUsageCertSign | x509.KeyUsageCRLSign) {
		t.Errorf("unexpected KeyUsage: %v", cert1.KeyUsage)
	}
	if cert1.NotAfter.Sub(cert1.NotBefore) < 9*365*24*time.Hour {
		t.Errorf("expected ~10-year CA validity, got %v", cert1.NotAfter.Sub(cert1.NotBefore))
	}

	// Second call must reload the exact same CA without regenerating.
	_, _, cert2, key2, err := EnsureCA(dir)
	if err != nil {
		t.Fatalf("EnsureCA second call: %v", err)
	}
	if cert1.SerialNumber.Cmp(cert2.SerialNumber) != 0 {
		t.Fatalf("expected persisted CA serial %v, got %v", cert1.SerialNumber, cert2.SerialNumber)
	}
	if !key1.Equal(key2) {
		t.Fatal("expected persisted CA key to match")
	}

	// Corrupting ca.crt forces clean regeneration.
	if err := os.WriteFile(certPath1, []byte("not-a-cert"), 0o644); err != nil {
		t.Fatalf("corrupt ca.crt: %v", err)
	}
	_, _, cert3, _, err := EnsureCA(dir)
	if err != nil {
		t.Fatalf("EnsureCA after corruption: %v", err)
	}
	if cert1.SerialNumber.Cmp(cert3.SerialNumber) == 0 {
		t.Fatal("expected regenerated CA with new serial after corruption")
	}
}

func TestCertCacheValidationAndEviction(t *testing.T) {
	_, _, caCert, caKey, err := EnsureCA(t.TempDir())
	if err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}
	cache := newCertCache(caCert, caKey)
	cache.maxEntries = 3

	// Empty SNI falls back to localhost.
	c0, err := cache.GetCertificate(&tls.ClientHelloInfo{ServerName: ""})
	if err != nil {
		t.Fatalf("empty SNI: %v", err)
	}
	c0Again, err := cache.GetCertificate(&tls.ClientHelloInfo{ServerName: "LOCALHOST."})
	if err != nil {
		t.Fatalf("LOCALHOST.: %v", err)
	}
	if c0 != c0Again {
		t.Fatal("expected cached certificate reuse for localhost")
	}

	// Reject non-localhost SNIs.
	for _, bad := range []string{
		"example.com",
		"evil.localhost.com",
		".localhost",
		"a..localhost",
		"bad_host.localhost",
		"-leading.localhost",
		"trailing-.localhost",
		"127.0.0.1",
	} {
		if _, err := cache.GetCertificate(&tls.ClientHelloInfo{ServerName: bad}); err == nil {
			t.Errorf("expected error for non-localhost SNI %q, got nil", bad)
		}
	}

	// Mint enough certs to trigger cache eviction.
	for _, name := range []string{"a.localhost", "b.localhost", "c.localhost", "d.localhost"} {
		if _, err := cache.GetCertificate(&tls.ClientHelloInfo{ServerName: name}); err != nil {
			t.Fatalf("GetCertificate(%q): %v", name, err)
		}
	}
	cache.mu.RLock()
	size := len(cache.certs)
	cache.mu.RUnlock()
	if size > cache.maxEntries {
		t.Fatalf("cache size %d exceeded maxEntries %d", size, cache.maxEntries)
	}
}

func newLocalProxyTestClient(t *testing.T, caPath string) *http.Client {
	t.Helper()
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatalf("read CA cert %q: %v", caPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatalf("failed to parse CA cert PEM")
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				RootCAs:    pool,
			},
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err == nil && (host == "localhost" || strings.HasSuffix(host, ".localhost")) {
					addr = net.JoinHostPort("127.0.0.1", port)
				}
				return dialer.DialContext(ctx, network, addr)
			},
		},
	}
}

type echoPayload struct {
	Host            string `json:"host"`
	XForwardedProto string `json:"xForwardedProto"`
	XForwardedHost  string `json:"xForwardedHost"`
	XForwardedFor   string `json:"xForwardedFor"`
	Path            string `json:"path"`
	Query           string `json:"query"`
	Tag             string `json:"tag"`
}

func newEchoUpstream(tag string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(echoPayload{
			Host:            r.Host,
			XForwardedProto: r.Header.Get("X-Forwarded-Proto"),
			XForwardedHost:  r.Header.Get("X-Forwarded-Host"),
			XForwardedFor:   r.Header.Get("X-Forwarded-For"),
			Path:            r.URL.Path,
			Query:           r.URL.RawQuery,
			Tag:             tag,
		})
	}))
}

func TestEndToEndHTTPAndHTTPSProxy(t *testing.T) {
	upstreamA := newEchoUpstream("upstream-a")
	defer upstreamA.Close()
	upstreamB := newEchoUpstream("upstream-b")
	defer upstreamB.Close()

	caDir := t.TempDir()
	srv := New(Config{
		Dir:       caDir,
		HTTPPort:  -1,
		HTTPSPort: -1,
	})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := srv.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	srv.SetRoutes([]Route{
		{
			Host:      "WEB.LOCALHOST",
			Target:    upstreamA.Listener.Addr().String(),
			Kind:      "container",
			Name:      "web",
			GuestPort: 80,
		},
		{
			Host:      "api.myproj.localhost",
			Target:    upstreamB.Listener.Addr().String(),
			Kind:      "k8s",
			Name:      "svc/api",
			GuestPort: 8080,
		},
	})

	client := newLocalProxyTestClient(t, srv.CAPath())
	defer client.CloseIdleConnections()

	// 1. HTTP reverse proxy to web.localhost.
	httpURL := srv.HTTPURL("web.localhost") + "/hello?foo=bar"
	resp, err := client.Get(httpURL)
	if err != nil {
		t.Fatalf("GET %s: %v", httpURL, err)
	}
	var got echoPayload
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		_ = resp.Body.Close()
		t.Fatalf("decode HTTP response: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP status = %d, want 200", resp.StatusCode)
	}
	wantHTTPHost := fmt.Sprintf("web.localhost:%d", srv.HTTPPort())
	if got.Tag != "upstream-a" || got.Path != "/hello" || got.Query != "foo=bar" {
		t.Fatalf("unexpected HTTP echo payload: %+v", got)
	}
	if got.XForwardedProto != "http" {
		t.Errorf("HTTP X-Forwarded-Proto = %q, want http", got.XForwardedProto)
	}
	if got.XForwardedHost != wantHTTPHost {
		t.Errorf("HTTP X-Forwarded-Host = %q, want %q", got.XForwardedHost, wantHTTPHost)
	}
	if got.Host != wantHTTPHost {
		t.Errorf("HTTP upstream Host = %q, want %q", got.Host, wantHTTPHost)
	}

	// 2. HTTPS reverse proxy to web.localhost with real TLS verification against CA.
	httpsWebURL := srv.HTTPSURL("web.localhost") + "/hello"
	resp, err = client.Get(httpsWebURL)
	if err != nil {
		t.Fatalf("GET %s: %v", httpsWebURL, err)
	}
	got = echoPayload{}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		_ = resp.Body.Close()
		t.Fatalf("decode HTTPS web response: %v", err)
	}
	_ = resp.Body.Close()
	wantHTTPSWebHost := fmt.Sprintf("web.localhost:%d", srv.HTTPSPort())
	if got.Tag != "upstream-a" || got.Path != "/hello" {
		t.Fatalf("unexpected HTTPS web payload: %+v", got)
	}
	if got.XForwardedProto != "https" {
		t.Errorf("HTTPS X-Forwarded-Proto = %q, want https", got.XForwardedProto)
	}
	if got.XForwardedHost != wantHTTPSWebHost {
		t.Errorf("HTTPS X-Forwarded-Host = %q, want %q", got.XForwardedHost, wantHTTPSWebHost)
	}
	if got.Host != wantHTTPSWebHost {
		t.Errorf("HTTPS upstream Host = %q, want %q", got.Host, wantHTTPSWebHost)
	}

	// 3. HTTPS reverse proxy to multi-level api.myproj.localhost with real TLS verification.
	httpsAPIURL := srv.HTTPSURL("api.myproj.localhost") + "/hello"
	resp, err = client.Get(httpsAPIURL)
	if err != nil {
		t.Fatalf("GET %s: %v", httpsAPIURL, err)
	}
	got = echoPayload{}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		_ = resp.Body.Close()
		t.Fatalf("decode HTTPS api response: %v", err)
	}
	_ = resp.Body.Close()
	wantHTTPSAPIHost := fmt.Sprintf("api.myproj.localhost:%d", srv.HTTPSPort())
	if got.Tag != "upstream-b" || got.Path != "/hello" {
		t.Fatalf("unexpected HTTPS api payload: %+v", got)
	}
	if got.XForwardedProto != "https" || got.Host != wantHTTPSAPIHost || got.XForwardedHost != wantHTTPSAPIHost {
		t.Errorf("unexpected HTTPS api headers: %+v", got)
	}

	// 4. TLS handshake with non-localhost SNI must be rejected.
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", srv.HTTPSAddr(), &tls.Config{
		ServerName:         "example.com",
		InsecureSkipVerify: true, // Testing server-side SNI rejection.
	})
	if err == nil {
		_ = conn.Close()
		t.Fatal("expected TLS handshake failure for non-localhost SNI example.com")
	}

	// 5. Live SetRoutes updates: replace web.localhost target, then remove it.
	srv.SetRoutes([]Route{
		{
			Host:   "web.localhost",
			Target: upstreamB.Listener.Addr().String(),
			Kind:   "sandbox",
			Name:   "web-v2",
		},
	})
	resp, err = client.Get(httpsWebURL)
	if err != nil {
		t.Fatalf("GET after replace: %v", err)
	}
	got = echoPayload{}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	_ = resp.Body.Close()
	if got.Tag != "upstream-b" {
		t.Fatalf("after route replacement got tag %q, want upstream-b", got.Tag)
	}

	srv.SetRoutes(nil)
	resp, err = client.Get(httpsWebURL)
	if err != nil {
		t.Fatalf("GET after route removal: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status after route removal = %d, want 404", resp.StatusCode)
	}

	// 6. Restarting a new Server on the same CA dir reloads the exact same CA cert.
	caBytesBefore, err := os.ReadFile(srv.CAPath())
	if err != nil {
		t.Fatalf("read CA before restart: %v", err)
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop before restart: %v", err)
	}

	srv2 := New(Config{Dir: caDir, HTTPPort: -1, HTTPSPort: -1})
	if err := srv2.Start(); err != nil {
		t.Fatalf("srv2.Start: %v", err)
	}
	defer func() { _ = srv2.Stop(context.Background()) }()
	caBytesAfter, err := os.ReadFile(srv2.CAPath())
	if err != nil {
		t.Fatalf("read CA after restart: %v", err)
	}
	if string(caBytesBefore) != string(caBytesAfter) {
		t.Fatal("expected persisted CA cert to remain identical across restarts")
	}
	srv2.SetRoutes([]Route{{Host: "web.localhost", Target: upstreamA.Listener.Addr().String()}})
	// Same client trusting original CA must still trust srv2!
	resp, err = client.Get(srv2.HTTPSURL("web.localhost") + "/restarted")
	if err != nil {
		t.Fatalf("GET srv2 HTTPS with original CA trust: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("srv2 HTTPS status = %d, want 200", resp.StatusCode)
	}
}

func TestRoutesEndpointNotFoundAndBadGateway(t *testing.T) {
	upstream := newEchoUpstream("ok")
	defer upstream.Close()

	// Allocate and immediately close a loopback listener to obtain a port that
	// refuses connections for the 502 test.
	deadLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen dead port: %v", err)
	}
	deadAddr := deadLn.Addr().String()
	_ = deadLn.Close()

	srv := New(Config{Dir: t.TempDir(), HTTPPort: -1, HTTPSPort: -1})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Stop(context.Background()) }()

	client := newLocalProxyTestClient(t, srv.CAPath())
	defer client.CloseIdleConnections()

	// Empty routes -> /_gvx/routes returns empty JSON array (not null).
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/_gvx/routes", srv.HTTPPort()))
	if err != nil {
		t.Fatalf("GET /_gvx/routes empty: %v", err)
	}
	rawEmpty, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(rawEmpty), `"routes":[]`) {
		t.Fatalf("expected empty JSON array, got: %s", rawEmpty)
	}

	// Register routes in unsorted order.
	srv.SetRoutes([]Route{
		{Host: "zeta.localhost", Target: deadAddr, Kind: "container", Name: "zeta", GuestPort: 9000},
		{Host: "alpha.localhost", Target: upstream.Listener.Addr().String(), Kind: "k8s", Name: "svc/alpha", GuestPort: 8080},
	})

	routes := srv.Routes()
	if len(routes) != 2 || routes[0].Host != "alpha.localhost" || routes[1].Host != "zeta.localhost" {
		t.Fatalf("Routes() not deterministically sorted: %+v", routes)
	}
	// Mutating returned slice must not affect server state.
	routes[0].Host = "mutated.localhost"
	if srv.Routes()[0].Host != "alpha.localhost" {
		t.Fatal("Routes() did not return a defensive copy")
	}

	// /_gvx/routes over HTTPS on localhost.
	resp, err = client.Get(srv.HTTPSURL("localhost") + "/_gvx/routes")
	if err != nil {
		t.Fatalf("GET https://localhost/_gvx/routes: %v", err)
	}
	var routesPayload struct {
		Routes []struct {
			Host      string `json:"host"`
			Target    string `json:"target"`
			Kind      string `json:"kind"`
			Name      string `json:"name"`
			GuestPort int    `json:"guestPort"`
			HTTPURL   string `json:"httpUrl"`
			HTTPSURL  string `json:"httpsUrl"`
		} `json:"routes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&routesPayload); err != nil {
		_ = resp.Body.Close()
		t.Fatalf("decode /_gvx/routes: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(routesPayload.Routes) != 2 {
		t.Fatalf("unexpected /_gvx/routes response: status=%d len=%d", resp.StatusCode, len(routesPayload.Routes))
	}
	r0 := routesPayload.Routes[0]
	if r0.Host != "alpha.localhost" || r0.Kind != "k8s" || r0.Name != "svc/alpha" || r0.GuestPort != 8080 ||
		r0.HTTPURL != srv.HTTPURL("alpha.localhost") || r0.HTTPSURL != srv.HTTPSURL("alpha.localhost") {
		t.Fatalf("unexpected first route DTO: %+v", r0)
	}

	// 404 JSON response for unknown host.
	req, _ := http.NewRequest(http.MethodGet, srv.HTTPURL("missing.localhost")+"/", nil)
	req.Header.Set("Accept", "application/json")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("GET unknown JSON: %v", err)
	}
	var notFoundJSON struct {
		Error  string            `json:"error"`
		Host   string            `json:"host"`
		Routes []json.RawMessage `json:"routes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&notFoundJSON); err != nil {
		_ = resp.Body.Close()
		t.Fatalf("decode 404 JSON: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("404 JSON status = %d, want 404", resp.StatusCode)
	}
	if notFoundJSON.Error != "route not found" || notFoundJSON.Host != "missing.localhost" || len(notFoundJSON.Routes) != 2 {
		t.Fatalf("unexpected 404 JSON body: %+v", notFoundJSON)
	}

	// 404 HTML response for browser Accept header.
	req, _ = http.NewRequest(http.MethodGet, srv.HTTPURL("missing.localhost")+"/", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("GET unknown HTML: %v", err)
	}
	htmlBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("404 HTML status = %d, want 404", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Errorf("404 HTML Content-Type = %q", resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(string(htmlBody), srv.HTTPSURL("alpha.localhost")) || !strings.Contains(string(htmlBody), "missing.localhost") {
		t.Errorf("404 HTML missing expected links/host: %s", htmlBody)
	}

	// 502 Bad Gateway for unreachable upstream.
	resp, err = client.Get(srv.HTTPURL("zeta.localhost") + "/")
	if err != nil {
		t.Fatalf("GET unreachable upstream: %v", err)
	}
	body502, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("502 status = %d, want 502 (body=%q)", resp.StatusCode, body502)
	}
	if !strings.Contains(string(body502), "502 Bad Gateway") {
		t.Errorf("unexpected 502 body: %q", body502)
	}
}

func TestLifecycleIdempotencyAndSecondBindFailure(t *testing.T) {
	// Default ports reported when stopped.
	defSrv := New(Config{Dir: t.TempDir()})
	if defSrv.Running() {
		t.Fatal("new server should not be running")
	}
	if defSrv.HTTPAddr() != "" || defSrv.HTTPSAddr() != "" {
		t.Fatalf("stopped addrs should be empty, got %q / %q", defSrv.HTTPAddr(), defSrv.HTTPSAddr())
	}
	if defSrv.HTTPPort() != DefaultHTTPPort || defSrv.HTTPSPort() != DefaultHTTPSPort {
		t.Fatalf("stopped default ports = %d/%d, want %d/%d", defSrv.HTTPPort(), defSrv.HTTPSPort(), DefaultHTTPPort, DefaultHTTPSPort)
	}
	if defSrv.CAPath() == "" || defSrv.TrustCommand() == "" {
		t.Fatal("expected non-empty CAPath and TrustCommand when Dir is set")
	}

	// Ephemeral server: idempotent Start, idempotent Stop, and restart.
	srv := New(Config{Dir: t.TempDir(), HTTPPort: -1, HTTPSPort: -1})
	if srv.HTTPPort() != 0 || srv.HTTPSPort() != 0 {
		t.Fatalf("stopped ephemeral ports = %d/%d, want 0/0", srv.HTTPPort(), srv.HTTPSPort())
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start #1: %v", err)
	}
	httpPort1, httpsPort1 := srv.HTTPPort(), srv.HTTPSPort()
	if !srv.Running() || httpPort1 <= 0 || httpsPort1 <= 0 {
		t.Fatalf("expected running server with bound ports, got running=%v %d/%d", srv.Running(), httpPort1, httpsPort1)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start #2 (idempotent): %v", err)
	}
	if srv.HTTPPort() != httpPort1 || srv.HTTPSPort() != httpsPort1 {
		t.Fatal("idempotent Start changed bound ports")
	}

	stopStart := time.Now()
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop #1: %v", err)
	}
	if time.Since(stopStart) > 2*time.Second {
		t.Errorf("Stop took too long: %v", time.Since(stopStart))
	}
	if srv.Running() || srv.HTTPAddr() != "" || srv.HTTPSAddr() != "" {
		t.Fatal("server still reports running after Stop")
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop #2 (idempotent): %v", err)
	}

	// Second bind failure must close the first listener cleanly and return error.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen blocker: %v", err)
	}
	defer func() { _ = blocker.Close() }()
	blockedPort := blocker.Addr().(*net.TCPAddr).Port

	failSrv := New(Config{
		Dir:       t.TempDir(),
		HTTPPort:  -1,
		HTTPSPort: blockedPort,
	})
	if err := failSrv.Start(); err == nil {
		_ = failSrv.Stop(context.Background())
		t.Fatal("expected Start to fail when HTTPSPort is already bound")
	}
	if failSrv.Running() || failSrv.HTTPAddr() != "" || failSrv.HTTPSAddr() != "" {
		t.Fatal("failed Start left server in running state")
	}
}

func TestConcurrentRequestsAndRouteUpdates(t *testing.T) {
	upstreamA := newEchoUpstream("a")
	defer upstreamA.Close()
	upstreamB := newEchoUpstream("b")
	defer upstreamB.Close()

	srv := New(Config{Dir: t.TempDir(), HTTPPort: -1, HTTPSPort: -1})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Stop(context.Background()) }()

	srv.SetRoutes([]Route{
		{Host: "app.localhost", Target: upstreamA.Listener.Addr().String(), Kind: "container", Name: "app"},
	})

	client := newLocalProxyTestClient(t, srv.CAPath())
	defer client.CloseIdleConnections()

	var wg sync.WaitGroup
	// Concurrent readers across HTTP, HTTPS, and /_gvx/routes.
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 15; j++ {
				var target string
				switch (id + j) % 3 {
				case 0:
					target = srv.HTTPURL("app.localhost") + "/ping"
				case 1:
					target = srv.HTTPSURL("app.localhost") + "/ping"
				default:
					target = srv.HTTPSURL("localhost") + "/_gvx/routes"
				}
				resp, err := client.Get(target)
				if err != nil {
					t.Errorf("worker %d iter %d GET %s: %v", id, j, target, err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Errorf("worker %d iter %d GET %s: status %d", id, j, target, resp.StatusCode)
					return
				}
			}
		}(i)
	}

	// Concurrent route table swaps while requests are in flight.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 20; j++ {
			target := upstreamA.Listener.Addr().String()
			if j%2 == 1 {
				target = upstreamB.Listener.Addr().String()
			}
			srv.SetRoutes([]Route{
				{Host: "app.localhost", Target: target, Kind: "container", Name: "app"},
				{Host: fmt.Sprintf("sub-%d.localhost", j%4), Target: target, Kind: "sandbox", Name: "sub"},
			})
			_ = srv.Routes()
		}
	}()

	wg.Wait()
}
