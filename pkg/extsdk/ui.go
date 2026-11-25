package extsdk

import (
	"encoding/json"
	"fmt"
)

// rawNode mirrors internal/uiblock.rawNode's exact JSON schema.
// All fields use omitempty so default values are left to internal/uiblock.Read.
type rawNode struct {
	Kind        string          `json:"kind"`
	Key         string          `json:"key,omitempty"`
	Text        string          `json:"text,omitempty"`
	Tone        string          `json:"tone,omitempty"`
	Variant     string          `json:"variant,omitempty"`
	Icon        string          `json:"icon,omitempty"`
	Weight      string          `json:"weight,omitempty"`
	Size        string          `json:"size,omitempty"`
	Value       string          `json:"value,omitempty"`
	Checked     bool            `json:"checked,omitempty"`
	Options     []string        `json:"options,omitempty"`
	Progress    float64         `json:"progress,omitempty"`
	Language    string          `json:"language,omitempty"`
	Placeholder string          `json:"placeholder,omitempty"`
	Disabled    bool            `json:"disabled,omitempty"`
	Grow        float64         `json:"grow,omitempty"`
	Gap         float64         `json:"gap,omitempty"`
	Columns     int             `json:"columns,omitempty"`
	Ratio       *float64        `json:"ratio,omitempty"`
	Fill        bool            `json:"fill,omitempty"`
	OnTap       json.RawMessage `json:"onTap,omitempty"`
	OnChange    json.RawMessage `json:"onChange,omitempty"`
	OnSubmit    json.RawMessage `json:"onSubmit,omitempty"`
	OnLink      json.RawMessage `json:"onLink,omitempty"`
	Drag        string          `json:"drag,omitempty"`
	DragGroup   string          `json:"dragGroup,omitempty"`
	OnDrop      json.RawMessage `json:"onDrop,omitempty"`
	DropAccept  []string        `json:"dropAccept,omitempty"`
	Direction   string          `json:"direction,omitempty"`
	MaxHeight   float64         `json:"maxHeight,omitempty"`
	Children    []Node          `json:"children,omitempty"`
	Chart       string          `json:"chart,omitempty"`
	Series      []ChartSeries   `json:"series,omitempty"`
	Labels      []string        `json:"labels,omitempty"`
	GraphNodes  []GraphNode     `json:"graphNodes,omitempty"`
	GraphEdges  []GraphEdge     `json:"graphEdges,omitempty"`
	Badges      []Badge         `json:"badges,omitempty"`
	Check       string          `json:"check,omitempty"`
	OnCheck     json.RawMessage `json:"onCheck,omitempty"`
	ContextMenu string          `json:"contextMenu,omitempty"`
	ContextArg  string          `json:"contextArg,omitempty"`
	Rows        []DagRow        `json:"rows,omitempty"`
}

// Node represents a declarative UI node serialized to the internal/uiblock JSON schema.
type Node struct {
	raw rawNode
}

// MarshalJSON implements json.Marshaler using a value receiver so both Node and *Node serialize cleanly.
func (n Node) MarshalJSON() ([]byte, error) {
	return json.Marshal(n.raw)
}

// UnmarshalJSON implements json.Unmarshaler.
func (n *Node) UnmarshalJSON(data []byte) error {
	return json.Unmarshal(data, &n.raw)
}

// ChartSeries describes one data series on a chart node.
type ChartSeries struct {
	Name   string    `json:"name,omitempty"`
	Tone   string    `json:"tone,omitempty"`
	Values []float64 `json:"values"`
}

// GraphNode describes a node inside a graph canvas.
type GraphNode struct {
	ID       string  `json:"id"`
	Label    string  `json:"label"`
	Group    string  `json:"group,omitempty"`
	Tone     string  `json:"tone,omitempty"`
	Subtitle string  `json:"subtitle,omitempty"`
	Weight   float64 `json:"weight,omitempty"`
}

// GraphEdge describes a directed edge between two GraphNodes.
type GraphEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Label  string `json:"label,omitempty"`
	Tone   string `json:"tone,omitempty"`
	Dashed bool   `json:"dashed,omitempty"`
}

// Badge describes a trailing status tag attached to a node or dag row.
type Badge struct {
	Text    string `json:"text,omitempty"`
	Tone    string `json:"tone,omitempty"`
	Variant string `json:"variant,omitempty"`
	Icon    string `json:"icon,omitempty"`
}

// DagRow describes one row inside a dag node.
type DagRow struct {
	ID      string   `json:"id"`
	Parents []string `json:"parents,omitempty"`
	Text    string   `json:"text,omitempty"`
	Detail  string   `json:"detail,omitempty"`
	Meta    string   `json:"meta,omitempty"`
	Badges  []Badge  `json:"badges,omitempty"`
	Tone    string   `json:"tone,omitempty"`
	Strong  bool     `json:"strong,omitempty"`
	Icon    string   `json:"icon,omitempty"`
}

// Option mutates a Node during construction.
type Option func(*Node)

func encodeAction(name string, args ...any) json.RawMessage {
	if name == "" {
		return nil
	}
	parts := make([]any, 0, 1+len(args))
	parts = append(parts, name)
	parts = append(parts, args...)
	b, err := json.Marshal(parts)
	if err != nil {
		return nil
	}
	return json.RawMessage(b)
}

// Key sets a node's deterministic key (used to form node IDs like "/key").
func Key(k string) Option { return func(n *Node) { n.raw.Key = k } }

// ID is an alias for Key.
func ID(k string) Option { return Key(k) }

// Text sets a node's text label or content.
func Text(s string) Option { return func(n *Node) { n.raw.Text = s } }

// Tone sets a node's semantic tone token ("muted", "faint", "accent", "success", "warning", "danger").
func Tone(t string) Option { return func(n *Node) { n.raw.Tone = t } }

// Variant sets a node's visual variant token.
func Variant(v string) Option { return func(n *Node) { n.raw.Variant = v } }

// Icon sets a node's Lucide icon name.
func Icon(name string) Option { return func(n *Node) { n.raw.Icon = name } }

// Weight sets a text node's font weight ("medium", "semibold").
func Weight(w string) Option { return func(n *Node) { n.raw.Weight = w } }

// Size sets a node's size token ("small", "large").
func Size(s string) Option { return func(n *Node) { n.raw.Size = s } }

// Value sets an input, select, tabs, or graph node's value.
func Value(v string) Option { return func(n *Node) { n.raw.Value = v } }

// Checked sets a toggle or collapsible's checked/expanded boolean state.
func Checked(b bool) Option { return func(n *Node) { n.raw.Checked = b } }

// Options sets the options list on select, tabs, or breadcrumb nodes.
func Options(opts ...string) Option {
	return func(n *Node) { n.raw.Options = append([]string(nil), opts...) }
}

// Progress sets a progress bar's completion ratio in [0, 1].
func Progress(p float64) Option { return func(n *Node) { n.raw.Progress = p } }

// Language sets a code block's syntax highlighting language.
func Language(lang string) Option { return func(n *Node) { n.raw.Language = lang } }

// Placeholder sets placeholder or tooltip text on supported nodes.
func Placeholder(p string) Option { return func(n *Node) { n.raw.Placeholder = p } }

// Disabled marks an interactive node as disabled.
func Disabled(d bool) Option { return func(n *Node) { n.raw.Disabled = d } }

// Grow sets a node's flex grow factor.
func Grow(g float64) Option { return func(n *Node) { n.raw.Grow = g } }

// Gap sets spacing between children in a layout node.
func Gap(g float64) Option { return func(n *Node) { n.raw.Gap = g } }

// Columns sets the column count on a grid node.
func Columns(c int) Option { return func(n *Node) { n.raw.Columns = c } }

// Ratio sets the split pane ratio in [0.1, 0.9].
func Ratio(r float64) Option {
	return func(n *Node) {
		v := r
		n.raw.Ratio = &v
	}
}

// Fill sets fill=true on a root column, row, or split node.
func Fill(f bool) Option { return func(n *Node) { n.raw.Fill = f } }

// Direction sets direction on scroll ("vertical"|"horizontal") or split ("vertical"|"horizontal"|"auto").
func Direction(d string) Option { return func(n *Node) { n.raw.Direction = d } }

// MaxHeight sets maxHeight on scroll, graph, or dag nodes.
func MaxHeight(h float64) Option { return func(n *Node) { n.raw.MaxHeight = h } }

// Labels sets column headers on table or axis/slice labels on chart.
func Labels(labels ...string) Option {
	return func(n *Node) { n.raw.Labels = append([]string(nil), labels...) }
}

// Badges attaches trailing badges to a node.
func Badges(badges ...Badge) Option {
	return func(n *Node) { n.raw.Badges = append([]Badge(nil), badges...) }
}

// Check sets the tri-state check token ("on", "off", "mixed") on checkbox or tree_item.
func Check(c string) Option { return func(n *Node) { n.raw.Check = c } }

// Drag sets the drag payload and optional dragGroup.
func Drag(payload string, group ...string) Option {
	return func(n *Node) {
		n.raw.Drag = payload
		if len(group) > 0 {
			n.raw.DragGroup = group[0]
		}
	}
}

// DragGroup sets the dragGroup on a draggable node.
func DragGroup(group string) Option { return func(n *Node) { n.raw.DragGroup = group } }

// DropAccept sets accepted drag groups on a drop-target node.
func DropAccept(groups ...string) Option {
	return func(n *Node) { n.raw.DropAccept = append([]string(nil), groups...) }
}

// ContextMenu binds a context menu key (and optional contextArg) to a node.
func ContextMenu(key string, arg ...string) Option {
	return func(n *Node) {
		n.raw.ContextMenu = key
		if len(arg) > 0 {
			n.raw.ContextArg = arg[0]
		}
	}
}

// ContextArg sets contextArg on a node with a ContextMenu.
func ContextArg(arg string) Option { return func(n *Node) { n.raw.ContextArg = arg } }

// OnTap binds a tap handler message [name, ...args].
func OnTap(name string, args ...any) Option {
	return func(n *Node) { n.raw.OnTap = encodeAction(name, args...) }
}

// OnChange binds a change handler message [name, ...args].
func OnChange(name string, args ...any) Option {
	return func(n *Node) { n.raw.OnChange = encodeAction(name, args...) }
}

// OnSubmit binds a submit handler message [name, ...args] on input/textarea.
func OnSubmit(name string, args ...any) Option {
	return func(n *Node) { n.raw.OnSubmit = encodeAction(name, args...) }
}

// OnLink binds a link handler message [name, ...args] on markdown.
func OnLink(name string, args ...any) Option {
	return func(n *Node) { n.raw.OnLink = encodeAction(name, args...) }
}

// OnDrop binds a drop handler message [name, ...args].
func OnDrop(name string, args ...any) Option {
	return func(n *Node) { n.raw.OnDrop = encodeAction(name, args...) }
}

// OnCheck binds a check handler message [name, ...args] on tree_item.
func OnCheck(name string, args ...any) Option {
	return func(n *Node) { n.raw.OnCheck = encodeAction(name, args...) }
}

// Apply returns a copy of n with opts applied.
func (n Node) Apply(opts ...Option) Node {
	for _, opt := range opts {
		if opt != nil {
			opt(&n)
		}
	}
	return n
}

func (n Node) Key(k string) Node                      { return n.Apply(Key(k)) }
func (n Node) ID(k string) Node                       { return n.Apply(ID(k)) }
func (n Node) Text(s string) Node                     { return n.Apply(Text(s)) }
func (n Node) Tone(t string) Node                     { return n.Apply(Tone(t)) }
func (n Node) Variant(v string) Node                  { return n.Apply(Variant(v)) }
func (n Node) Icon(name string) Node                  { return n.Apply(Icon(name)) }
func (n Node) Weight(w string) Node                   { return n.Apply(Weight(w)) }
func (n Node) Size(s string) Node                     { return n.Apply(Size(s)) }
func (n Node) Value(v string) Node                    { return n.Apply(Value(v)) }
func (n Node) Checked(b bool) Node                    { return n.Apply(Checked(b)) }
func (n Node) Options(opts ...string) Node            { return n.Apply(Options(opts...)) }
func (n Node) Progress(p float64) Node                { return n.Apply(Progress(p)) }
func (n Node) Language(lang string) Node              { return n.Apply(Language(lang)) }
func (n Node) Placeholder(p string) Node              { return n.Apply(Placeholder(p)) }
func (n Node) Disabled(d bool) Node                   { return n.Apply(Disabled(d)) }
func (n Node) Grow(g float64) Node                    { return n.Apply(Grow(g)) }
func (n Node) Gap(g float64) Node                     { return n.Apply(Gap(g)) }
func (n Node) Columns(c int) Node                     { return n.Apply(Columns(c)) }
func (n Node) Ratio(r float64) Node                   { return n.Apply(Ratio(r)) }
func (n Node) Fill(f bool) Node                       { return n.Apply(Fill(f)) }
func (n Node) Direction(d string) Node                { return n.Apply(Direction(d)) }
func (n Node) MaxHeight(h float64) Node               { return n.Apply(MaxHeight(h)) }
func (n Node) Labels(labels ...string) Node           { return n.Apply(Labels(labels...)) }
func (n Node) Badges(badges ...Badge) Node            { return n.Apply(Badges(badges...)) }
func (n Node) Check(c string) Node                    { return n.Apply(Check(c)) }
func (n Node) Drag(p string, g ...string) Node        { return n.Apply(Drag(p, g...)) }
func (n Node) DragGroup(g string) Node                { return n.Apply(DragGroup(g)) }
func (n Node) DropAccept(groups ...string) Node       { return n.Apply(DropAccept(groups...)) }
func (n Node) ContextMenu(k string, a ...string) Node { return n.Apply(ContextMenu(k, a...)) }
func (n Node) ContextArg(arg string) Node             { return n.Apply(ContextArg(arg)) }
func (n Node) OnTap(name string, a ...any) Node       { return n.Apply(OnTap(name, a...)) }
func (n Node) OnChange(name string, a ...any) Node    { return n.Apply(OnChange(name, a...)) }
func (n Node) OnSubmit(name string, a ...any) Node    { return n.Apply(OnSubmit(name, a...)) }
func (n Node) OnLink(name string, a ...any) Node      { return n.Apply(OnLink(name, a...)) }
func (n Node) OnDrop(name string, a ...any) Node      { return n.Apply(OnDrop(name, a...)) }
func (n Node) OnCheck(name string, a ...any) Node     { return n.Apply(OnCheck(name, a...)) }

// Children appends child nodes to n.
func (n Node) Children(children ...Node) Node {
	n.raw.Children = append(append([]Node(nil), n.raw.Children...), children...)
	return n
}

// UI provides declarative builder methods for all 35 uiblock node kinds.
// A nil *UI receiver is safe to use.
type UI struct{}

func buildContainer(kind string, items []any, opts ...Option) Node {
	n := Node{raw: rawNode{Kind: kind}}
	for _, opt := range opts {
		if opt != nil {
			opt(&n)
		}
	}
	for _, item := range items {
		switch v := item.(type) {
		case nil:
		case Node:
			n.raw.Children = append(n.raw.Children, v)
		case *Node:
			if v != nil {
				n.raw.Children = append(n.raw.Children, *v)
			}
		case []Node:
			n.raw.Children = append(n.raw.Children, v...)
		case Option:
			if v != nil {
				v(&n)
			}
		case []Option:
			for _, o := range v {
				if o != nil {
					o(&n)
				}
			}
		default:
			panic(fmt.Sprintf("extsdk.UI: unsupported item type %T in %s", item, kind))
		}
	}
	return n
}

func applyOpts(n Node, opts ...Option) Node {
	for _, o := range opts {
		if o != nil {
			o(&n)
		}
	}
	return n
}

func (u *UI) Column(items ...any) Node { return buildContainer("column", items) }
func (u *UI) Row(items ...any) Node    { return buildContainer("row", items) }

// Toolbar constructs a row node with variant="toolbar".
func (u *UI) Toolbar(items ...any) Node {
	return buildContainer("row", items, Variant("toolbar"))
}

func (u *UI) Stack(items ...any) Node  { return buildContainer("stack", items) }
func (u *UI) Card(items ...any) Node   { return buildContainer("card", items) }
func (u *UI) Scroll(items ...any) Node { return buildContainer("scroll", items) }
func (u *UI) Wrap(items ...any) Node   { return buildContainer("wrap", items) }
func (u *UI) List(items ...any) Node   { return buildContainer("list", items) }

func (u *UI) Grid(columns int, items ...any) Node {
	return buildContainer("grid", items, Columns(columns))
}

// Split constructs a split container. Accepts an optional leading direction string
// ("horizontal", "vertical", "auto"), two child Nodes, and Options.
func (u *UI) Split(args ...any) Node {
	n := Node{raw: rawNode{Kind: "split"}}
	for i, arg := range args {
		switch v := arg.(type) {
		case nil:
		case string:
			if i == 0 {
				n.raw.Direction = v
			} else {
				panic(fmt.Sprintf("extsdk.UI: unexpected string argument at position %d in Split", i))
			}
		case Node:
			n.raw.Children = append(n.raw.Children, v)
		case *Node:
			if v != nil {
				n.raw.Children = append(n.raw.Children, *v)
			}
		case []Node:
			n.raw.Children = append(n.raw.Children, v...)
		case Option:
			if v != nil {
				v(&n)
			}
		default:
			panic(fmt.Sprintf("extsdk.UI: unsupported argument type %T in Split", arg))
		}
	}
	return n
}

func (u *UI) Collapsible(title string, items ...any) Node {
	return buildContainer("collapsible", items, Text(title))
}

func (u *UI) Breadcrumb(options []string, opts ...Option) Node {
	return applyOpts(Node{raw: rawNode{Kind: "breadcrumb", Options: append([]string(nil), options...)}}, opts...)
}

func (u *UI) Tree(items ...any) Node { return buildContainer("tree", items) }

func (u *UI) TreeItem(text string, items ...any) Node {
	return buildContainer("tree_item", items, Text(text))
}

func (u *UI) Text(text string, opts ...Option) Node {
	return applyOpts(Node{raw: rawNode{Kind: "text", Text: text}}, opts...)
}

func (u *UI) Icon(name string, opts ...Option) Node {
	return applyOpts(Node{raw: rawNode{Kind: "icon", Icon: name}}, opts...)
}

func (u *UI) Pill(text string, opts ...Option) Node {
	return applyOpts(Node{raw: rawNode{Kind: "pill", Text: text}}, opts...)
}

func (u *UI) Chip(text string, opts ...Option) Node {
	return applyOpts(Node{raw: rawNode{Kind: "chip", Text: text}}, opts...)
}

// Button constructs a button node. Accepts an optional leading text string and Options.
func (u *UI) Button(args ...any) Node {
	n := Node{raw: rawNode{Kind: "button"}}
	for _, arg := range args {
		switch v := arg.(type) {
		case nil:
		case string:
			n.raw.Text = v
		case Option:
			if v != nil {
				v(&n)
			}
		default:
			panic(fmt.Sprintf("extsdk.UI: unsupported argument type %T in Button", arg))
		}
	}
	return n
}

// Input constructs a single-line text input. Accepts an optional value string and Options.
func (u *UI) Input(args ...any) Node {
	n := Node{raw: rawNode{Kind: "input"}}
	for _, arg := range args {
		switch v := arg.(type) {
		case nil:
		case string:
			n.raw.Value = v
		case Option:
			if v != nil {
				v(&n)
			}
		default:
			panic(fmt.Sprintf("extsdk.UI: unsupported argument type %T in Input", arg))
		}
	}
	return n
}

// Textarea constructs a multi-line text input. Accepts an optional value string and Options.
func (u *UI) Textarea(args ...any) Node {
	n := Node{raw: rawNode{Kind: "textarea"}}
	for _, arg := range args {
		switch v := arg.(type) {
		case nil:
		case string:
			n.raw.Value = v
		case Option:
			if v != nil {
				v(&n)
			}
		default:
			panic(fmt.Sprintf("extsdk.UI: unsupported argument type %T in Textarea", arg))
		}
	}
	return n
}

// Toggle constructs a toggle switch. Accepts optional bool (Checked), string (Text), and Options.
func (u *UI) Toggle(args ...any) Node {
	n := Node{raw: rawNode{Kind: "toggle"}}
	for _, arg := range args {
		switch v := arg.(type) {
		case nil:
		case bool:
			n.raw.Checked = v
		case string:
			n.raw.Text = v
		case Option:
			if v != nil {
				v(&n)
			}
		default:
			panic(fmt.Sprintf("extsdk.UI: unsupported argument type %T in Toggle", arg))
		}
	}
	return n
}

// Select constructs a dropdown select. Accepts []string (options), optional string (value), and Options in any order.
func (u *UI) Select(args ...any) Node {
	n := Node{raw: rawNode{Kind: "select"}}
	for _, arg := range args {
		switch v := arg.(type) {
		case nil:
		case []string:
			n.raw.Options = append([]string(nil), v...)
		case string:
			n.raw.Value = v
		case Option:
			if v != nil {
				v(&n)
			}
		default:
			panic(fmt.Sprintf("extsdk.UI: unsupported argument type %T in Select", arg))
		}
	}
	return n
}

// Tabs constructs a tab bar. Accepts []string (options), optional string (value), and Options in any order.
func (u *UI) Tabs(args ...any) Node {
	n := Node{raw: rawNode{Kind: "tabs"}}
	for _, arg := range args {
		switch v := arg.(type) {
		case nil:
		case []string:
			n.raw.Options = append([]string(nil), v...)
		case string:
			n.raw.Value = v
		case Option:
			if v != nil {
				v(&n)
			}
		default:
			panic(fmt.Sprintf("extsdk.UI: unsupported argument type %T in Tabs", arg))
		}
	}
	return n
}

func (u *UI) Table(labels []string, items ...any) Node {
	return buildContainer("table", items, Labels(labels...))
}

func (u *UI) Markdown(text string, opts ...Option) Node {
	return applyOpts(Node{raw: rawNode{Kind: "markdown", Text: text}}, opts...)
}

func (u *UI) Code(language, text string, opts ...Option) Node {
	return applyOpts(Node{raw: rawNode{Kind: "code", Language: language, Text: text}}, opts...)
}

func (u *UI) Progress(progress float64, opts ...Option) Node {
	return applyOpts(Node{raw: rawNode{Kind: "progress", Progress: progress}}, opts...)
}

// Spinner constructs a progress node with variant="spinner".
func (u *UI) Spinner(opts ...Option) Node {
	return applyOpts(Node{raw: rawNode{Kind: "progress", Variant: "spinner"}}, opts...)
}

func (u *UI) Divider(opts ...Option) Node {
	return applyOpts(Node{raw: rawNode{Kind: "divider"}}, opts...)
}

func (u *UI) Chart(chartType string, series []ChartSeries, opts ...Option) Node {
	copied := make([]ChartSeries, len(series))
	for i, s := range series {
		copied[i] = ChartSeries{
			Name:   s.Name,
			Tone:   s.Tone,
			Values: append([]float64(nil), s.Values...),
		}
	}
	return applyOpts(Node{raw: rawNode{Kind: "chart", Chart: chartType, Series: copied}}, opts...)
}

func (u *UI) Graph(nodes []GraphNode, edges []GraphEdge, opts ...Option) Node {
	return applyOpts(Node{raw: rawNode{
		Kind:       "graph",
		GraphNodes: append([]GraphNode(nil), nodes...),
		GraphEdges: append([]GraphEdge(nil), edges...),
	}}, opts...)
}

// Menu constructs a menu node. Accepts optional string (Text), child Nodes (menu_item/divider/menu), and Options.
func (u *UI) Menu(args ...any) Node {
	n := Node{raw: rawNode{Kind: "menu"}}
	for _, arg := range args {
		switch v := arg.(type) {
		case nil:
		case string:
			n.raw.Text = v
		case Node:
			n.raw.Children = append(n.raw.Children, v)
		case *Node:
			if v != nil {
				n.raw.Children = append(n.raw.Children, *v)
			}
		case []Node:
			n.raw.Children = append(n.raw.Children, v...)
		case Option:
			if v != nil {
				v(&n)
			}
		default:
			panic(fmt.Sprintf("extsdk.UI: unsupported argument type %T in Menu", arg))
		}
	}
	return n
}

// ContextMenu constructs a context menu node (variant="context") with the given key.
func (u *UI) ContextMenu(key string, items ...any) Node {
	return buildContainer("menu", items, Key(key), Variant("context"))
}

func (u *UI) MenuItem(text string, opts ...Option) Node {
	return applyOpts(Node{raw: rawNode{Kind: "menu_item", Text: text}}, opts...)
}

// Checkbox constructs a checkbox node. Accepts optional string (label), bool (on/off), and Options.
func (u *UI) Checkbox(args ...any) Node {
	n := Node{raw: rawNode{Kind: "checkbox"}}
	for _, arg := range args {
		switch v := arg.(type) {
		case nil:
		case string:
			n.raw.Text = v
		case bool:
			if v {
				n.raw.Check = "on"
			} else {
				n.raw.Check = "off"
			}
		case Option:
			if v != nil {
				v(&n)
			}
		default:
			panic(fmt.Sprintf("extsdk.UI: unsupported argument type %T in Checkbox", arg))
		}
	}
	if n.raw.Check == "" {
		n.raw.Check = "off"
	}
	return n
}

func (u *UI) Dialog(title string, items ...any) Node {
	return buildContainer("dialog", items, Text(title))
}

func (u *UI) Dag(rows []DagRow, opts ...Option) Node {
	copied := make([]DagRow, len(rows))
	for i, r := range rows {
		copied[i] = r
		copied[i].Parents = append([]string(nil), r.Parents...)
		copied[i].Badges = append([]Badge(nil), r.Badges...)
	}
	return applyOpts(Node{raw: rawNode{Kind: "dag", Rows: copied}}, opts...)
}
