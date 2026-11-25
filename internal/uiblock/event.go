package uiblock

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Event kind constants.
const (
	EventTap    = "tap"
	EventChange = "change"
	EventSubmit = "submit"
	EventDrop   = "drop"
	EventCheck  = "check"
)

// The bounds of an event from a front-end. Its values are as many as a
// tree has nodes at most, a front-end sending every field of the tree it
// has, so they are bounded by their bytes.
const (
	MaxEventValue = 8 << 10  // the value a change carries
	maxValuesSize = 32 << 10 // the block's values an event carries, ids and values
)

// Event is a user action on a node of a view's UI tree.
type Event struct {
	Block  string            `json:"block,omitempty"`
	Node   string            `json:"node,omitempty"`
	Kind   string            `json:"kind,omitempty"`
	Value  string            `json:"value,omitempty"`
	Values map[string]string `json:"values,omitempty"`
	Locale string            `json:"locale,omitempty"`
}

// Message is what a node sends for an event: the message its tree gave
// it, [name, ...arguments], and for a change the value last.
type Message struct {
	Name string `json:"name,omitempty"`
	// Given is the node's message as the tree gave it.
	Given json.RawMessage `json:"given,omitempty"`
	// Args are its arguments, then a change's value: a toggle's as a
	// boolean, any other's as text.
	Args []json.RawMessage `json:"args,omitempty"`
	// Kind is the node's kind.
	Kind string `json:"kind,omitempty"`
}

// Message is the message the node of an event sends, refusing an event of
// a node the tree does not hold, or one that sends nothing for it.
func (t *Tree) Message(ev Event) (Message, error) {
	if len(ev.Value) > MaxEventValue {
		return Message{}, fmt.Errorf("the event's value is over %d KiB", MaxEventValue>>10)
	}
	var a actions
	var kind string
	var node *Node
	found := false
	if t != nil {
		a, found = t.actions[ev.Node]
		if node = NodeOf(t.Root, ev.Node); node != nil {
			kind = node.Kind
		}
	}
	var raw json.RawMessage
	switch ev.Kind {
	case EventTap:
		raw = a.tap
	case EventChange:
		raw = a.change
	case EventSubmit:
		if kind == KindInput || kind == KindTextarea {
			raw = a.submit
			if raw == nil {
				raw = a.change
			}
		}
	case EventDrop:
		if ev.Value == "" || len(ev.Value) > maxDrag || !utf8.ValidString(ev.Value) || strings.ContainsFunc(ev.Value, Spoofing) {
			return Message{}, fmt.Errorf("invalid drop payload for %q of %s", ev.Node, ev.Block)
		}
		raw = a.drop
	case EventCheck:
		if ev.Value != "on" && ev.Value != "off" {
			return Message{}, fmt.Errorf("invalid check value %q for %q of %s", ev.Value, ev.Node, ev.Block)
		}
		raw = a.check
	default:
		return Message{}, fmt.Errorf("%q is not a kind of event", ev.Kind)
	}
	if !found || raw == nil {
		return Message{}, fmt.Errorf("node %q of %s sends no message on %s", ev.Node, ev.Block, ev.Kind)
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil || len(parts) == 0 {
		return Message{}, errors.New("the node's message does not read")
	}
	m := Message{Given: raw, Args: parts[1:], Kind: kind}
	_ = json.Unmarshal(parts[0], &m.Name)
	switch ev.Kind {
	case EventTap:
		if menuKey, ok := t.itemContextMenu[ev.Node]; ok {
			allowed := t.contextArgs[menuKey]
			if !allowed[ev.Value] {
				return Message{}, fmt.Errorf("context menu %q of %s has no opener for arg %q", menuKey, ev.Block, ev.Value)
			}
			b, _ := json.Marshal(ev.Value)
			m.Args = append(m.Args, b)
		}
	case EventChange:
		var value any = ev.Value
		switch kind {
		case KindToggle, KindCollapsible, KindTreeItem:
			value = ev.Value == "true"
		case KindCheckbox:
			if ev.Value != "on" && ev.Value != "off" {
				return Message{}, fmt.Errorf("invalid checkbox value %q for %q of %s", ev.Value, ev.Node, ev.Block)
			}
		case KindDialog:
			if ev.Value != "dismiss" {
				return Message{}, fmt.Errorf("invalid dialog change value %q for %q of %s", ev.Value, ev.Node, ev.Block)
			}
		case KindDag:
			if !dagHasRow(node, ev.Value) {
				return Message{}, fmt.Errorf("dag %q of %s has no row %q", ev.Node, ev.Block, ev.Value)
			}
		}
		b, _ := json.Marshal(value)
		m.Args = append(m.Args, b)
	default:
		b, _ := json.Marshal(ev.Value)
		m.Args = append(m.Args, b)
	}
	return m, nil
}

func dagHasRow(n *Node, id string) bool {
	if n == nil || id == "" {
		return false
	}
	for _, r := range n.Rows {
		if r.ID == id {
			return true
		}
	}
	return false
}

// Values are the values of an event's block, as the front-end has them,
// kept to the tree's inputs, textareas, toggles, checkboxes, selects and
// tabs, by node id: what a form's submit carries. Too many, more than a
// tree holds nodes, or too much is refused, as is a value of a node of
// another kind. A value of a node the tree no longer holds is left out:
// the front-end's tree may be a render behind, and the rest of the form
// is still the user's.
func (t *Tree) Values(in map[string]string) (map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > maxNodes {
		return nil, fmt.Errorf("an event carries at most %d values", maxNodes)
	}
	size := 0
	for id, v := range in {
		size += len(id) + len(v)
		if len(v) > MaxEventValue || size > maxValuesSize {
			return nil, fmt.Errorf("an event's values are at most %d KiB", maxValuesSize>>10)
		}
	}
	kinds := map[string]string{}
	var walk func(n *Node)
	walk = func(n *Node) {
		if n == nil {
			return
		}
		kinds[n.ID] = n.Kind
		for _, c := range n.Children {
			walk(c)
		}
	}
	if t != nil {
		walk(t.Root)
	}
	out := make(map[string]string, len(in))
	for id, v := range in {
		switch kind, ok := kinds[id]; {
		case !ok:
			continue
		case kind != KindInput && kind != KindTextarea && kind != KindToggle && kind != KindCheckbox && kind != KindSelect && kind != KindTabs:
			return nil, fmt.Errorf("node %q is no input, textarea, toggle, checkbox, select or tabs of the block", id)
		}
		out[id] = v
	}
	return out, nil
}

// NodeOf is the node of id in a tree, nil for none.
func NodeOf(n *Node, id string) *Node {
	if n == nil {
		return nil
	}
	if n.ID == id {
		return n
	}
	for _, c := range n.Children {
		if m := NodeOf(c, id); m != nil {
			return m
		}
	}
	return nil
}
