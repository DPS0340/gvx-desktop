// Package dockerapi is a small client of the Docker Engine API, enough to
// list, control and read the logs and the usage of the containers of a
// dockerd that runs in a gvx sandbox.
package dockerapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
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
	network, addr, base, err := parseHost(host)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{MaxIdleConns: 2, IdleConnTimeout: 30 * time.Second}
	if network == "unix" {
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}
	}
	dial := func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	return &Client{http: &http.Client{Transport: tr, Timeout: 15 * time.Second}, base: base, dial: dial}, nil
}

func parseHost(host string) (network, addr, base string, err error) {
	return parseHostFor(host, filepath.FromSlash)
}

// parseHostFor parses a DOCKER_HOST string into (network, dialAddr, httpBase).
// Unix hosts are parsed by cutting the "unix://" prefix rather than url.Parse
// so Windows drive letters such as "unix://C:/Users/..." or "unix:///C:/..."
// are preserved instead of being split into url.Host.
func parseHostFor(host string, fromSlash func(string) string) (network, addr, base string, err error) {
	if rest, ok := strings.CutPrefix(host, "unix://"); ok {
		if len(rest) >= 3 && rest[0] == '/' && isASCIIAlpha(rest[1]) && rest[2] == ':' {
			rest = rest[1:]
		}
		if rest == "" {
			return "", "", "", fmt.Errorf("unsupported docker host %q", host)
		}
		return "unix", fromSlash(rest), "http://docker", nil
	}
	if rest, ok := strings.CutPrefix(host, "unix:"); ok {
		if rest == "" {
			return "", "", "", fmt.Errorf("unsupported docker host %q", host)
		}
		return "unix", fromSlash(rest), "http://docker", nil
	}
	u, err := url.Parse(host)
	if err != nil {
		return "", "", "", err
	}
	switch u.Scheme {
	case "tcp", "http":
		if u.Host == "" {
			return "", "", "", fmt.Errorf("unsupported docker host %q", host)
		}
		return "tcp", u.Host, "http://" + u.Host, nil
	default:
		return "", "", "", fmt.Errorf("unsupported docker host %q", host)
	}
}

func isASCIIAlpha(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
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
	Mounts  []Mount `json:"Mounts"`
	// NetworkSettings holds the networks the container is on, by name.
	NetworkSettings struct {
		Networks map[string]Endpoint `json:"Networks"`
	} `json:"NetworkSettings"`
}

// Endpoint is a container's place on a network.
type Endpoint struct {
	NetworkID   string   `json:"NetworkID"`
	IPAddress   string   `json:"IPAddress"`
	IPPrefixLen int      `json:"IPPrefixLen"`
	Gateway     string   `json:"Gateway"`
	Aliases     []string `json:"Aliases"`
	DNSNames    []string `json:"DNSNames"`
}

// ContainerNetwork is one of a container's networks, with its name.
type ContainerNetwork struct {
	Name string
	Endpoint
}

// Networks are the container's networks, sorted by name.
func (c Container) Networks() []ContainerNetwork {
	out := make([]ContainerNetwork, 0, len(c.NetworkSettings.Networks))
	for name, ep := range c.NetworkSettings.Networks {
		out = append(out, ContainerNetwork{Name: name, Endpoint: ep})
	}
	slices.SortFunc(out, func(a, b ContainerNetwork) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// networksKey is the container's networks and addresses as one string,
// for Equal.
func (c Container) networksKey() string {
	var b strings.Builder
	for _, n := range c.Networks() {
		b.WriteString(n.Name + "=" + n.IPAddress + "/" + strings.Join(n.DNSNames, ",") + ";")
	}
	return b.String()
}

// Mount is a mount of a container: a bind mount of a path where dockerd
// runs, or a volume.
type Mount struct {
	Type        string `json:"Type"` // bind, volume, tmpfs
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
}

// Binds are the container's bind mounts.
func (c Container) Binds() []Mount {
	var out []Mount
	for _, m := range c.Mounts {
		if m.Type == "bind" {
			out = append(out, m)
		}
	}
	return out
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
		c.Name() == o.Name() && c.PortsText() == o.PortsText() && slices.Equal(c.Mounts, o.Mounts) &&
		c.networksKey() == o.networksKey()
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

// Pause freezes a running container.
func (c *Client) Pause(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/pause", nil)
}

// Unpause thaws a paused container.
func (c *Client) Unpause(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/unpause", nil)
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

// ImagePlatform identifies the OS and CPU architecture of an image manifest.
type ImagePlatform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
}

// ImageManifestData holds image-specific metadata for an image manifest.
type ImageManifestData struct {
	Platform ImagePlatform `json:"Platform"`
}

// ImageManifest is one manifest entry in GET /images/json (API 1.47+).
type ImageManifest struct {
	ID        string             `json:"ID"`
	Available bool               `json:"Available"`
	Kind      string             `json:"Kind"`
	ImageData *ImageManifestData `json:"ImageData,omitempty"`
}

// Equal makes ImageManifest comparable for signals.
func (m ImageManifest) Equal(o ImageManifest) bool {
	if m.ID != o.ID || m.Available != o.Available || m.Kind != o.Kind {
		return false
	}
	if (m.ImageData == nil) != (o.ImageData == nil) {
		return false
	}
	return m.ImageData == nil || *m.ImageData == *o.ImageData
}

// Image is an entry of GET /images/json.
type Image struct {
	ID          string          `json:"Id"`
	RepoTags    []string        `json:"RepoTags"`
	RepoDigests []string        `json:"RepoDigests"`
	Size        int64           `json:"Size"`
	Created     int64           `json:"Created"`
	Containers  int64           `json:"Containers"`
	Manifests   []ImageManifest `json:"Manifests,omitempty"`
}

// Architecture returns the image's CPU architecture from its available image
// manifest, or "" when GET /images/json did not include manifests.
func (i Image) Architecture() string {
	var fallback string
	for _, m := range i.Manifests {
		if m.ImageData == nil || m.ImageData.Platform.Architecture == "" {
			continue
		}
		if m.Kind != "" && m.Kind != "image" {
			continue
		}
		if m.Available {
			return m.ImageData.Platform.Architecture
		}
		if fallback == "" {
			fallback = m.ImageData.Platform.Architecture
		}
	}
	return fallback
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
		slices.Equal(i.RepoDigests, o.RepoDigests) &&
		slices.EqualFunc(i.Manifests, o.Manifests, ImageManifest.Equal)
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
	return c.InspectImagePlatform(ctx, name, "")
}

// InspectImagePlatform returns the indented JSON of GET /images/{name}/json,
// scoped to platform (such as "linux/amd64") when non-empty.
func (c *Client) InspectImagePlatform(ctx context.Context, name, platform string) (string, error) {
	path := "/images/" + escapeImagePath(name) + "/json"
	if platform != "" {
		path += "?" + url.Values{"platform": {platform}}.Encode()
	}
	body, err := c.send(ctx, http.MethodGet, path)
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
	return c.RemoveImagePlatform(ctx, name, force, "")
}

// RemoveImagePlatform untags or deletes an image from the engine, scoped to
// platform (such as "linux/amd64") when non-empty.
func (c *Client) RemoveImagePlatform(ctx context.Context, name string, force bool, platform string) error {
	q := url.Values{}
	if force {
		q.Set("force", "1")
	}
	if platform != "" {
		q.Set("platform", platform)
	}
	path := "/images/" + escapeImagePath(name)
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return c.do(ctx, http.MethodDelete, path, nil)
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
	return c.stream(ctx, "/images/create?"+q.Encode(), line)
}

// Tag names the image source repo:tag as well.
func (c *Client) Tag(ctx context.Context, source, repo, tag string) error {
	return c.TagPlatform(ctx, source, repo, tag, "")
}

// TagPlatform names the image source repo:tag as well, scoped to platform
// (such as "linux/amd64") when non-empty.
func (c *Client) TagPlatform(ctx context.Context, source, repo, tag, platform string) error {
	q := url.Values{"repo": {repo}, "tag": {tag}}
	if platform != "" {
		q.Set("platform", platform)
	}
	return c.do(ctx, http.MethodPost, "/images/"+escapeImagePath(source)+"/tag?"+q.Encode(), nil)
}

// PushImage pushes ref, which names its registry, from the engine, handing
// progress lines to line. It needs no credentials: the registries the app
// pushes to take none.
func (c *Client) PushImage(ctx context.Context, ref string, line func(string)) error {
	name, tag := splitImageRef(ref)
	path := "/images/" + escapeImagePath(name) + "/push"
	if tag != "" {
		path += "?" + url.Values{"tag": {tag}}.Encode()
	}
	// dockerd refuses a push without an X-Registry-Auth header; an empty
	// configuration is no credentials.
	return c.stream(ctx, path, line, "X-Registry-Auth", base64.URLEncoding.EncodeToString([]byte("{}")))
}

// stream posts to path and reads the progress dockerd answers with, a JSON
// object a line, as pulls and pushes do, handing what each says to line.
// An object that carries an error ends it with that error. header is
// pairs of a header and its value.
func (c *Client) stream(ctx context.Context, path string, line func(string), header ...string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, nil)
	if err != nil {
		return err
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	// A pull or push can take minutes; ctx bounds it, not the short API timeout.
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
		op, _, _ := strings.Cut(path, "?")
		return fmt.Errorf("POST %s: %s", op, res.Status)
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

// Network is an entry of GET /networks.
type Network struct {
	Name       string `json:"Name"`
	ID         string `json:"Id"`
	Created    string `json:"Created"`
	Driver     string `json:"Driver"`
	Scope      string `json:"Scope"`
	Internal   bool   `json:"Internal"`
	EnableIPv6 bool   `json:"EnableIPv6"`
	IPAM       struct {
		Config []struct {
			Subnet  string `json:"Subnet"`
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
	Containers map[string]NetworkMember `json:"Containers"`
	Labels     map[string]string        `json:"Labels"`
}

// NetworkMember is a container on a network, as the network lists it.
type NetworkMember struct {
	Name        string `json:"Name"`
	IPv4Address string `json:"IPv4Address"` // with its prefix length: 10.89.1.2/24
	IPv6Address string `json:"IPv6Address"`
}

// Builtin reports a network the engine always has: bridge, host and none.
func (n Network) Builtin() bool {
	switch n.Name {
	case "bridge", "host", "none":
		return true
	}
	return false
}

// Subnet is the network's first subnet and its gateway, or "".
func (n Network) Subnet() (subnet, gateway string) {
	if len(n.IPAM.Config) == 0 {
		return "", ""
	}
	return n.IPAM.Config[0].Subnet, n.IPAM.Config[0].Gateway
}

// Project is the Compose project that made the network, if any.
func (n Network) Project() string { return n.Labels["com.docker.compose.project"] }

// ShortID is the first 12 digits of the network's ID.
func (n Network) ShortID() string { return n.ID[:min(12, len(n.ID))] }

// CreatedAt is when the network was made, or the zero time.
func (n Network) CreatedAt() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, n.Created)
	return t
}

// Equal makes Network comparable for signals.
func (n Network) Equal(o Network) bool {
	if n.ID != o.ID || n.Name != o.Name || n.Driver != o.Driver || n.Internal != o.Internal || len(n.Containers) != len(o.Containers) {
		return false
	}
	if sa, _ := n.Subnet(); sa != "" || len(o.IPAM.Config) > 0 {
		if sb, _ := o.Subnet(); sa != sb {
			return false
		}
	}
	for id, m := range n.Containers {
		if o.Containers[id] != m {
			return false
		}
	}
	return true
}

// Networks lists the networks.
func (c *Client) Networks(ctx context.Context) ([]Network, error) {
	var out []Network
	err := c.do(ctx, http.MethodGet, "/networks", &out)
	return out, err
}

// RemoveNetwork removes a network.
func (c *Client) RemoveNetwork(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/networks/"+url.PathEscape(id), nil)
}

// PruneNetworks removes the networks no container uses and returns their
// names.
func (c *Client) PruneNetworks(ctx context.Context) ([]string, error) {
	var raw struct {
		NetworksDeleted []string `json:"NetworksDeleted"`
	}
	err := c.do(ctx, http.MethodPost, "/networks/prune", &raw)
	return raw.NetworksDeleted, err
}
