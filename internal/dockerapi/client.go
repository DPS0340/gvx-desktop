// Package dockerapi is a small client of the Docker Engine API, enough to
// list and control the containers of a dockerd that runs in a gvx sandbox.
package dockerapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to one engine.
type Client struct {
	http *http.Client
	base string
}

// New connects to host, a unix:// socket or tcp:// address as DOCKER_HOST
// writes them.
func New(host string) (*Client, error) {
	u, err := url.Parse(host)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{MaxIdleConns: 2, IdleConnTimeout: 30 * time.Second}
	base := "http://docker"
	switch u.Scheme {
	case "unix":
		path := u.Path
		if path == "" {
			path = u.Opaque
		}
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}
	case "tcp", "http":
		base = "http://" + u.Host
	default:
		return nil, fmt.Errorf("unsupported docker host %q", host)
	}
	return &Client{http: &http.Client{Transport: tr, Timeout: 15 * time.Second}, base: base}, nil
}

// Close releases idle connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }

type apiError struct {
	Message string `json:"message"`
}

func (c *Client) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, nil)
	if err != nil {
		return err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		if res.StatusCode == http.StatusNotModified {
			return nil
		}
		var e apiError
		body, _ := io.ReadAll(io.LimitReader(res.Body, 64*1024))
		if json.Unmarshal(body, &e) == nil && e.Message != "" {
			return fmt.Errorf("%s", e.Message)
		}
		return fmt.Errorf("%s %s: %s", method, path, res.Status)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, res.Body)
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// Ping checks that the engine answers.
func (c *Client) Ping(ctx context.Context) error { return c.do(ctx, http.MethodGet, "/_ping", nil) }

// Version is part of GET /version.
type Version struct {
	Version       string `json:"Version"`
	APIVersion    string `json:"ApiVersion"`
	OS            string `json:"Os"`
	Arch          string `json:"Arch"`
	KernelVersion string `json:"KernelVersion"`
}

// Version reads the engine's version.
func (c *Client) Version(ctx context.Context) (Version, error) {
	var v Version
	err := c.do(ctx, http.MethodGet, "/version", &v)
	return v, err
}

// Port is a published port of a container.
type Port struct {
	IP          string `json:"IP"`
	PrivatePort int    `json:"PrivatePort"`
	PublicPort  int    `json:"PublicPort"`
	Type        string `json:"Type"`
}

// Container is an entry of GET /containers/json.
type Container struct {
	ID      string   `json:"Id"`
	Names   []string `json:"Names"`
	Image   string   `json:"Image"`
	Command string   `json:"Command"`
	Created int64    `json:"Created"`
	State   string   `json:"State"`
	Status  string   `json:"Status"`
	Ports   []Port   `json:"Ports"`
	Labels  map[string]string
}

// Name is the container's first name without the leading slash.
func (c Container) Name() string {
	if len(c.Names) == 0 {
		return c.ID[:min(12, len(c.ID))]
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// Project is the Compose project the container belongs to, if any.
func (c Container) Project() string { return c.Labels["com.docker.compose.project"] }

// PortsText formats the published ports as docker ps does.
func (c Container) PortsText() string {
	var parts []string
	seen := map[string]bool{}
	for _, p := range c.Ports {
		var s string
		if p.PublicPort != 0 {
			s = fmt.Sprintf("%d→%d/%s", p.PublicPort, p.PrivatePort, p.Type)
		} else {
			s = fmt.Sprintf("%d/%s", p.PrivatePort, p.Type)
		}
		if !seen[s] {
			seen[s] = true
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ", ")
}

// Equal makes Container comparable for signals.
func (c Container) Equal(o Container) bool {
	return c.ID == o.ID && c.State == o.State && c.Status == o.Status && c.Image == o.Image &&
		c.Name() == o.Name() && c.PortsText() == o.PortsText()
}

// Containers lists containers, stopped ones too when all is set.
func (c *Client) Containers(ctx context.Context, all bool) ([]Container, error) {
	var out []Container
	path := "/containers/json"
	if all {
		path += "?all=1"
	}
	err := c.do(ctx, http.MethodGet, path, &out)
	return out, err
}

// Start starts a container.
func (c *Client) Start(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil)
}

// Stop stops a container.
func (c *Client) Stop(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/stop?t=10", nil)
}

// Restart restarts a container.
func (c *Client) Restart(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/restart?t=10", nil)
}

// Remove deletes a container, stopping it first with force.
func (c *Client) Remove(ctx context.Context, id string, force bool) error {
	q := ""
	if force {
		q = "?force=1"
	}
	return c.do(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id)+q, nil)
}

// Image is an entry of GET /images/json.
type Image struct {
	ID       string   `json:"Id"`
	RepoTags []string `json:"RepoTags"`
	Size     int64    `json:"Size"`
	Created  int64    `json:"Created"`
}

// Images lists the engine's images.
func (c *Client) Images(ctx context.Context) ([]Image, error) {
	var out []Image
	err := c.do(ctx, http.MethodGet, "/images/json", &out)
	return out, err
}
