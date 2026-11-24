package localproxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	caCertFilename   = "ca.crt"
	caKeyFilename    = "ca.key"
	maxCertCacheSize = 256
)

var (
	errNoDir = errors.New("localproxy: persistent CA directory is required")
)

// EnsureCA loads an existing valid, non-expired local development CA from dir
// (ca.crt and ca.key), or generates and atomically persists a fresh ECDSA P-256
// self-signed root CA if absent, unreadable, mismatched, or expired.
func EnsureCA(dir string) (certPath, keyPath string, caCert *x509.Certificate, caKey *ecdsa.PrivateKey, err error) {
	if strings.TrimSpace(dir) == "" {
		return "", "", nil, nil, errNoDir
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", nil, nil, fmt.Errorf("localproxy: create CA directory: %w", err)
	}

	certPath = filepath.Join(dir, caCertFilename)
	keyPath = filepath.Join(dir, caKeyFilename)

	if cert, key, ok := loadExistingCA(certPath, keyPath); ok {
		return certPath, keyPath, cert, key, nil
	}

	cert, key, certPEM, keyPEM, err := generateCA()
	if err != nil {
		return "", "", nil, nil, err
	}

	// Write private key first, then certificate, using Windows-safe atomic renames.
	if err := writeFileAtomic(keyPath, keyPEM, 0o600); err != nil {
		return "", "", nil, nil, fmt.Errorf("localproxy: write CA key: %w", err)
	}
	if err := writeFileAtomic(certPath, certPEM, 0o644); err != nil {
		return "", "", nil, nil, fmt.Errorf("localproxy: write CA cert: %w", err)
	}

	return certPath, keyPath, cert, key, nil
}

func loadExistingCA(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, bool) {
	certBytes, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, false
	}
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, false
	}

	certBlock, _ := pem.Decode(certBytes)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return nil, nil, false
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, false
	}
	if !cert.IsCA || !cert.BasicConstraintsValid {
		return nil, nil, false
	}

	now := time.Now()
	if now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
		return nil, nil, false
	}

	keyBlock, _ := pem.Decode(keyBytes)
	if keyBlock == nil {
		return nil, nil, false
	}

	var ecKey *ecdsa.PrivateKey
	if k, err := x509.ParseECPrivateKey(keyBlock.Bytes); err == nil {
		ecKey = k
	} else if anyKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes); err == nil {
		if k, ok := anyKey.(*ecdsa.PrivateKey); ok {
			ecKey = k
		}
	}
	if ecKey == nil {
		return nil, nil, false
	}

	certPub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !certPub.Equal(&ecKey.PublicKey) {
		return nil, nil, false
	}

	return cert, ecKey, true
}

func generateCA() (*x509.Certificate, *ecdsa.PrivateKey, []byte, []byte, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("localproxy: generate CA key: %w", err)
	}

	serial, err := randomSerialNumber()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("localproxy: generate CA serial: %w", err)
	}

	ski, err := subjectKeyID(&caKey.PublicKey)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("localproxy: compute CA subject key id: %w", err)
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "GVX Desktop Local Dev CA",
			Organization: []string{"GVX Desktop"},
		},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		SubjectKeyId:          ski,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("localproxy: create CA certificate: %w", err)
	}
	caCert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("localproxy: parse CA certificate: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(caKey)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("localproxy: marshal CA key: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return caCert, caKey, certPEM, keyPEM, nil
}

func randomSerialNumber() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	for {
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return nil, err
		}
		if n.Sign() > 0 {
			return n, nil
		}
	}
}

func subjectKeyID(pub *ecdsa.PublicKey) ([]byte, error) {
	pubBytes, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(pubBytes)
	return sum[:20], nil
}

// writeFileAtomic writes data to a temporary file in the destination directory,
// sets permissions, syncs and closes the handle BEFORE renaming so the atomic
// replacement succeeds on Windows as well as Unix platforms.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".localproxy-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

type cachedCert struct {
	cert    *tls.Certificate
	expires time.Time
	created time.Time
}

// certCache dynamically mints and caches ECDSA P-256 leaf TLS certificates for
// localhost and *.localhost hostnames, signed by the local dev CA.
type certCache struct {
	caCert     *x509.Certificate
	caKey      *ecdsa.PrivateKey
	maxEntries int

	mu    sync.RWMutex
	certs map[string]*cachedCert
}

func newCertCache(caCert *x509.Certificate, caKey *ecdsa.PrivateKey) *certCache {
	return &certCache{
		caCert:     caCert,
		caKey:      caKey,
		maxEntries: maxCertCacheSize,
		certs:      make(map[string]*cachedCert),
	}
}

// GetCertificate implements tls.Config.GetCertificate. It normalizes SNI,
// enforces localhost / *.localhost scope, and returns a cached or freshly
// minted leaf certificate signed by the local dev CA.
func (c *certCache) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	rawHost := ""
	if hello != nil {
		rawHost = hello.ServerName
	}
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(rawHost), "."))
	if host == "" {
		host = "localhost"
	}
	if !isAllowedLocalhost(host) {
		return nil, fmt.Errorf("localproxy: refusing to mint TLS certificate for non-localhost host %q", host)
	}

	now := time.Now()
	c.mu.RLock()
	if entry, ok := c.certs[host]; ok && now.Before(entry.expires.Add(-24*time.Hour)) {
		c.mu.RUnlock()
		return entry.cert, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-check after acquiring the write lock.
	if entry, ok := c.certs[host]; ok && now.Before(entry.expires.Add(-24*time.Hour)) {
		return entry.cert, nil
	}

	cert, expires, err := c.mintLeaf(host, now)
	if err != nil {
		return nil, err
	}

	c.evictIfNeeded(now)
	c.certs[host] = &cachedCert{
		cert:    cert,
		expires: expires,
		created: now,
	}
	return cert, nil
}

func (c *certCache) mintLeaf(host string, now time.Time) (*tls.Certificate, time.Time, error) {
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("localproxy: generate leaf key: %w", err)
	}
	serial, err := randomSerialNumber()
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("localproxy: generate leaf serial: %w", err)
	}
	ski, err := subjectKeyID(&leafKey.PublicKey)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("localproxy: compute leaf subject key id: %w", err)
	}

	dnsNames := make([]string, 0, 3)
	if host != "localhost" {
		dnsNames = append(dnsNames, host)
	}
	dnsNames = append(dnsNames, "localhost", "*.localhost")

	var ipAddrs []net.IP
	if host == "localhost" {
		ipAddrs = []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	}

	expires := now.AddDate(0, 0, 397)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   host,
			Organization: []string{"GVX Desktop"},
		},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              expires,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		SubjectKeyId:          ski,
		DNSNames:              dnsNames,
		IPAddresses:           ipAddrs,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.caCert, &leafKey.PublicKey, c.caKey)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("localproxy: sign leaf certificate for %q: %w", host, err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("localproxy: parse leaf certificate for %q: %w", host, err)
	}

	tlsCert := &tls.Certificate{
		Certificate: [][]byte{der, c.caCert.Raw},
		PrivateKey:  leafKey,
		Leaf:        leaf,
	}
	return tlsCert, expires, nil
}

func (c *certCache) evictIfNeeded(now time.Time) {
	if len(c.certs) < c.maxEntries {
		return
	}
	for k, entry := range c.certs {
		if !now.Before(entry.expires.Add(-24 * time.Hour)) {
			delete(c.certs, k)
		}
	}
	for len(c.certs) >= c.maxEntries {
		var oldestKey string
		var oldestTime time.Time
		first := true
		for k, entry := range c.certs {
			if first || entry.created.Before(oldestTime) {
				oldestKey = k
				oldestTime = entry.created
				first = false
			}
		}
		if first {
			break
		}
		delete(c.certs, oldestKey)
	}
}

// isAllowedLocalhost reports whether host is "localhost" or a valid
// "*.localhost" hostname with well-formed DNS labels.
func isAllowedLocalhost(host string) bool {
	if host == "localhost" {
		return true
	}
	if !strings.HasSuffix(host, ".localhost") {
		return false
	}
	prefix := strings.TrimSuffix(host, ".localhost")
	if prefix == "" || len(host) > 253 {
		return false
	}
	labels := strings.Split(prefix, ".")
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			ch := label[i]
			if !((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-') {
				return false
			}
		}
	}
	return true
}

// TrustCommand returns a platform-appropriate shell command for trusting the
// local development CA certificate at certPath.
func TrustCommand(certPath string) string {
	return trustCommandForOS(runtime.GOOS, certPath)
}

func trustCommandForOS(goos, certPath string) string {
	switch goos {
	case "darwin":
		return fmt.Sprintf(`security add-trusted-cert -d -r trustRoot -k ~/Library/Keychains/login.keychain-db "%s"`, certPath)
	case "windows":
		return fmt.Sprintf(`certutil -user -addstore Root "%s"`, certPath)
	default:
		return fmt.Sprintf(`sudo cp "%s" /usr/local/share/ca-certificates/gvx-desktop-dev-ca.crt && sudo update-ca-certificates`, certPath)
	}
}
