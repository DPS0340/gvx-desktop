// Package dockerapi is a small client of the Docker Engine API, enough to
// list, control and read the logs and the usage of the containers of a
// dockerd that runs in a gvx sandbox.
package dockerapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Client talks to one engine.
type Client struct {
	http *http.Client
	base string
	// dial connects to the engine, for a request that takes the
	// connection over, as an exec's does.
	dial func(ctx context.Context) (net.Conn, error)
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
	var network, addr string
	switch u.Scheme {
	case "unix":
		network, addr = "unix", u.Path
		if addr == "" {
			addr = u.Opaque
		}
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}
	case "tcp", "http":
		network, addr = "tcp", u.Host
		base = "http://" + u.Host
	default:
		return nil, fmt.Errorf("unsupported docker host %q", host)
	}
	dial := func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	return &Client{http: &http.Client{Transport: tr, Timeout: 15 * time.Second}, base: base, dial: dial}, nil
}

// Close releases idle connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }

type apiError struct {
	Message string `json:"message"`
}

func (c *Client) do(ctx context.Context, method, path string, out any) error {
	body, err := c.send(ctx, method, path)
	if err != nil || body == nil {
		return err
	}
	defer body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, body)
		return nil
	}
	return json.NewDecoder(body).Decode(out)
}

// send makes a request and returns the body of a successful answer, which
// the caller closes, or nil for a 304, which has none.
func (c *Client) send(ctx context.Context, method, path string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 300 {
		return res.Body, nil
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotModified {
		return nil, nil
	}
	var e apiError
	body, _ := io.ReadAll(io.LimitReader(res.Body, 64*1024))
	if json.Unmarshal(body, &e) == nil && e.Message != "" {
		return nil, fmt.Errorf("%s", e.Message)
	}
	return nil, fmt.Errorf("%s %s: %s", method, path, res.Status)
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
	ImageID string   `json:"ImageID"`
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
		return c.ShortID()
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// Project is the Compose project the container belongs to, if any.
func (c Container) Project() string { return c.Labels["com.docker.compose.project"] }

// Service is the service of its Compose project the container runs, if
// any.
func (c Container) Service() string { return c.Labels["com.docker.compose.service"] }

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

// ShortID is the first twelve characters of the ID, as docker ps shows it.
func (c Container) ShortID() string { return c.ID[:min(12, len(c.ID))] }

// ExitCode is the code an exited container ended with, as its Status tells
// it ("Exited (137) 2 hours ago"), and ok false for a container that has
// not exited.
func (c Container) ExitCode() (code int, ok bool) {
	if c.State != "exited" {
		return 0, false
	}
	rest, found := strings.CutPrefix(c.Status, "Exited (")
	if !found {
		return 0, false
	}
	num, _, found := strings.Cut(rest, ")")
	if !found {
		return 0, false
	}
	code, err := strconv.Atoi(num)
	return code, err == nil
}

// LogLine is a line a container wrote.
type LogLine struct {
	Stderr bool      // written to standard error, not standard output
	Time   time.Time // when the engine received it
	Text   string
}

// maxLogBytes bounds how much of a container's log Logs reads.
const maxLogBytes = 4 << 20

// Logs reads the last tail lines a container wrote to its standard output
// and error, oldest first, with the time of each.
func (c *Client) Logs(ctx context.Context, id string, tail int) ([]LogLine, error) {
	path := fmt.Sprintf("/containers/%s/logs?stdout=1&stderr=1&timestamps=1&tail=%d", url.PathEscape(id), tail)
	body, err := c.send(ctx, http.MethodGet, path)
	if err != nil || body == nil {
		return nil, err
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, maxLogBytes))
	if err != nil {
		return nil, err
	}
	return parseLogs(raw), nil
}

// parseLogs splits what GET /containers/{id}/logs answers into lines. A
// container without a terminal has its two streams multiplexed, each run of
// bytes behind an eight-byte header: the stream (1 or 2), three zeros, and
// the run's length, big-endian. One with a terminal has a single stream,
// sent as it is. With timestamps on, every line starts with an RFC 3339
// time, which no header does.
func parseLogs(raw []byte) []LogLine {
	var out []LogLine
	pending := [3][]byte{}
	emit := func(stream byte, b []byte) {
		for {
			i := bytes.IndexByte(b, '\n')
			if i < 0 {
				pending[stream] = append(pending[stream], b...)
				return
			}
			line := append(pending[stream], b[:i]...)
			pending[stream] = nil
			b = b[i+1:]
			out = append(out, logLine(stream == 2, line))
		}
	}
	if multiplexed(raw) {
		for len(raw) >= 8 {
			stream, n := raw[0], int(binary.BigEndian.Uint32(raw[4:8]))
			raw = raw[8:]
			n = min(n, len(raw))
			if stream > 2 {
				stream = 1
			}
			emit(stream, raw[:n])
			raw = raw[n:]
		}
	} else {
		emit(1, raw)
	}
	for stream, rest := range pending {
		if len(rest) > 0 {
			out = append(out, logLine(stream == 2, rest))
		}
	}
	return out
}

// multiplexed reports whether raw starts with a stream header.
func multiplexed(raw []byte) bool {
	return len(raw) >= 8 && raw[0] <= 2 && raw[1] == 0 && raw[2] == 0 && raw[3] == 0
}

// logLine splits the time from the front of a line.
func logLine(stderr bool, b []byte) LogLine {
	s := strings.TrimSuffix(string(b), "\r")
	l := LogLine{Stderr: stderr, Text: s}
	if stamp, text, ok := strings.Cut(s, " "); ok {
		if t, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
			l.Time, l.Text = t, text
		}
	} else if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		l.Time, l.Text = t, ""
	}
	return l
}

// Image is an entry of GET /images/json.
type Image struct {
	ID          string   `json:"Id"`
	RepoTags    []string `json:"RepoTags"`
	RepoDigests []string `json:"RepoDigests"`
	Size        int64    `json:"Size"`
	Created     int64    `json:"Created"`
	Containers  int64    `json:"Containers"`
}

// ShortID is the first twelve hex characters of the ID, without any
// algorithm prefix, as docker images shows it.
func (i Image) ShortID() string {
	id := strings.TrimPrefix(i.ID, "sha256:")
	return id[:min(12, len(id))]
}

// Equal makes Image comparable for signals.
func (i Image) Equal(o Image) bool {
	return i.ID == o.ID && i.Size == o.Size && i.Created == o.Created &&
		i.Containers == o.Containers &&
		slices.Equal(i.RepoTags, o.RepoTags) &&
		slices.Equal(i.RepoDigests, o.RepoDigests)
}

// escapeImagePath escapes each segment of an image reference while keeping
// the slashes that Docker's /images/{name:.*} route expects.
func escapeImagePath(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// Images lists the engine's images.
func (c *Client) Images(ctx context.Context) ([]Image, error) {
	var out []Image
	err := c.do(ctx, http.MethodGet, "/images/json", &out)
	return out, err
}

// InspectImage returns the indented JSON of GET /images/{name}/json.
func (c *Client) InspectImage(ctx context.Context, name string) (string, error) {
	body, err := c.send(ctx, http.MethodGet, "/images/"+escapeImagePath(name)+"/json")
	if err != nil || body == nil {
		return "", err
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, 4<<20))
	if err != nil {
		return "", err
	}
	var v any
	if json.Unmarshal(raw, &v) == nil {
		if pretty, err := json.MarshalIndent(v, "", "  "); err == nil {
			return string(pretty), nil
		}
	}
	return string(raw), nil
}

// RemoveImage untags or deletes an image from the engine.
func (c *Client) RemoveImage(ctx context.Context, name string, force bool) error {
	q := ""
	if force {
		q = "?force=1"
	}
	return c.do(ctx, http.MethodDelete, "/images/"+escapeImagePath(name)+q, nil)
}

// PruneResult is what POST /images/prune removed.
type PruneResult struct {
	Deleted        int
	SpaceReclaimed int64
}

// PruneImages deletes dangling images, or every unused image when all is set.
func (c *Client) PruneImages(ctx context.Context, all bool) (PruneResult, error) {
	path := "/images/prune"
	if all {
		path += "?filters=" + url.QueryEscape(`{"dangling":["false"]}`)
	}
	var raw struct {
		ImagesDeleted []struct {
			Deleted  string `json:"Deleted"`
			Untagged string `json:"Untagged"`
		} `json:"ImagesDeleted"`
		SpaceReclaimed int64 `json:"SpaceReclaimed"`
	}
	if err := c.do(ctx, http.MethodPost, path, &raw); err != nil {
		return PruneResult{}, err
	}
	deleted := 0
	for _, item := range raw.ImagesDeleted {
		if item.Deleted != "" {
			deleted++
		}
	}
	if deleted == 0 {
		deleted = len(raw.ImagesDeleted)
	}
	return PruneResult{Deleted: deleted, SpaceReclaimed: raw.SpaceReclaimed}, nil
}

// splitImageRef splits a reference into the fromImage and tag query
// parameters of POST /images/create. When no tag or digest is named, tag is
// "latest" so the engine pulls one tag rather than every tag of the repository.
func splitImageRef(ref string) (fromImage, tag string) {
	ref = strings.TrimSpace(ref)
	if strings.Contains(ref, "@") {
		return ref, ""
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i], ref[i+1:]
	}
	return ref, "latest"
}

// PullImage pulls ref into the engine, handing progress lines to line.
func (c *Client) PullImage(ctx context.Context, ref string, line func(string)) error {
	from, tag := splitImageRef(ref)
	q := url.Values{"fromImage": {from}}
	if tag != "" {
		q.Set("tag", tag)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/images/create?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	// Pulling an image can take minutes; ctx bounds it, not the short API timeout.
	streamClient := &http.Client{Transport: c.http.Transport}
	res, err := streamClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		var e apiError
		body, _ := io.ReadAll(io.LimitReader(res.Body, 64*1024))
		if json.Unmarshal(body, &e) == nil && e.Message != "" {
			return fmt.Errorf("%s", e.Message)
		}
		return fmt.Errorf("POST /images/create: %s", res.Status)
	}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		var ev struct {
			Status      string `json:"status"`
			ID          string `json:"id"`
			Progress    string `json:"progress"`
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			continue
		}
		if msg := ev.ErrorDetail.Message; msg != "" {
			return fmt.Errorf("%s", msg)
		}
		if ev.Error != "" {
			return fmt.Errorf("%s", ev.Error)
		}
		if line == nil {
			continue
		}
		var parts []string
		if ev.ID != "" {
			parts = append(parts, ev.ID+":")
		}
		if ev.Status != "" {
			parts = append(parts, ev.Status)
		}
		if ev.Progress != "" {
			parts = append(parts, ev.Progress)
		}
		if len(parts) > 0 {
			line(strings.Join(parts, " "))
		}
	}
	return sc.Err()
}
