package extsdk

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultHostTimeout = 30 * time.Second
	maxHostRespBytes   = 8 << 20 // 8 MiB
)

// HostError represents a non-2xx error response returned by the GVX Desktop Host API.
type HostError struct {
	StatusCode int    `json:"-"`
	Code       string `json:"code,omitempty"`
	Message    string `json:"message,omitempty"`
	Err        string `json:"error,omitempty"`
}

func (e *HostError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = e.Err
	}
	if msg == "" {
		msg = http.StatusText(e.StatusCode)
	}
	if e.Code != "" {
		return fmt.Sprintf("host API HTTP %d (%s): %s", e.StatusCode, e.Code, msg)
	}
	return fmt.Sprintf("host API HTTP %d: %s", e.StatusCode, msg)
}

// IsNotFound reports whether err is a HostError with HTTP 404 or code "not_found".
func IsNotFound(err error) bool {
	var he *HostError
	return errors.As(err, &he) && (he.StatusCode == http.StatusNotFound || he.Code == "not_found")
}

// IsPermissionDenied reports whether err is a HostError with HTTP 403 or code "permission_denied".
func IsPermissionDenied(err error) bool {
	var he *HostError
	return errors.As(err, &he) && (he.StatusCode == http.StatusForbidden || he.Code == "permission_denied")
}

// IsGestureRequired reports whether err is a HostError rejecting a missing/invalid gesture token.
func IsGestureRequired(err error) bool {
	var he *HostError
	return errors.As(err, &he) && (he.Code == "gesture_required" || (he.StatusCode == http.StatusForbidden && strings.Contains(strings.ToLower(he.Message), "gesture")))
}

// IsRateLimited reports whether err is a HostError with HTTP 429 or code "rate_limited".
func IsRateLimited(err error) bool {
	var he *HostError
	return errors.As(err, &he) && (he.StatusCode == http.StatusTooManyRequests || he.Code == "rate_limited")
}

// IsServiceUnavailable reports whether err is a HostError with HTTP 503.
func IsServiceUnavailable(err error) bool {
	var he *HostError
	return errors.As(err, &he) && he.StatusCode == http.StatusServiceUnavailable
}

// HostInfo describes the GVX Desktop host environment returned by GET /v1/host/info.
type HostInfo struct {
	AppVersion   string `json:"app_version"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	Locale       string `json:"locale"`
	Theme        string `json:"theme"`
	DockerMode   string `json:"docker_mode"`
	DockerReady  bool   `json:"docker_ready"`
	RungvVersion string `json:"rungv_version,omitempty"`
}

// Settings holds extension settings returned by GET /v1/settings.
// Secret-typed settings appear as {"set": true} rather than plaintext values.
type Settings struct {
	values map[string]any
}

// UnmarshalJSON decodes settings JSON into Settings.
func (s *Settings) UnmarshalJSON(data []byte) error {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	s.values = m
	return nil
}

// Raw returns the underlying settings map.
func (s Settings) Raw() map[string]any {
	return s.values
}

// String returns a string setting value or "".
func (s Settings) String(key string) string {
	if s.values == nil {
		return ""
	}
	v, ok := s.values[key]
	if !ok || v == nil {
		return ""
	}
	str, _ := v.(string)
	return str
}

// Bool returns a boolean setting value or false.
func (s Settings) Bool(key string) bool {
	if s.values == nil {
		return false
	}
	b, _ := s.values[key].(bool)
	return b
}

// Float returns a numeric setting value as float64 or 0.
func (s Settings) Float(key string) float64 {
	if s.values == nil {
		return 0
	}
	switch v := s.values[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	default:
		return 0
	}
}

// Int returns a numeric setting value as int or 0.
func (s Settings) Int(key string) int {
	return int(s.Float(key))
}

// IsSet reports whether a setting (including masked secret settings) is configured/non-empty.
func (s Settings) IsSet(key string) bool {
	if s.values == nil {
		return false
	}
	v, ok := s.values[key]
	if !ok || v == nil {
		return false
	}
	switch x := v.(type) {
	case map[string]any:
		b, _ := x["set"].(bool)
		return b
	case string:
		return x != ""
	case bool:
		return x
	default:
		return true
	}
}

// SandboxMount describes a mount attached to a sandbox.
type SandboxMount struct {
	Type     string `json:"type"`
	Source   string `json:"source,omitempty"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"readOnly,omitempty"`
	Size     string `json:"size,omitempty"`
}

// SandboxInfo describes a sandbox returned by Host Sandbox APIs.
type SandboxInfo struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Image      string   `json:"image"`
	Status     string   `json:"status"`
	Paused     bool     `json:"paused,omitempty"`
	Checkpoint bool     `json:"checkpoint,omitempty"`
	Ports      []string `json:"ports,omitempty"`
	Recipe     string   `json:"recipe,omitempty"`
	LocalURL   string   `json:"local_url,omitempty"`
}

// CreateSandboxRequest defines parameters for POST /v1/sandboxes.
type CreateSandboxRequest struct {
	Name            string         `json:"name,omitempty"`
	Image           string         `json:"image"`
	Command         string         `json:"command,omitempty"`
	Env             []string       `json:"env,omitempty"`
	Ports           []string       `json:"ports,omitempty"`
	Mounts          []SandboxMount `json:"mounts,omitempty"`
	Workspace       string         `json:"workspace,omitempty"`
	Memory          string         `json:"memory,omitempty"`
	CPUs            string         `json:"cpus,omitempty"`
	Checkpoint      *bool          `json:"checkpoint,omitempty"`
	Start           *bool          `json:"start,omitempty"`
	Ephemeral       bool           `json:"ephemeral,omitempty"`
	MountDockerSock bool           `json:"mount_docker_sock,omitempty"`
}

// ExecRequest defines parameters for POST /v1/sandboxes/{id}/exec.
type ExecRequest struct {
	Argv       []string `json:"argv"`
	Workdir    string   `json:"workdir,omitempty"`
	Env        []string `json:"env,omitempty"`
	TimeoutSec int      `json:"timeout_sec,omitempty"`
}

// ExecResult contains the result of POST /v1/sandboxes/{id}/exec.
type ExecResult struct {
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Truncated bool   `json:"truncated,omitempty"`
}

// CreateSnapshotRequest defines parameters for POST /v1/sandboxes/{id}/snapshots.
type CreateSnapshotRequest struct {
	Name         string `json:"name"`
	LeaveRunning bool   `json:"leave_running,omitempty"`
}

// SnapshotInfo describes a sandbox checkpoint snapshot.
type SnapshotInfo struct {
	SandboxID   string    `json:"sandbox_id"`
	SandboxName string    `json:"sandbox_name,omitempty"`
	Name        string    `json:"name"`
	Dir         string    `json:"dir,omitempty"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"created_at"`
}

// RestoreRequest defines parameters for POST /v1/sandboxes/{id}/restore.
type RestoreRequest struct {
	Snapshot string   `json:"snapshot"`
	Env      []string `json:"env,omitempty"`
	Exec     []string `json:"exec,omitempty"`
}

// RestoreResult describes the outcome of POST /v1/sandboxes/{id}/restore.
type RestoreResult struct {
	Sandbox SandboxInfo `json:"sandbox,omitempty"`
	Exec    *ExecResult `json:"exec,omitempty"`
}

// BadgeRequest describes a sidebar badge update for POST /v1/ui/badge.
type BadgeRequest struct {
	Text    string `json:"text,omitempty"`
	Tone    string `json:"tone,omitempty"`
	Tooltip string `json:"tooltip,omitempty"`
}

// OpenTerminalRequest describes a target for POST /v1/ui/open-terminal.
type OpenTerminalRequest struct {
	TargetType string `json:"target_type"` // "container" or "sandbox"
	TargetID   string `json:"target_id"`
	Shell      string `json:"shell,omitempty"`
}

// HostEvent represents one SSE event emitted by GET /v1/events.
type HostEvent struct {
	ID   string          `json:"id,omitempty"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Host is a client for the GVX Desktop Host API.
type Host struct {
	client  *http.Client
	baseURL string
	token   string
	initErr error
}

// NewHost creates a Host API client for endpoint ("unix:///..." or "http(s)://...") and Bearer token.
func NewHost(endpoint, token string) (*Host, error) {
	if endpoint == "" {
		return nil, errors.New("extsdk: host endpoint is empty")
	}
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        16,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	baseURL := ""
	if strings.HasPrefix(endpoint, "unix://") {
		sockPath := strings.TrimPrefix(endpoint, "unix://")
		if sockPath == "" {
			return nil, errors.New("extsdk: empty unix socket path")
		}
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sockPath)
		}
		baseURL = "http://unix"
	} else {
		u, err := url.Parse(endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("extsdk: invalid host endpoint %q", endpoint)
		}
		baseURL = strings.TrimRight(u.String(), "/")
	}
	return &Host{
		client:  &http.Client{Transport: tr}, // Timeout: 0 so SSE streams and interactive dialogs never time out globally.
		baseURL: baseURL,
		token:   token,
	}, nil
}

// NewHostClient constructs a Host client, recording any initialization error to be surfaced on API calls.
func NewHostClient(baseURL, token string) *Host {
	h, err := NewHost(baseURL, token)
	if err != nil {
		return &Host{initErr: err}
	}
	return h
}

var jsonRawMessageType = reflect.TypeOf(json.RawMessage(nil))

func (h *Host) checkReady() error {
	if h == nil {
		return errors.New("extsdk: host client is nil")
	}
	if h.initErr != nil {
		return h.initErr
	}
	return nil
}

func validateUTF8Param(what, s string) error {
	if !utf8.ValidString(s) {
		return &HostError{
			StatusCode: http.StatusBadRequest,
			Code:       "bad_request",
			Message:    fmt.Sprintf("invalid UTF-8 in %s", what),
		}
	}
	return nil
}

func validateBodyUTF8(v reflect.Value) error {
	if !v.IsValid() {
		return nil
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return validateBodyUTF8(v.Elem())
	case reflect.String:
		if !utf8.ValidString(v.String()) {
			return &HostError{
				StatusCode: http.StatusBadRequest,
				Code:       "bad_request",
				Message:    "request contains invalid UTF-8 string",
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.Type() == jsonRawMessageType {
			raw := v.Bytes()
			if len(bytes.TrimSpace(raw)) > 0 && (!utf8.Valid(raw) || hasLoneSurrogateEscape(raw)) {
				return &HostError{
					StatusCode: http.StatusBadRequest,
					Code:       "bad_request",
					Message:    "request contains invalid UTF-8 or unpaired surrogate in JSON",
				}
			}
			return nil
		}
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return nil
		}
		for i := 0; i < v.Len(); i++ {
			if err := validateBodyUTF8(v.Index(i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		for mr := v.MapRange(); mr.Next(); {
			if mr.Key().Kind() == reflect.String && !utf8.ValidString(mr.Key().String()) {
				return &HostError{
					StatusCode: http.StatusBadRequest,
					Code:       "bad_request",
					Message:    "request contains invalid UTF-8 map key",
				}
			}
			if err := validateBodyUTF8(mr.Value()); err != nil {
				return err
			}
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			if !t.Field(i).IsExported() {
				continue
			}
			if err := validateBodyUTF8(v.Field(i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func hasLoneSurrogateEscape(data []byte) bool {
	inString := false
	for i := 0; i < len(data); {
		b := data[i]
		if !inString {
			if b == '"' {
				inString = true
			}
			i++
			continue
		}
		if b == '"' {
			inString = false
			i++
			continue
		}
		if b != '\\' {
			i++
			continue
		}
		if i+1 >= len(data) {
			return false
		}
		if data[i+1] != 'u' {
			i += 2
			continue
		}
		if i+6 > len(data) {
			i += 2
			continue
		}
		u1, ok := parseHex4(data[i+2 : i+6])
		if !ok {
			i += 2
			continue
		}
		if u1 >= 0xD800 && u1 <= 0xDBFF {
			if i+12 <= len(data) && data[i+6] == '\\' && data[i+7] == 'u' {
				if u2, ok2 := parseHex4(data[i+8 : i+12]); ok2 && u2 >= 0xDC00 && u2 <= 0xDFFF {
					i += 12
					continue
				}
			}
			return true
		}
		if u1 >= 0xDC00 && u1 <= 0xDFFF {
			return true
		}
		i += 6
	}
	return false
}

func parseHex4(b []byte) (uint16, bool) {
	if len(b) < 4 {
		return 0, false
	}
	var v uint16
	for i := 0; i < 4; i++ {
		c := b[i]
		var nib uint16
		switch {
		case c >= '0' && c <= '9':
			nib = uint16(c - '0')
		case c >= 'a' && c <= 'f':
			nib = uint16(c-'a') + 10
		case c >= 'A' && c <= 'F':
			nib = uint16(c-'A') + 10
		default:
			return 0, false
		}
		v = (v << 4) | nib
	}
	return v, true
}

func (h *Host) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	if err := h.checkReady(); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var bodyReader io.Reader
	if body != nil {
		if err := validateBodyUTF8(reflect.ValueOf(body)); err != nil {
			return nil, err
		}
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.baseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}
	if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
	if gid := GestureIDFromContext(ctx); gid != "" {
		req.Header.Set("X-GVX-Gesture-ID", gid)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (h *Host) doJSON(ctx context.Context, applyDefaultTimeout bool, method, path string, reqBody, respBody any) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if applyDefaultTimeout {
		if _, hasDeadline := ctx.Deadline(); !hasDeadline {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, defaultHostTimeout)
			defer cancel()
		}
	}
	req, err := h.newRequest(ctx, method, path, reqBody)
	if err != nil {
		return 0, err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxHostRespBytes))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		he := &HostError{StatusCode: resp.StatusCode}
		_ = json.Unmarshal(data, he)
		if he.Message == "" && he.Err == "" && len(bytes.TrimSpace(data)) > 0 {
			he.Message = strings.TrimSpace(string(data))
		}
		return resp.StatusCode, he
	}
	if respBody != nil && len(bytes.TrimSpace(data)) > 0 {
		if !utf8.Valid(data) || hasLoneSurrogateEscape(data) {
			return resp.StatusCode, fmt.Errorf("extsdk: decode %s %s response: response body is not valid UTF-8 JSON", method, path)
		}
		if err := json.Unmarshal(data, respBody); err != nil {
			return resp.StatusCode, fmt.Errorf("extsdk: decode %s %s response: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

func (h *Host) Info(ctx context.Context) (HostInfo, error) {
	var out HostInfo
	_, err := h.doJSON(ctx, true, http.MethodGet, "/v1/host/info", nil, &out)
	return out, err
}

func (h *Host) Settings(ctx context.Context) (Settings, error) {
	var out Settings
	_, err := h.doJSON(ctx, true, http.MethodGet, "/v1/settings", nil, &out)
	return out, err
}

func (h *Host) StorageList(ctx context.Context, prefix string) ([]string, error) {
	if err := h.checkReady(); err != nil {
		return nil, err
	}
	if err := validateUTF8Param("storage prefix", prefix); err != nil {
		return nil, err
	}
	path := "/v1/storage"
	if prefix != "" {
		path += "?prefix=" + url.QueryEscape(prefix)
	}
	var raw json.RawMessage
	if _, err := h.doJSON(ctx, true, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	return decodeKeyList(raw)
}

func (h *Host) StorageGet(ctx context.Context, key string) (string, bool, error) {
	if err := h.checkReady(); err != nil {
		return "", false, err
	}
	if err := validateUTF8Param("storage key", key); err != nil {
		return "", false, err
	}
	var raw json.RawMessage
	status, err := h.doJSON(ctx, true, http.MethodGet, "/v1/storage/"+url.PathEscape(key), nil, &raw)
	if err != nil {
		if status == http.StatusNotFound || IsNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}
	val, err := decodeKVValue(raw)
	return val, true, err
}

func (h *Host) StorageSet(ctx context.Context, key, value string) error {
	if err := h.checkReady(); err != nil {
		return err
	}
	if err := validateUTF8Param("storage key", key); err != nil {
		return err
	}
	_, err := h.doJSON(ctx, true, http.MethodPut, "/v1/storage/"+url.PathEscape(key), map[string]string{"value": value}, nil)
	return err
}

// StoragePut is an alias for StorageSet.
func (h *Host) StoragePut(ctx context.Context, key, value string) error {
	return h.StorageSet(ctx, key, value)
}

func (h *Host) StorageDelete(ctx context.Context, key string) error {
	if err := h.checkReady(); err != nil {
		return err
	}
	if err := validateUTF8Param("storage key", key); err != nil {
		return err
	}
	_, err := h.doJSON(ctx, true, http.MethodDelete, "/v1/storage/"+url.PathEscape(key), nil, nil)
	return err
}

func (h *Host) StorageGetJSON(ctx context.Context, key string, dst any) (bool, error) {
	v, ok, err := h.StorageGet(ctx, key)
	if err != nil || !ok {
		return ok, err
	}
	if err := json.Unmarshal([]byte(v), dst); err != nil {
		return true, err
	}
	return true, nil
}

func (h *Host) StorageSetJSON(ctx context.Context, key string, val any) error {
	if err := h.checkReady(); err != nil {
		return err
	}
	if err := validateUTF8Param("storage key", key); err != nil {
		return err
	}
	if val != nil {
		if err := validateBodyUTF8(reflect.ValueOf(val)); err != nil {
			return err
		}
	}
	b, err := json.Marshal(val)
	if err != nil {
		return err
	}
	return h.StorageSet(ctx, key, string(b))
}

func (h *Host) SecretList(ctx context.Context) ([]string, error) {
	var raw json.RawMessage
	if _, err := h.doJSON(ctx, true, http.MethodGet, "/v1/secrets", nil, &raw); err != nil {
		return nil, err
	}
	return decodeKeyList(raw)
}

func (h *Host) SecretGet(ctx context.Context, key string) (string, bool, error) {
	if err := h.checkReady(); err != nil {
		return "", false, err
	}
	if err := validateUTF8Param("secret key", key); err != nil {
		return "", false, err
	}
	var raw json.RawMessage
	status, err := h.doJSON(ctx, true, http.MethodGet, "/v1/secrets/"+url.PathEscape(key), nil, &raw)
	if err != nil {
		if status == http.StatusNotFound || IsNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}
	val, err := decodeKVValue(raw)
	return val, true, err
}

func (h *Host) SecretSet(ctx context.Context, key, value string) error {
	if err := h.checkReady(); err != nil {
		return err
	}
	if err := validateUTF8Param("secret key", key); err != nil {
		return err
	}
	_, err := h.doJSON(ctx, true, http.MethodPut, "/v1/secrets/"+url.PathEscape(key), map[string]string{"value": value}, nil)
	return err
}

// SecretPut is an alias for SecretSet.
func (h *Host) SecretPut(ctx context.Context, key, value string) error {
	return h.SecretSet(ctx, key, value)
}

func (h *Host) SecretDelete(ctx context.Context, key string) error {
	if err := h.checkReady(); err != nil {
		return err
	}
	if err := validateUTF8Param("secret key", key); err != nil {
		return err
	}
	_, err := h.doJSON(ctx, true, http.MethodDelete, "/v1/secrets/"+url.PathEscape(key), nil, nil)
	return err
}

func (h *Host) SandboxList(ctx context.Context, ownedOnly bool) ([]SandboxInfo, error) {
	path := "/v1/sandboxes?owned=" + strconv.FormatBool(ownedOnly)
	var raw json.RawMessage
	if _, err := h.doJSON(ctx, true, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	if bytes.TrimSpace(raw)[0] == '[' {
		var list []SandboxInfo
		err := json.Unmarshal(raw, &list)
		return list, err
	}
	var wrap struct {
		Sandboxes []SandboxInfo `json:"sandboxes"`
	}
	err := json.Unmarshal(raw, &wrap)
	return wrap.Sandboxes, err
}

func (h *Host) SandboxCreate(ctx context.Context, req CreateSandboxRequest) (SandboxInfo, error) {
	var out SandboxInfo
	_, err := h.doJSON(ctx, true, http.MethodPost, "/v1/sandboxes", req, &out)
	return out, err
}

func (h *Host) SandboxStart(ctx context.Context, id string) (SandboxInfo, error) {
	if err := h.checkReady(); err != nil {
		return SandboxInfo{}, err
	}
	if err := validateUTF8Param("sandbox id", id); err != nil {
		return SandboxInfo{}, err
	}
	var out SandboxInfo
	_, err := h.doJSON(ctx, true, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/start", struct{}{}, &out)
	return out, err
}

func (h *Host) SandboxStop(ctx context.Context, id string) (SandboxInfo, error) {
	if err := h.checkReady(); err != nil {
		return SandboxInfo{}, err
	}
	if err := validateUTF8Param("sandbox id", id); err != nil {
		return SandboxInfo{}, err
	}
	var out SandboxInfo
	_, err := h.doJSON(ctx, true, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/stop", struct{}{}, &out)
	return out, err
}

func (h *Host) SandboxDelete(ctx context.Context, id string) error {
	if err := h.checkReady(); err != nil {
		return err
	}
	if err := validateUTF8Param("sandbox id", id); err != nil {
		return err
	}
	_, err := h.doJSON(ctx, true, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/delete", struct{}{}, nil)
	return err
}

func (h *Host) SandboxExec(ctx context.Context, id string, req ExecRequest) (ExecResult, error) {
	if err := h.checkReady(); err != nil {
		return ExecResult{}, err
	}
	if err := validateUTF8Param("sandbox id", id); err != nil {
		return ExecResult{}, err
	}
	var out ExecResult
	_, err := h.doJSON(ctx, false, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/exec", req, &out)
	return out, err
}

func (h *Host) SandboxLogs(ctx context.Context, id string, tail int) (string, error) {
	if err := h.checkReady(); err != nil {
		return "", err
	}
	if err := validateUTF8Param("sandbox id", id); err != nil {
		return "", err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultHostTimeout)
		defer cancel()
	}
	path := "/v1/sandboxes/" + url.PathEscape(id) + "/logs"
	if tail > 0 {
		path += "?tail=" + strconv.Itoa(tail)
	}
	req, err := h.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxHostRespBytes))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		he := &HostError{StatusCode: resp.StatusCode}
		_ = json.Unmarshal(data, he)
		if he.Message == "" && he.Err == "" {
			he.Message = strings.TrimSpace(string(data))
		}
		return "", he
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var wrap struct {
			Logs string `json:"logs"`
		}
		if json.Unmarshal(trimmed, &wrap) == nil {
			return wrap.Logs, nil
		}
	}
	return string(data), nil
}

func (h *Host) SandboxLogsFollow(ctx context.Context, id string, tail int, fn func(string) error) error {
	if err := h.checkReady(); err != nil {
		return err
	}
	if err := validateUTF8Param("sandbox id", id); err != nil {
		return err
	}
	if fn == nil {
		return errors.New("extsdk: log callback is nil")
	}
	q := url.Values{}
	q.Set("follow", "true")
	if tail > 0 {
		q.Set("tail", strconv.Itoa(tail))
	}
	path := "/v1/sandboxes/" + url.PathEscape(id) + "/logs?" + q.Encode()
	return h.streamSSE(ctx, path, func(ev HostEvent) error {
		var wrap struct {
			Line string `json:"line"`
		}
		if len(ev.Data) > 0 && ev.Data[0] == '{' && json.Unmarshal(ev.Data, &wrap) == nil && wrap.Line != "" {
			return fn(wrap.Line)
		}
		if len(ev.Data) > 0 && ev.Data[0] == '"' {
			var s string
			if json.Unmarshal(ev.Data, &s) == nil {
				return fn(s)
			}
		}
		return fn(string(ev.Data))
	})
}

func (h *Host) SnapshotCreate(ctx context.Context, id string, req CreateSnapshotRequest) (SnapshotInfo, error) {
	if err := h.checkReady(); err != nil {
		return SnapshotInfo{}, err
	}
	if err := validateUTF8Param("sandbox id", id); err != nil {
		return SnapshotInfo{}, err
	}
	var out SnapshotInfo
	_, err := h.doJSON(ctx, false, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/snapshots", req, &out)
	return out, err
}

func (h *Host) SnapshotList(ctx context.Context, id string) ([]SnapshotInfo, error) {
	if err := h.checkReady(); err != nil {
		return nil, err
	}
	if err := validateUTF8Param("sandbox id", id); err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if _, err := h.doJSON(ctx, true, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(id)+"/snapshots", nil, &raw); err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	if bytes.TrimSpace(raw)[0] == '[' {
		var list []SnapshotInfo
		err := json.Unmarshal(raw, &list)
		return list, err
	}
	var wrap struct {
		Snapshots []SnapshotInfo `json:"snapshots"`
	}
	err := json.Unmarshal(raw, &wrap)
	return wrap.Snapshots, err
}

func (h *Host) Restore(ctx context.Context, id string, req RestoreRequest) (RestoreResult, error) {
	if err := h.checkReady(); err != nil {
		return RestoreResult{}, err
	}
	if err := validateUTF8Param("sandbox id", id); err != nil {
		return RestoreResult{}, err
	}
	var out RestoreResult
	_, err := h.doJSON(ctx, false, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/restore", req, &out)
	return out, err
}

func (h *Host) SnapshotDelete(ctx context.Context, id, name string) error {
	if err := h.checkReady(); err != nil {
		return err
	}
	if err := validateUTF8Param("sandbox id", id); err != nil {
		return err
	}
	if err := validateUTF8Param("snapshot name", name); err != nil {
		return err
	}
	_, err := h.doJSON(ctx, true, http.MethodDelete, "/v1/sandboxes/"+url.PathEscape(id)+"/snapshots/"+url.PathEscape(name), nil, nil)
	return err
}

func (h *Host) SetBadge(ctx context.Context, badge BadgeRequest) error {
	_, err := h.doJSON(ctx, true, http.MethodPost, "/v1/ui/badge", badge, nil)
	return err
}

func (h *Host) Toast(ctx context.Context, level, message string) error {
	_, err := h.doJSON(ctx, true, http.MethodPost, "/v1/ui/toast", map[string]string{
		"level":   level,
		"message": message,
	}, nil)
	return err
}

func (h *Host) PickFolder(ctx context.Context) (string, bool, error) {
	var out struct {
		Cancelled bool   `json:"cancelled"`
		Path      string `json:"path"`
	}
	if _, err := h.doJSON(ctx, false, http.MethodPost, "/v1/ui/pick-folder", struct{}{}, &out); err != nil {
		return "", false, err
	}
	if out.Cancelled || out.Path == "" {
		return "", false, nil
	}
	return out.Path, true, nil
}

func (h *Host) OpenTerminal(ctx context.Context, req OpenTerminalRequest) error {
	_, err := h.doJSON(ctx, true, http.MethodPost, "/v1/ui/open-terminal", req, nil)
	return err
}

func (h *Host) OpenURL(ctx context.Context, rawURL string) error {
	_, err := h.doJSON(ctx, true, http.MethodPost, "/v1/ui/open-url", map[string]string{"url": rawURL}, nil)
	return err
}

func (h *Host) Copy(ctx context.Context, text string) error {
	_, err := h.doJSON(ctx, true, http.MethodPost, "/v1/ui/copy", map[string]string{"text": text}, nil)
	return err
}

// SubscribeEvents opens GET /v1/events SSE stream and invokes fn for each HostEvent until ctx cancels or EOF/error.
func (h *Host) SubscribeEvents(ctx context.Context, fn func(HostEvent) error) error {
	if fn == nil {
		return errors.New("extsdk: event callback is nil")
	}
	return h.streamSSE(ctx, "/v1/events", fn)
}

func (h *Host) streamSSE(ctx context.Context, path string, fn func(HostEvent) error) error {
	req, err := h.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		he := &HostError{StatusCode: resp.StatusCode}
		_ = json.Unmarshal(data, he)
		if he.Message == "" && he.Err == "" {
			he.Message = strings.TrimSpace(string(data))
		}
		return he
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var evType, evID string
	var dataLines []string
	flush := func() error {
		if len(dataLines) == 0 && evType == "" && evID == "" {
			return nil
		}
		t := evType
		if t == "" {
			t = "message"
		}
		joined := strings.Join(dataLines, "\n")
		ev := HostEvent{
			ID:   evID,
			Type: t,
			Data: json.RawMessage([]byte(joined)),
		}
		evType, evID = "", ""
		dataLines = dataLines[:0]
		return fn(ev)
	}

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, val, hasColon := strings.Cut(line, ":")
		if hasColon {
			val = strings.TrimPrefix(val, " ")
		}
		switch field {
		case "event":
			evType = val
		case "id":
			evID = val
		case "data":
			dataLines = append(dataLines, val)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}

func decodeKeyList(raw json.RawMessage) ([]string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var keys []string
		err := json.Unmarshal(trimmed, &keys)
		return keys, err
	}
	var wrap struct {
		Keys []string `json:"keys"`
	}
	err := json.Unmarshal(trimmed, &wrap)
	return wrap.Keys, err
}

func decodeKVValue(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "", nil
	}
	if trimmed[0] == '"' {
		var s string
		err := json.Unmarshal(trimmed, &s)
		return s, err
	}
	var wrap struct {
		Value string `json:"value"`
	}
	err := json.Unmarshal(trimmed, &wrap)
	return wrap.Value, err
}
