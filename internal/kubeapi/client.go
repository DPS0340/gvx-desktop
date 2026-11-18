// Package kubeapi is a small client of the Kubernetes API, enough to list,
// delete and read the logs of the pods of the k3s cluster a gvx sandbox
// runs, and what they use as metrics-server tells it, list, scale, restart
// and delete its workloads, list its services and forward ports to its
// pods. It reaches the API server with the credentials of a kubeconfig,
// as kubectl would, over plain net/http.
package kubeapi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to one API server.
type Client struct {
	http  *http.Client
	base  string
	token string
	tls   *tls.Config // what the client checks the server with and shows it
	host  string      // the server's host:port, which a port forward dials
}

// Open reads the kubeconfig at path and connects with its current context.
func Open(path string) (*Client, error) {
	cfg, err := ReadConfig(path)
	if err != nil {
		return nil, err
	}
	e, ok := cfg.Entry("")
	if !ok {
		return nil, errors.New("the kubeconfig has no current context")
	}
	return New(e)
}

// New connects with a kubeconfig's entry. The server's certificate is
// checked against the entry's CA when it has one, as kubectl does, and
// against the system's otherwise; for the name the entry's ServerName
// gives, or else the server's host, which may be an IP address the
// certificate must name. The client authenticates with the entry's
// certificate, its token, or both.
func New(e Entry) (*Client, error) {
	u, err := url.Parse(e.Server)
	if err != nil {
		return nil, fmt.Errorf("the kubeconfig's server %q: %w", e.Server, err)
	}
	if u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("the kubeconfig's server %q is not an https URL", e.Server)
	}
	conf := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: e.ServerName}
	if conf.ServerName == "" {
		conf.ServerName = u.Hostname()
	}
	if e.CA != "" {
		pem, err := base64.StdEncoding.DecodeString(e.CA)
		if err != nil {
			return nil, fmt.Errorf("the kubeconfig's certificate-authority-data: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("the kubeconfig's certificate-authority-data holds no certificate")
		}
		conf.RootCAs = pool
	}
	if e.Cert != "" || e.Key != "" {
		cert, err := base64.StdEncoding.DecodeString(e.Cert)
		if err != nil {
			return nil, fmt.Errorf("the kubeconfig's client-certificate-data: %w", err)
		}
		key, err := base64.StdEncoding.DecodeString(e.Key)
		if err != nil {
			return nil, fmt.Errorf("the kubeconfig's client-key-data: %w", err)
		}
		pair, err := tls.X509KeyPair(cert, key)
		if err != nil {
			return nil, fmt.Errorf("the kubeconfig's client certificate: %w", err)
		}
		conf.Certificates = []tls.Certificate{pair}
	}
	tr := &http.Transport{
		TLSClientConfig:     conf,
		MaxIdleConns:        2,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	base := strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/")
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "443")
	}
	return &Client{http: &http.Client{Transport: tr, Timeout: 15 * time.Second}, base: base, token: e.Token, tls: conf.Clone(), host: host}, nil
}

// Close releases idle connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// status is the Status object the API server answers a failure with.
type status struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Reason  string `json:"reason"`
}

// send makes a request and returns the body of a successful answer, which
// the caller closes.
func (c *Client) send(ctx context.Context, method, path string) (io.ReadCloser, error) {
	return c.sendBody(ctx, method, path, "", nil)
}

// sendBody is send with a body of the content type given.
func (c *Client) sendBody(ctx context.Context, method, path, contentType string, body []byte) (io.ReadCloser, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 300 {
		return res.Body, nil
	}
	defer res.Body.Close()
	return nil, failure(res, method, path)
}

// failure is the error a failed answer carries: the message of the Status
// the API server answers with, or its HTTP status.
func failure(res *http.Response, method, path string) error {
	body, _ := io.ReadAll(io.LimitReader(res.Body, 64*1024))
	var s status
	if json.Unmarshal(body, &s) == nil && s.Message != "" {
		return &APIError{Code: res.StatusCode, Reason: s.Reason, Message: s.Message}
	}
	return &APIError{Code: res.StatusCode, Message: fmt.Sprintf("%s %s: %s", method, path, res.Status)}
}

// APIError is a failure the API server answered with.
type APIError struct {
	Code    int    // the HTTP status
	Reason  string // the Status's reason, such as NotFound
	Message string
}

func (e *APIError) Error() string { return e.Message }

func (c *Client) do(ctx context.Context, method, path string, out any) error {
	return c.doBody(ctx, method, path, "", nil, out)
}

// doBody makes a request with a body and decodes the answer into out, or
// drops it for a nil out.
func (c *Client) doBody(ctx context.Context, method, path, contentType string, in []byte, out any) error {
	body, err := c.sendBody(ctx, method, path, contentType, in)
	if err != nil {
		return err
	}
	defer body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, body)
		return nil
	}
	return json.NewDecoder(body).Decode(out)
}

// podPath is the path of a pod's resource.
func podPath(namespace, name string) string {
	return "/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods/" + url.PathEscape(name)
}

// Pods lists the pods of every namespace.
func (c *Client) Pods(ctx context.Context) ([]Pod, error) {
	var list struct {
		Items []Pod `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/pods", &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// DeletePod deletes a pod, gracefully: its containers are given their
// grace period to stop, and the pod shows as Terminating until then.
func (c *Client) DeletePod(ctx context.Context, namespace, name string) error {
	return c.do(ctx, http.MethodDelete, podPath(namespace, name), nil)
}

// LogLine is a line a container wrote, with the time the kubelet read it.
type LogLine struct {
	Time time.Time
	Text string
}

// Logs reads the last tail lines a container of a pod wrote. The kubelet
// gives no stream: stdout and stderr come mixed.
func (c *Client) Logs(ctx context.Context, namespace, name, container string, tail int) ([]LogLine, error) {
	q := url.Values{}
	if container != "" {
		q.Set("container", container)
	}
	if tail > 0 {
		q.Set("tailLines", strconv.Itoa(tail))
	}
	q.Set("timestamps", "true")
	body, err := c.send(ctx, http.MethodGet, podPath(namespace, name)+"/log?"+q.Encode())
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return readLogs(body)
}

// readLogs splits a log into its lines, each led by its RFC 3339 time.
func readLogs(r io.Reader) ([]LogLine, error) {
	var out []LogLine
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		var l LogLine
		if stamp, text, ok := strings.Cut(line, " "); ok {
			if t, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
				l = LogLine{Time: t, Text: text}
			}
		}
		if l.Time.IsZero() {
			if t, err := time.Parse(time.RFC3339Nano, line); err == nil {
				l = LogLine{Time: t} // an empty line
			} else {
				l.Text = line
			}
		}
		out = append(out, l)
	}
	return out, sc.Err()
}
