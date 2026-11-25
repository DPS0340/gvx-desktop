package extsdk

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"
)

// DefaultCoalesceInterval is the minimum interval between coalesced SSE updates on a view stream.
const DefaultCoalesceInterval = 100 * time.Millisecond

const streamKeepaliveInterval = 25 * time.Second

// Diff operation constants matching internal/uiblock.
const (
	OpReplace = "replace"
	OpUpdate  = "update"
	OpInsert  = "insert"
	OpRemove  = "remove"
	OpMove    = "move"
)

// Op describes a tree patch operation matching internal/uiblock.Op's JSON schema.
type Op struct {
	Op     string    `json:"op,omitempty"`
	ID     string    `json:"id,omitempty"`
	Parent string    `json:"parent,omitempty"`
	Index  int       `json:"index,omitempty"`
	Node   *wireNode `json:"node,omitempty"`
}

// wireNode mirrors internal/uiblock.Node's JSON schema for event: patch ops.
type wireNode struct {
	ID          string           `json:"id,omitempty"`
	Kind        string           `json:"kind,omitempty"`
	Text        string           `json:"text,omitempty"`
	Tone        string           `json:"tone,omitempty"`
	Variant     string           `json:"variant,omitempty"`
	Icon        string           `json:"icon,omitempty"`
	Weight      string           `json:"weight,omitempty"`
	Size        string           `json:"size,omitempty"`
	Value       string           `json:"value,omitempty"`
	Checked     bool             `json:"checked,omitempty"`
	Options     []string         `json:"options,omitempty"`
	Progress    float64          `json:"progress,omitempty"`
	Language    string           `json:"language,omitempty"`
	Placeholder string           `json:"placeholder,omitempty"`
	Disabled    bool             `json:"disabled,omitempty"`
	Grow        float64          `json:"grow,omitempty"`
	Gap         float64          `json:"gap,omitempty"`
	Columns     int              `json:"columns,omitempty"`
	Ratio       float64          `json:"ratio,omitempty"`
	Fill        bool             `json:"fill,omitempty"`
	Tap         bool             `json:"tap,omitempty"`
	Change      bool             `json:"change,omitempty"`
	Drag        string           `json:"drag,omitempty"`
	Drop        bool             `json:"drop,omitempty"`
	DragGroup   string           `json:"dragGroup,omitempty"`
	DropAccept  []string         `json:"dropAccept,omitempty"`
	Direction   string           `json:"direction,omitempty"`
	MaxHeight   float64          `json:"maxHeight,omitempty"`
	Children    []*wireNode      `json:"children,omitempty"`
	Chart       string           `json:"chart,omitempty"`
	Series      []*wireSeries    `json:"series,omitempty"`
	Labels      []string         `json:"labels,omitempty"`
	GraphNodes  []*wireGraphNode `json:"graphNodes,omitempty"`
	GraphEdges  []*wireGraphEdge `json:"graphEdges,omitempty"`
	Badges      []*wireBadge     `json:"badges,omitempty"`
	Check       string           `json:"check,omitempty"`
	Checks      bool             `json:"checks,omitempty"`
	ContextMenu string           `json:"contextMenu,omitempty"`
	ContextArg  string           `json:"contextArg,omitempty"`
	Rows        []*wireRow       `json:"rows,omitempty"`
}

type wireSeries struct {
	Name   string    `json:"name,omitempty"`
	Tone   string    `json:"tone,omitempty"`
	Values []float64 `json:"values,omitempty"`
}

type wireGraphNode struct {
	ID       string  `json:"id,omitempty"`
	Label    string  `json:"label,omitempty"`
	Group    string  `json:"group,omitempty"`
	Tone     string  `json:"tone,omitempty"`
	Subtitle string  `json:"subtitle,omitempty"`
	Weight   float64 `json:"weight,omitempty"`
}

type wireGraphEdge struct {
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Label  string `json:"label,omitempty"`
	Tone   string `json:"tone,omitempty"`
	Dashed bool   `json:"dashed,omitempty"`
}

type wireBadge struct {
	Text    string `json:"text,omitempty"`
	Tone    string `json:"tone,omitempty"`
	Variant string `json:"variant,omitempty"`
	Icon    string `json:"icon,omitempty"`
}

type wireRow struct {
	ID      string       `json:"id,omitempty"`
	Parents []string     `json:"parents,omitempty"`
	Text    string       `json:"text,omitempty"`
	Detail  string       `json:"detail,omitempty"`
	Meta    string       `json:"meta,omitempty"`
	Badges  []*wireBadge `json:"badges,omitempty"`
	Tone    string       `json:"tone,omitempty"`
	Strong  bool         `json:"strong,omitempty"`
	Icon    string       `json:"icon,omitempty"`
}

type indexedNode struct {
	kind        string
	onTap       json.RawMessage
	onChange    json.RawMessage
	onSubmit    json.RawMessage
	onDrop      json.RawMessage
	onCheck     json.RawMessage
	dagRows     map[string]bool
	contextMenu string
	contextArg  string
	isCtxMenu   bool
	key         string
}

type viewIndex struct {
	rootKind        string
	nodes           map[string]indexedNode
	itemContextMenu map[string]string
	contextArgs     map[string]map[string]bool
	inputKinds      map[string]string
	keyUnique       map[string]string
}

func isPlaceholderKind(kind string) bool {
	switch kind {
	case "input", "textarea", "button", "menu", "tree_item", "checkbox", "icon", "pill", "chip":
		return true
	default:
		return false
	}
}

func isFormValueKind(kind string) bool {
	switch kind {
	case "input", "textarea", "toggle", "checkbox", "select", "tabs":
		return true
	default:
		return false
	}
}

func hasRawMessage(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

func normalizeAndIndex(root Node) (*wireNode, *viewIndex) {
	idx := &viewIndex{
		rootKind:        root.raw.Kind,
		nodes:           make(map[string]indexedNode),
		itemContextMenu: make(map[string]string),
		contextArgs:     make(map[string]map[string]bool),
		inputKinds:      make(map[string]string),
		keyUnique:       make(map[string]string),
	}
	keyCounts := make(map[string]int)
	keyToID := make(map[string]string)

	var walk func(n Node, id, ctxMenuKey string) *wireNode
	walk = func(n Node, id, ctxMenuKey string) *wireNode {
		r := n.raw
		checkVal := r.Check
		if r.Kind == "checkbox" && checkVal == "" {
			checkVal = "off"
		}
		var ratio float64
		if r.Kind == "split" {
			if r.Ratio == nil {
				ratio = 0.5
			} else {
				ratio = *r.Ratio
			}
		}
		text := r.Text
		if (r.Kind == "input" || r.Kind == "textarea") && text == "" {
			text = r.Placeholder
		}
		placeholder := ""
		if r.Placeholder != "" && isPlaceholderKind(r.Kind) {
			placeholder = r.Placeholder
		}
		var labels []string
		if r.Kind == "table" || r.Kind == "chart" {
			labels = slices.Clone(r.Labels)
		}

		onChange := r.OnChange
		if r.Kind == "markdown" && hasRawMessage(r.OnLink) {
			onChange = r.OnLink
		}

		wn := &wireNode{
			ID:          id,
			Kind:        r.Kind,
			Text:        text,
			Tone:        r.Tone,
			Variant:     r.Variant,
			Icon:        r.Icon,
			Weight:      r.Weight,
			Size:        r.Size,
			Value:       r.Value,
			Checked:     r.Checked,
			Options:     slices.Clone(r.Options),
			Progress:    r.Progress,
			Language:    r.Language,
			Placeholder: placeholder,
			Disabled:    r.Disabled,
			Grow:        r.Grow,
			Gap:         r.Gap,
			Columns:     r.Columns,
			Ratio:       ratio,
			Fill:        r.Fill,
			Tap:         hasRawMessage(r.OnTap),
			Change:      hasRawMessage(onChange) || hasRawMessage(r.OnSubmit),
			Drag:        r.Drag,
			Drop:        hasRawMessage(r.OnDrop),
			DragGroup:   r.DragGroup,
			DropAccept:  slices.Clone(r.DropAccept),
			Direction:   r.Direction,
			MaxHeight:   r.MaxHeight,
			Chart:       r.Chart,
			Labels:      labels,
			Check:       checkVal,
			Checks:      hasRawMessage(r.OnCheck),
			ContextMenu: r.ContextMenu,
			ContextArg:  r.ContextArg,
		}

		for _, s := range r.Series {
			wn.Series = append(wn.Series, &wireSeries{
				Name:   s.Name,
				Tone:   s.Tone,
				Values: slices.Clone(s.Values),
			})
		}
		for _, gn := range r.GraphNodes {
			w := gn.Weight
			if w == 0 {
				w = 1.0
			}
			wn.GraphNodes = append(wn.GraphNodes, &wireGraphNode{
				ID:       gn.ID,
				Label:    gn.Label,
				Group:    gn.Group,
				Tone:     gn.Tone,
				Subtitle: gn.Subtitle,
				Weight:   w,
			})
		}
		for _, ge := range r.GraphEdges {
			wn.GraphEdges = append(wn.GraphEdges, &wireGraphEdge{
				From:   ge.From,
				To:     ge.To,
				Label:  ge.Label,
				Tone:   ge.Tone,
				Dashed: ge.Dashed,
			})
		}
		for _, b := range r.Badges {
			wn.Badges = append(wn.Badges, &wireBadge{
				Text:    b.Text,
				Tone:    b.Tone,
				Variant: b.Variant,
				Icon:    b.Icon,
			})
		}

		var dagRows map[string]bool
		if r.Kind == "dag" && len(r.Rows) > 0 {
			dagRows = make(map[string]bool, len(r.Rows))
		}
		for _, row := range r.Rows {
			if dagRows != nil {
				dagRows[row.ID] = true
			}
			var rbadges []*wireBadge
			for _, b := range row.Badges {
				rbadges = append(rbadges, &wireBadge{
					Text:    b.Text,
					Tone:    b.Tone,
					Variant: b.Variant,
					Icon:    b.Icon,
				})
			}
			wn.Rows = append(wn.Rows, &wireRow{
				ID:      row.ID,
				Parents: slices.Clone(row.Parents),
				Text:    row.Text,
				Detail:  row.Detail,
				Meta:    row.Meta,
				Badges:  rbadges,
				Tone:    row.Tone,
				Strong:  row.Strong,
				Icon:    row.Icon,
			})
		}

		isCtxMenu := r.Kind == "menu" && r.Variant == "context"
		if isCtxMenu && r.Key != "" {
			ctxMenuKey = r.Key
		}
		if r.Kind == "menu_item" && ctxMenuKey != "" {
			idx.itemContextMenu[id] = ctxMenuKey
		}
		if r.ContextMenu != "" {
			args := idx.contextArgs[r.ContextMenu]
			if args == nil {
				args = make(map[string]bool)
				idx.contextArgs[r.ContextMenu] = args
			}
			if r.Kind == "dag" {
				for _, row := range r.Rows {
					args[row.ID] = true
				}
				if r.ContextArg != "" {
					args[r.ContextArg] = true
				}
			} else {
				args[r.ContextArg] = true
			}
		}

		idx.nodes[id] = indexedNode{
			kind:        r.Kind,
			onTap:       r.OnTap,
			onChange:    onChange,
			onSubmit:    r.OnSubmit,
			onDrop:      r.OnDrop,
			onCheck:     r.OnCheck,
			dagRows:     dagRows,
			contextMenu: r.ContextMenu,
			contextArg:  r.ContextArg,
			isCtxMenu:   isCtxMenu,
			key:         r.Key,
		}
		if isFormValueKind(r.Kind) {
			idx.inputKinds[id] = r.Kind
			if r.Key != "" {
				keyCounts[r.Key]++
				keyToID[r.Key] = id
			}
		}

		for i, c := range r.Children {
			k := c.raw.Key
			if k == "" {
				k = fmt.Sprintf("#%d", i)
			}
			childID := id + "/" + k
			wn.Children = append(wn.Children, walk(c, childID, ctxMenuKey))
		}
		return wn
	}

	wire := walk(root, "", "")
	for k, count := range keyCounts {
		if count == 1 {
			idx.keyUnique[k] = keyToID[k]
		}
	}
	return wire, idx
}

func hasActionOrContext(n indexedNode) bool {
	return hasRawMessage(n.onTap) ||
		hasRawMessage(n.onChange) ||
		hasRawMessage(n.onSubmit) ||
		hasRawMessage(n.onDrop) ||
		hasRawMessage(n.onCheck) ||
		n.contextMenu != "" ||
		n.contextArg != "" ||
		n.isCtxMenu
}

func nodeActionEqual(a, b indexedNode) bool {
	return a.kind == b.kind &&
		bytes.Equal(a.onTap, b.onTap) &&
		bytes.Equal(a.onChange, b.onChange) &&
		bytes.Equal(a.onSubmit, b.onSubmit) &&
		bytes.Equal(a.onDrop, b.onDrop) &&
		bytes.Equal(a.onCheck, b.onCheck) &&
		a.contextMenu == b.contextMenu &&
		a.contextArg == b.contextArg &&
		a.isCtxMenu == b.isCtxMenu &&
		a.key == b.key
}

func actionsAndContextEqual(a, b *viewIndex) bool {
	if a == nil || b == nil {
		return false
	}
	if a.rootKind != b.rootKind {
		return false
	}
	for id, na := range a.nodes {
		if !hasActionOrContext(na) {
			continue
		}
		nb, ok := b.nodes[id]
		if !ok || !nodeActionEqual(na, nb) {
			return false
		}
	}
	for id, nb := range b.nodes {
		if !hasActionOrContext(nb) {
			continue
		}
		na, ok := a.nodes[id]
		if !ok || !nodeActionEqual(na, nb) {
			return false
		}
	}
	if len(a.contextArgs) != len(b.contextArgs) {
		return false
	}
	for menuKey, argsA := range a.contextArgs {
		argsB, ok := b.contextArgs[menuKey]
		if !ok || len(argsA) != len(argsB) {
			return false
		}
		for arg := range argsA {
			if !argsB[arg] {
				return false
			}
		}
	}
	return true
}

// Diff computes internal/uiblock-compatible patch ops between two normalized trees.
func diffWireNodes(old, next *wireNode) []*Op {
	if old == nil && next == nil {
		return nil
	}
	if old == nil {
		return []*Op{{Op: OpReplace, ID: next.ID, Node: cloneWireNode(next)}}
	}
	if next == nil {
		return nil
	}
	var ops []*Op
	diffNodesRec(old, next, &ops)
	return ops
}

func diffNodesRec(old, next *wireNode, ops *[]*Op) {
	if old.Kind != next.Kind {
		*ops = append(*ops, &Op{Op: OpReplace, ID: next.ID, Node: cloneWireNode(next)})
		return
	}
	if !wireNodeOwnEqual(old, next) {
		*ops = append(*ops, &Op{Op: OpUpdate, ID: next.ID, Node: cloneOwnWireNode(next)})
	}
	byID := make(map[string]*wireNode, len(old.Children))
	for _, c := range old.Children {
		byID[c.ID] = c
	}
	wanted := make(map[string]bool, len(next.Children))
	for _, c := range next.Children {
		wanted[c.ID] = true
	}
	cur := make([]string, 0, len(old.Children))
	for _, c := range old.Children {
		if wanted[c.ID] {
			cur = append(cur, c.ID)
		} else {
			*ops = append(*ops, &Op{Op: OpRemove, ID: c.ID})
		}
	}
	for i, c := range next.Children {
		prev, had := byID[c.ID]
		switch {
		case !had:
			*ops = append(*ops, &Op{Op: OpInsert, Parent: next.ID, Index: i, Node: cloneWireNode(c)})
			cur = slices.Insert(cur, i, c.ID)
		case cur[i] != c.ID:
			*ops = append(*ops, &Op{Op: OpMove, ID: c.ID, Parent: next.ID, Index: i})
			idx := slices.Index(cur, c.ID)
			cur = slices.Delete(cur, idx, idx+1)
			cur = slices.Insert(cur, i, c.ID)
			diffNodesRec(prev, c, ops)
		default:
			diffNodesRec(prev, c, ops)
		}
	}
}

func cloneOwnWireNode(n *wireNode) *wireNode {
	if n == nil {
		return nil
	}
	c := *n
	c.Children = nil
	c.Options = slices.Clone(n.Options)
	c.DropAccept = slices.Clone(n.DropAccept)
	c.Labels = slices.Clone(n.Labels)
	if n.Series != nil {
		c.Series = make([]*wireSeries, len(n.Series))
		for i, s := range n.Series {
			if s != nil {
				sc := *s
				sc.Values = slices.Clone(s.Values)
				c.Series[i] = &sc
			}
		}
	}
	if n.GraphNodes != nil {
		c.GraphNodes = make([]*wireGraphNode, len(n.GraphNodes))
		for i, gn := range n.GraphNodes {
			if gn != nil {
				gnc := *gn
				c.GraphNodes[i] = &gnc
			}
		}
	}
	if n.GraphEdges != nil {
		c.GraphEdges = make([]*wireGraphEdge, len(n.GraphEdges))
		for i, ge := range n.GraphEdges {
			if ge != nil {
				gec := *ge
				c.GraphEdges[i] = &gec
			}
		}
	}
	c.Badges = cloneWireBadges(n.Badges)
	if n.Rows != nil {
		c.Rows = make([]*wireRow, len(n.Rows))
		for i, r := range n.Rows {
			if r != nil {
				rc := *r
				rc.Parents = slices.Clone(r.Parents)
				rc.Badges = cloneWireBadges(r.Badges)
				c.Rows[i] = &rc
			}
		}
	}
	return &c
}

func cloneWireBadges(in []*wireBadge) []*wireBadge {
	if in == nil {
		return nil
	}
	out := make([]*wireBadge, len(in))
	for i, b := range in {
		if b != nil {
			bc := *b
			out[i] = &bc
		}
	}
	return out
}

func cloneWireNode(n *wireNode) *wireNode {
	if n == nil {
		return nil
	}
	c := cloneOwnWireNode(n)
	if n.Children != nil {
		c.Children = make([]*wireNode, len(n.Children))
		for i, ch := range n.Children {
			c.Children[i] = cloneWireNode(ch)
		}
	}
	return c
}

func wireNodeOwnEqual(a, b *wireNode) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.ID == b.ID &&
		a.Kind == b.Kind &&
		a.Text == b.Text &&
		a.Tone == b.Tone &&
		a.Variant == b.Variant &&
		a.Icon == b.Icon &&
		a.Weight == b.Weight &&
		a.Size == b.Size &&
		a.Value == b.Value &&
		a.Checked == b.Checked &&
		slices.Equal(a.Options, b.Options) &&
		a.Progress == b.Progress &&
		a.Language == b.Language &&
		a.Placeholder == b.Placeholder &&
		a.Disabled == b.Disabled &&
		a.Grow == b.Grow &&
		a.Gap == b.Gap &&
		a.Columns == b.Columns &&
		a.Ratio == b.Ratio &&
		a.Fill == b.Fill &&
		a.Tap == b.Tap &&
		a.Change == b.Change &&
		a.Drag == b.Drag &&
		a.Drop == b.Drop &&
		a.DragGroup == b.DragGroup &&
		slices.Equal(a.DropAccept, b.DropAccept) &&
		a.Direction == b.Direction &&
		a.MaxHeight == b.MaxHeight &&
		a.Chart == b.Chart &&
		wireSeriesEqual(a.Series, b.Series) &&
		slices.Equal(a.Labels, b.Labels) &&
		wireGraphNodesEqual(a.GraphNodes, b.GraphNodes) &&
		wireGraphEdgesEqual(a.GraphEdges, b.GraphEdges) &&
		wireBadgesEqual(a.Badges, b.Badges) &&
		a.Check == b.Check &&
		a.Checks == b.Checks &&
		a.ContextMenu == b.ContextMenu &&
		a.ContextArg == b.ContextArg &&
		wireRowsEqual(a.Rows, b.Rows)
}

func wireSeriesEqual(a, b []*wireSeries) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		sa, sb := a[i], b[i]
		if sa == sb {
			continue
		}
		if sa == nil || sb == nil || sa.Name != sb.Name || sa.Tone != sb.Tone || !slices.Equal(sa.Values, sb.Values) {
			return false
		}
	}
	return true
}

func wireGraphNodesEqual(a, b []*wireGraphNode) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		na, nb := a[i], b[i]
		if na == nb {
			continue
		}
		if na == nil || nb == nil || *na != *nb {
			return false
		}
	}
	return true
}

func wireGraphEdgesEqual(a, b []*wireGraphEdge) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ea, eb := a[i], b[i]
		if ea == eb {
			continue
		}
		if ea == nil || eb == nil || *ea != *eb {
			return false
		}
	}
	return true
}

func wireBadgesEqual(a, b []*wireBadge) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ba, bb := a[i], b[i]
		if ba == bb {
			continue
		}
		if ba == nil || bb == nil || *ba != *bb {
			return false
		}
	}
	return true
}

func wireRowsEqual(a, b []*wireRow) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ra, rb := a[i], b[i]
		if ra == rb {
			continue
		}
		if ra == nil || rb == nil ||
			ra.ID != rb.ID ||
			!slices.Equal(ra.Parents, rb.Parents) ||
			ra.Text != rb.Text ||
			ra.Detail != rb.Detail ||
			ra.Meta != rb.Meta ||
			!wireBadgesEqual(ra.Badges, rb.Badges) ||
			ra.Tone != rb.Tone ||
			ra.Strong != rb.Strong ||
			ra.Icon != rb.Icon {
			return false
		}
	}
	return true
}

type viewBroadcaster struct {
	mu      sync.Mutex
	changed chan struct{}
}

func newViewBroadcaster() *viewBroadcaster {
	return &viewBroadcaster{changed: make(chan struct{})}
}

func (b *viewBroadcaster) signalChan() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.changed
}

func (b *viewBroadcaster) notify() {
	b.mu.Lock()
	close(b.changed)
	b.changed = make(chan struct{})
	b.mu.Unlock()
}

func writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, event string, id uint64, data []byte) error {
	if _, err := fmt.Fprintf(w, "event: %s\nid: %d\ndata: %s\n\n", event, id, data); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func (a *App) serveViewStream(w http.ResponseWriter, r *http.Request, viewID string, v View) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "streaming_unsupported", "streaming unsupported")
		return
	}
	locale := r.URL.Query().Get("locale")
	if locale == "" {
		locale = "en"
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	b := a.broadcasterFor(viewID)
	render := func() (Node, *wireNode, *viewIndex, []byte, error) {
		vctx := &ViewContext{
			Context: a.newBaseContext(r.Context(), locale),
			ViewID:  viewID,
		}
		raw, err := a.renderViewSafe(v, vctx)
		if err != nil {
			return Node{}, nil, nil, nil, err
		}
		wire, idx := normalizeAndIndex(raw)
		a.setViewIndex(viewID, idx)
		rawJSON, err := json.Marshal(raw)
		if err != nil {
			return Node{}, nil, nil, nil, err
		}
		return raw, wire, idx, rawJSON, nil
	}

	ch := b.signalChan()
	_, prevWire, prevIdx, prevRawJSON, err := render()
	if err != nil {
		errJSON, _ := json.Marshal(map[string]string{"message": err.Error()})
		_ = writeSSEEvent(w, flusher, "error", 1, errJSON)
		return
	}
	var seq uint64 = 1
	if err := writeSSEEvent(w, flusher, "tree", seq, prevRawJSON); err != nil {
		return
	}
	lastSent := time.Now()

	keepalive := time.NewTicker(streamKeepaliveInterval)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-a.stoppingCh():
			return
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-ch:
			if wait := a.coalesceInterval() - time.Since(lastSent); wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-timer.C:
				case <-r.Context().Done():
					timer.Stop()
					return
				case <-a.stoppingCh():
					timer.Stop()
					return
				}
			}

			ch = b.signalChan()
			_, nextWire, nextIdx, nextRawJSON, err := render()
			if err != nil {
				seq++
				errJSON, _ := json.Marshal(map[string]string{"message": err.Error()})
				if err := writeSSEEvent(w, flusher, "error", seq, errJSON); err != nil {
					return
				}
				lastSent = time.Now()
				continue
			}

			if actionsAndContextEqual(prevIdx, nextIdx) {
				ops := diffWireNodes(prevWire, nextWire)
				if len(ops) == 0 {
					continue
				}
				patchJSON, err := json.Marshal(struct {
					Ops []*Op `json:"ops"`
				}{Ops: ops})
				if err != nil {
					continue
				}
				seq++
				if err := writeSSEEvent(w, flusher, "patch", seq, patchJSON); err != nil {
					return
				}
			} else {
				if bytes.Equal(prevRawJSON, nextRawJSON) {
					continue
				}
				seq++
				if err := writeSSEEvent(w, flusher, "tree", seq, nextRawJSON); err != nil {
					return
				}
			}
			prevWire = nextWire
			prevIdx = nextIdx
			prevRawJSON = nextRawJSON
			lastSent = time.Now()
		}
	}
}
