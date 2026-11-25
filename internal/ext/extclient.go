package ext

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
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ironpark/gvx-desktop/internal/uiblock"
)

const (
	extClientRPCTimeout = 30 * time.Second
	maxExtRespBytes     = 4 << 20
)

type extClient struct {
	httpClient *http.Client
	sseClient  *http.Client
	baseURL    string
	token      string
}

func newExtClient(endpoint, token string) (*extClient, error) {
	if strings.TrimSpace(endpoint) == "" {
		return nil, errors.New("extension endpoint is empty")
	}
	makeTransport := func() *http.Transport {
		return &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			DisableCompression:  true,
			MaxIdleConns:        8,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     60 * time.Second,
		}
	}

	var (
		baseURL string
		trRPC   = makeTransport()
		trSSE   = makeTransport()
	)
	if strings.HasPrefix(endpoint, "unix://") {
		sockPath := strings.TrimPrefix(endpoint, "unix://")
		if sockPath == "" {
			return nil, errors.New("empty extension unix socket path")
		}
		dialFn := func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sockPath)
		}
		trRPC.DialContext = dialFn
		trSSE.DialContext = dialFn
		baseURL = "http://unix"
	} else {
		raw := endpoint
		if !strings.Contains(raw, "://") {
			raw = "http://" + raw
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("invalid extension endpoint %q", endpoint)
		}
		baseURL = strings.TrimRight(u.String(), "/")
	}

	noRedirect := func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &extClient{
		httpClient: &http.Client{Transport: trRPC, Timeout: extClientRPCTimeout, CheckRedirect: noRedirect},
		sseClient:  &http.Client{Transport: trSSE, CheckRedirect: noRedirect},
		baseURL:    baseURL,
		token:      token,
	}, nil
}

func (c *extClient) closeIdle() {
	if c == nil {
		return
	}
	if tr, ok := c.httpClient.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
	if tr, ok := c.sseClient.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}

func (c *extClient) newRequest(ctx context.Context, method, reqPath string, body any, gestureID string) (*http.Request, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+reqPath, reader)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if gestureID != "" {
		req.Header.Set("X-GVX-Gesture-ID", gestureID)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *extClient) doJSON(ctx context.Context, method, reqPath string, reqBody, respBody any, gestureID string) error {
	req, err := c.newRequest(ctx, method, reqPath, reqBody, gestureID)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxExtRespBytes))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errWrap struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Error   string `json:"error"`
		}
		_ = json.Unmarshal(data, &errWrap)
		msg := errWrap.Message
		if msg == "" {
			msg = errWrap.Error
		}
		if msg == "" {
			msg = strings.TrimSpace(string(data))
		}
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		if errWrap.Code != "" {
			return fmt.Errorf("extension HTTP %d (%s): %s", resp.StatusCode, errWrap.Code, msg)
		}
		return fmt.Errorf("extension HTTP %d: %s", resp.StatusCode, msg)
	}
	if respBody != nil && len(bytes.TrimSpace(data)) > 0 {
		if !utf8.Valid(data) {
			return fmt.Errorf("decode %s %s response: invalid UTF-8", method, reqPath)
		}
		if hasLoneSurrogateEscape(data) {
			return fmt.Errorf("decode %s %s response: unpaired UTF-16 surrogate", method, reqPath)
		}
		if rawPtr, ok := respBody.(*json.RawMessage); ok {
			*rawPtr = append((*rawPtr)[:0], data...)
			return nil
		}
		if err := json.Unmarshal(data, respBody); err != nil {
			return fmt.Errorf("decode %s %s response: %w", method, reqPath, err)
		}
	}
	return nil
}

func (c *extClient) Health(ctx context.Context) error {
	var out struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/health", nil, &out, ""); err != nil {
		return err
	}
	if out.Status != "" && out.Status != "ok" {
		return fmt.Errorf("unhealthy extension status %q", out.Status)
	}
	return nil
}

func (c *extClient) SendViewEvent(ctx context.Context, viewID string, ev uiblock.Event, gestureID string) error {
	payload := map[string]any{
		"node_id": ev.Node,
		"node":    ev.Node,
		"kind":    ev.Kind,
		"value":   ev.Value,
		"values":  ev.Values,
		"locale":  ev.Locale,
	}
	return c.doJSON(ctx, http.MethodPost, "/v1/views/"+url.PathEscape(viewID)+"/event", payload, nil, gestureID)
}

func (c *extClient) RunCommand(ctx context.Context, cmdID, locale, gestureID string) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/commands/"+url.PathEscape(cmdID), map[string]string{
		"locale": locale,
	}, nil, gestureID)
}

func (c *extClient) InvokeTool(ctx context.Context, toolID string, args json.RawMessage, locale string) (json.RawMessage, error) {
	body := map[string]any{
		"locale": locale,
	}
	if len(bytes.TrimSpace(args)) > 0 {
		body["args"] = json.RawMessage(args)
	} else {
		body["args"] = json.RawMessage("{}")
	}
	var raw json.RawMessage
	err := c.doJSON(ctx, http.MethodPost, "/v1/tools/"+url.PathEscape(toolID), body, &raw, "")
	return raw, err
}

func (c *extClient) SettingsChanged(ctx context.Context, values map[string]any) error {
	if values == nil {
		values = map[string]any{}
	}
	return c.doJSON(ctx, http.MethodPost, "/v1/settings/changed", values, nil, "")
}

func (c *extClient) Shutdown(ctx context.Context) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/shutdown", struct{}{}, nil, "")
}

type httpStatusError struct {
	code int
	msg  string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("view stream HTTP %d: %s", e.code, e.msg)
}

func (c *extClient) StreamView(ctx context.Context, viewID, locale string, send func(uiblock.Update) error) error {
	if send == nil {
		return errors.New("view update callback is nil")
	}
	if locale == "" {
		locale = "en"
	}
	pathStr := "/v1/views/" + url.PathEscape(viewID) + "/stream?locale=" + url.QueryEscape(locale)
	backoff := 50 * time.Millisecond
	const maxBackoff = 2 * time.Second

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		needReconnect, err := c.streamViewOnce(ctx, pathStr, send, func() {
			backoff = 50 * time.Millisecond
		})
		if err == nil && !needReconnect {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var httpErr *httpStatusError
		if errors.As(err, &httpErr) && !needReconnect {
			_ = send(uiblock.Update{Error: httpErr.Error()})
			return nil
		}
		if err != nil && !needReconnect {
			return err
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

func (c *extClient) streamViewOnce(
	ctx context.Context,
	pathStr string,
	send func(uiblock.Update) error,
	onHealthyTree func(),
) (reconnect bool, err error) {
	req, err := c.newRequest(ctx, http.MethodGet, pathStr, nil, "")
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.sseClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return true, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		msg := strings.TrimSpace(string(data))
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		httpErr := &httpStatusError{code: resp.StatusCode, msg: msg}
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			return true, httpErr
		}
		return false, httpErr
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var (
		evType    string
		evID      string
		dataLines []string
		lastRoot  *uiblock.Node
		seq       uint64
	)

	flush := func() (stop bool, needReconnect bool, ferr error) {
		if len(dataLines) == 0 && evType == "" && evID == "" {
			return false, false, nil
		}
		t := evType
		if t == "" {
			t = "message"
		}
		var parsedSeq uint64
		if evID != "" {
			if n, perr := strconv.ParseUint(evID, 10, 64); perr == nil {
				parsedSeq = n
			}
		}
		if parsedSeq == 0 {
			seq++
			parsedSeq = seq
		} else {
			seq = parsedSeq
		}
		payload := []byte(strings.Join(dataLines, "\n"))
		evType, evID = "", ""
		dataLines = dataLines[:0]

		switch t {
		case "tree":
			tree, rerr := uiblock.Read(payload)
			if rerr != nil {
				lastRoot = nil
				if serr := send(uiblock.Update{Seq: parsedSeq, Error: rerr.Error()}); serr != nil {
					return true, false, serr
				}
				return false, false, nil
			}
			lastRoot = tree.Root
			if onHealthyTree != nil {
				onHealthyTree()
			}
			if serr := send(uiblock.Update{Seq: parsedSeq, Tree: tree.Root}); serr != nil {
				return true, false, serr
			}
		case "patch":
			if lastRoot == nil {
				return true, true, errors.New("received patch before initial tree")
			}
			if !utf8.Valid(payload) {
				return true, true, errors.New("invalid patch JSON: must be valid UTF-8")
			}
			if hasLoneSurrogateEscape(payload) {
				return true, true, errors.New("invalid patch JSON: contains unpaired UTF-16 surrogate")
			}
			var wrap struct {
				Ops []*uiblock.Op `json:"ops"`
			}
			if uerr := json.Unmarshal(payload, &wrap); uerr != nil {
				return true, true, fmt.Errorf("invalid patch JSON: %w", uerr)
			}
			nextRoot, aerr := uiblock.Apply(lastRoot, wrap.Ops)
			if aerr != nil {
				return true, true, fmt.Errorf("apply patch: %w", aerr)
			}
			if verr := validatePatchedNodeTree(nextRoot); verr != nil {
				return true, true, fmt.Errorf("invalid patched tree: %w", verr)
			}
			lastRoot = nextRoot
			if serr := send(uiblock.Update{Seq: parsedSeq, Ops: wrap.Ops, Tree: nextRoot}); serr != nil {
				return true, false, serr
			}
		case "error":
			if !utf8.Valid(payload) {
				return true, true, errors.New("invalid error event: must be valid UTF-8")
			}
			if hasLoneSurrogateEscape(payload) {
				return true, true, errors.New("invalid error event JSON: contains unpaired UTF-16 surrogate")
			}
			var wrap struct {
				Message string `json:"message"`
			}
			msg := strings.TrimSpace(string(payload))
			if json.Unmarshal(payload, &wrap) == nil && wrap.Message != "" {
				msg = wrap.Message
			}
			if msg == "" {
				msg = "view render error"
			}
			if serr := send(uiblock.Update{Seq: parsedSeq, Error: msg}); serr != nil {
				return true, false, serr
			}
		}
		return false, false, nil
	}

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			stop, needReconn, ferr := flush()
			if stop {
				return needReconn, ferr
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
	if len(dataLines) > 0 || evType != "" || evID != "" {
		stop, needReconn, ferr := flush()
		if stop {
			return needReconn, ferr
		}
	}
	if serr := scanner.Err(); serr != nil {
		return true, serr
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return true, io.EOF
}

// validatePatchedNodeTree converts a patched *uiblock.Node tree back to the raw
// JSON schema expected by uiblock.Read so that every patched node undergoes full
// kind, tone, variant, icon, limit, path-integrity, and spoofing validation.
func validatePatchedNodeTree(root *uiblock.Node) error {
	if root == nil {
		return errors.New("patched tree is nil")
	}
	if err := validatePatchedNodeIDs(root, true); err != nil {
		return err
	}
	raw := nodeToRawValidate(root, "", -1)
	b, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	tree, err := uiblock.Read(b)
	if err != nil {
		return err
	}
	return comparePatchedNodeIDs(root, tree.Root)
}

func validatePatchedNodeIDs(n *uiblock.Node, isRoot bool) error {
	if n == nil {
		return errors.New("patched node is nil")
	}
	if !utf8.ValidString(n.ID) || strings.ContainsFunc(n.ID, uiblock.Spoofing) {
		return fmt.Errorf("patched node id %q contains invalid UTF-8 or spoofing characters", n.ID)
	}
	if isRoot && n.ID != "" {
		return fmt.Errorf("patched root node id %q must be empty", n.ID)
	}
	seen := make(map[string]bool, len(n.Children))
	prefix := n.ID + "/"
	for i, c := range n.Children {
		if c == nil {
			return fmt.Errorf("patched child %d of %q is nil", i, n.ID)
		}
		if !strings.HasPrefix(c.ID, prefix) {
			return fmt.Errorf("patched child id %q does not match parent %q", c.ID, n.ID)
		}
		seg := strings.TrimPrefix(c.ID, prefix)
		if seg == "" || strings.ContainsRune(seg, '/') {
			return fmt.Errorf("patched child id %q has invalid segment under %q", c.ID, n.ID)
		}
		if seen[c.ID] {
			return fmt.Errorf("duplicate patched sibling id %q", c.ID)
		}
		seen[c.ID] = true
		if err := validatePatchedNodeIDs(c, false); err != nil {
			return err
		}
	}
	return nil
}

func comparePatchedNodeIDs(patched, canonical *uiblock.Node) error {
	if patched == nil || canonical == nil {
		return errors.New("patched node mismatch")
	}
	if patched.ID != canonical.ID {
		return fmt.Errorf("patched node id %q does not match canonical id %q", patched.ID, canonical.ID)
	}
	if len(patched.Children) != len(canonical.Children) {
		return fmt.Errorf("patched node %q child count mismatch", patched.ID)
	}
	for i := range patched.Children {
		if err := comparePatchedNodeIDs(patched.Children[i], canonical.Children[i]); err != nil {
			return err
		}
	}
	return nil
}

type rawValidateNode struct {
	Kind        string               `json:"kind,omitempty"`
	Key         string               `json:"key,omitempty"`
	Text        string               `json:"text,omitempty"`
	Tone        string               `json:"tone,omitempty"`
	Variant     string               `json:"variant,omitempty"`
	Icon        string               `json:"icon,omitempty"`
	Weight      string               `json:"weight,omitempty"`
	Size        string               `json:"size,omitempty"`
	Value       string               `json:"value,omitempty"`
	Checked     bool                 `json:"checked,omitempty"`
	Options     []string             `json:"options,omitempty"`
	Progress    float64              `json:"progress,omitempty"`
	Language    string               `json:"language,omitempty"`
	Placeholder string               `json:"placeholder,omitempty"`
	Disabled    bool                 `json:"disabled,omitempty"`
	Grow        float64              `json:"grow,omitempty"`
	Gap         float64              `json:"gap,omitempty"`
	Columns     int                  `json:"columns,omitempty"`
	Ratio       *float64             `json:"ratio,omitempty"`
	Fill        bool                 `json:"fill,omitempty"`
	OnTap       json.RawMessage      `json:"onTap,omitempty"`
	OnChange    json.RawMessage      `json:"onChange,omitempty"`
	OnLink      json.RawMessage      `json:"onLink,omitempty"`
	OnDrop      json.RawMessage      `json:"onDrop,omitempty"`
	OnCheck     json.RawMessage      `json:"onCheck,omitempty"`
	Drag        string               `json:"drag,omitempty"`
	DragGroup   string               `json:"dragGroup,omitempty"`
	DropAccept  []string             `json:"dropAccept,omitempty"`
	Direction   string               `json:"direction,omitempty"`
	MaxHeight   float64              `json:"maxHeight,omitempty"`
	Children    []*rawValidateNode   `json:"children,omitempty"`
	Chart       string               `json:"chart,omitempty"`
	Series      []*uiblock.Series    `json:"series,omitempty"`
	Labels      []string             `json:"labels,omitempty"`
	GraphNodes  []*uiblock.GraphNode `json:"graphNodes,omitempty"`
	GraphEdges  []*uiblock.GraphEdge `json:"graphEdges,omitempty"`
	Badges      []*uiblock.Badge     `json:"badges,omitempty"`
	Check       string               `json:"check,omitempty"`
	ContextMenu string               `json:"contextMenu,omitempty"`
	ContextArg  string               `json:"contextArg,omitempty"`
	Rows        []*uiblock.Row       `json:"rows,omitempty"`
}

func nodeToRawValidate(n *uiblock.Node, parentID string, childIdx int) *rawValidateNode {
	if n == nil {
		return nil
	}
	var key string
	if childIdx >= 0 && n.ID != "" {
		seg := strings.TrimPrefix(n.ID, parentID+"/")
		if seg != fmt.Sprintf("#%d", childIdx) {
			key = seg
		}
	}
	text := n.Text
	if (n.Kind == uiblock.KindInput || n.Kind == uiblock.KindTextarea) && n.Placeholder != "" && n.Text == n.Placeholder {
		text = ""
	}
	out := &rawValidateNode{
		Kind:        n.Kind,
		Key:         key,
		Text:        text,
		Tone:        n.Tone,
		Variant:     n.Variant,
		Icon:        n.Icon,
		Weight:      n.Weight,
		Size:        n.Size,
		Value:       n.Value,
		Checked:     n.Checked,
		Options:     n.Options,
		Progress:    n.Progress,
		Language:    n.Language,
		Placeholder: n.Placeholder,
		Disabled:    n.Disabled,
		Grow:        n.Grow,
		Gap:         n.Gap,
		Columns:     n.Columns,
		Fill:        n.Fill,
		Drag:        n.Drag,
		DragGroup:   n.DragGroup,
		DropAccept:  n.DropAccept,
		Direction:   n.Direction,
		MaxHeight:   n.MaxHeight,
		Chart:       n.Chart,
		Series:      n.Series,
		Labels:      n.Labels,
		GraphNodes:  n.GraphNodes,
		GraphEdges:  n.GraphEdges,
		Badges:      n.Badges,
		Check:       n.Check,
		ContextMenu: n.ContextMenu,
		ContextArg:  n.ContextArg,
		Rows:        n.Rows,
	}
	if n.Kind == uiblock.KindSplit || n.Ratio != 0 {
		r := n.Ratio
		out.Ratio = &r
	}
	if n.Tap {
		out.OnTap = json.RawMessage(`["ok"]`)
	}
	if n.Change {
		if n.Kind == uiblock.KindMarkdown {
			out.OnLink = json.RawMessage(`["ok"]`)
		} else {
			out.OnChange = json.RawMessage(`["ok"]`)
		}
	}
	if n.Drop {
		out.OnDrop = json.RawMessage(`["ok"]`)
	}
	if n.Checks {
		out.OnCheck = json.RawMessage(`["ok"]`)
	}
	if len(n.Children) > 0 {
		out.Children = make([]*rawValidateNode, len(n.Children))
		for i, c := range n.Children {
			out.Children[i] = nodeToRawValidate(c, n.ID, i)
		}
	}
	return out
}
