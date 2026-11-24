package localproxy

import (
	"fmt"
	"strings"
)

const (
	// DefaultHTTPPort is the default loopback port for HTTP reverse proxying.
	DefaultHTTPPort = 18080
	// DefaultHTTPSPort is the default loopback port for HTTPS reverse proxying.
	DefaultHTTPSPort = 18443
)

// Config configures a local HTTPS / *.localhost reverse proxy Server.
type Config struct {
	// Dir is the persistent directory for the local dev CA (ca.crt, ca.key),
	// e.g. <dataDir>/localproxy.
	Dir string
	// HTTPPort is the loopback TCP port for HTTP.
	// 0 selects DefaultHTTPPort (18080); a negative value binds an ephemeral
	// 127.0.0.1:0 port (useful for tests).
	HTTPPort int
	// HTTPSPort is the loopback TCP port for HTTPS.
	// 0 selects DefaultHTTPSPort (18443); a negative value binds an ephemeral
	// 127.0.0.1:0 port (useful for tests).
	HTTPSPort int
}

// Route maps a local *.localhost hostname to an upstream host:port target.
type Route struct {
	// Host is the normalized lowercase hostname without port
	// (e.g. "web.localhost", "api.myproj.localhost").
	Host string `json:"host"`
	// Target is the upstream "host:port" (e.g. "127.0.0.1:8080").
	Target string `json:"target"`
	// Kind identifies the route source: "container", "sandbox", or "k8s".
	Kind string `json:"kind"`
	// Name is a human-friendly source name (e.g. "web", "svc/frontend").
	Name string `json:"name"`
	// GuestPort is the optional guest/service port for display and sorting.
	GuestPort int `json:"guestPort"`
}

// Equal reports whether r and o have identical field values.
func (r Route) Equal(o Route) bool {
	return r == o
}

// SanitizeLabel converts an arbitrary container, sandbox, or service name into
// a valid RFC 1123 DNS label: lowercase ASCII [a-z0-9-], replacing invalid
// runes with '-', collapsing consecutive '-', trimming leading/trailing '-',
// and truncating to 63 bytes (re-trimming any trailing '-'). Returns "" if no
// valid characters remain.
func SanitizeLabel(s string) string {
	if s == "" {
		return ""
	}
	buf := make([]byte, 0, len(s))
	lastDash := false
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			buf = append(buf, byte(r-'A'+'a'))
			lastDash = false
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			buf = append(buf, byte(r))
			lastDash = false
		default:
			if len(buf) > 0 && !lastDash {
				buf = append(buf, '-')
				lastDash = true
			}
		}
	}
	for len(buf) > 0 && buf[len(buf)-1] == '-' {
		buf = buf[:len(buf)-1]
	}
	if len(buf) > 63 {
		buf = buf[:63]
		for len(buf) > 0 && buf[len(buf)-1] == '-' {
			buf = buf[:len(buf)-1]
		}
	}
	return string(buf)
}

// BuildHost sanitizes each non-empty label, joins them with ".", and appends
// ".localhost". Returns "" if no valid labels remain.
func BuildHost(labels ...string) string {
	cleaned := make([]string, 0, len(labels)+1)
	for _, raw := range labels {
		if label := SanitizeLabel(raw); label != "" {
			cleaned = append(cleaned, label)
		}
	}
	if len(cleaned) == 0 {
		return ""
	}
	cleaned = append(cleaned, "localhost")
	return strings.Join(cleaned, ".")
}

// FormatURL formats an HTTP or HTTPS URL for host and port, omitting the
// default port (:80 for http, :443 for https).
func FormatURL(scheme, host string, port int) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	scheme = strings.ToLower(strings.TrimSpace(scheme))
	if scheme == "" {
		scheme = "http"
	}
	if port <= 0 || (scheme == "http" && port == 80) || (scheme == "https" && port == 443) {
		return fmt.Sprintf("%s://%s", scheme, host)
	}
	return fmt.Sprintf("%s://%s:%d", scheme, host, port)
}
