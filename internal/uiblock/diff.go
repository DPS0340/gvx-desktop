package uiblock

import (
	"errors"
	"fmt"
	"slices"
)

// The ops of an Op.
const (
	OpReplace = "replace"
	OpUpdate  = "update"
	OpInsert  = "insert"
	OpRemove  = "remove"
	OpMove    = "move"
)

// Op is a change of a tree.
type Op struct {
	Op     string `json:"op,omitempty"`
	ID     string `json:"id,omitempty"`
	Parent string `json:"parent,omitempty"`
	Index  int    `json:"index,omitempty"`
	Node   *Node  `json:"node,omitempty"`
}

// Diff is the ops that make old into next, both trees of the same root
// id; none when they are the same.
func Diff(old, next *Node) []*Op {
	if old == nil && next == nil {
		return nil
	}
	if old == nil {
		return []*Op{{Op: OpReplace, ID: next.ID, Node: cloneNode(next)}}
	}
	if next == nil {
		return nil
	}
	var ops []*Op
	diffNodes(old, next, &ops)
	return ops
}

func diffNodes(old, next *Node, ops *[]*Op) {
	if old.Kind != next.Kind {
		*ops = append(*ops, &Op{Op: OpReplace, ID: next.ID, Node: cloneNode(next)})
		return
	}
	if !nodeOwnEqual(old, next) {
		*ops = append(*ops, &Op{Op: OpUpdate, ID: next.ID, Node: ownNode(next)})
	}
	byID := make(map[string]*Node, len(old.Children))
	for _, c := range old.Children {
		byID[c.ID] = c
	}
	wanted := make(map[string]bool, len(next.Children))
	for _, c := range next.Children {
		wanted[c.ID] = true
	}
	// The children as the front-end will have them, op by op.
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
			*ops = append(*ops, &Op{Op: OpInsert, Parent: next.ID, Index: i, Node: cloneNode(c)})
			cur = slices.Insert(cur, i, c.ID)
		case cur[i] != c.ID:
			*ops = append(*ops, &Op{Op: OpMove, ID: c.ID, Parent: next.ID, Index: i})
			idx := slices.Index(cur, c.ID)
			cur = slices.Delete(cur, idx, idx+1)
			cur = slices.Insert(cur, i, c.ID)
			diffNodes(prev, c, ops)
		default:
			diffNodes(prev, c, ops)
		}
	}
}

// Apply applies ops to a deep copy of root and returns it; root is left
// as it was. An op that names a node the tree does not hold fails.
func Apply(root *Node, ops []*Op) (*Node, error) {
	if root == nil {
		return nil, errors.New("apply: nil root")
	}
	out := cloneNode(root)
	for _, op := range ops {
		var err error
		if out, err = applyOp(out, op); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func applyOp(root *Node, op *Op) (*Node, error) {
	if op == nil {
		return nil, errors.New("apply: nil op")
	}
	switch op.Op {
	case OpReplace:
		if op.Node == nil {
			return nil, fmt.Errorf("replace: nil node for %q", op.ID)
		}
		if op.ID == root.ID {
			return cloneNode(op.Node), nil
		}
		parent, i := findParent(root, op.ID)
		if parent == nil {
			return nil, fmt.Errorf("replace: no node %q", op.ID)
		}
		parent.Children[i] = cloneNode(op.Node)
	case OpUpdate:
		if op.Node == nil {
			return nil, fmt.Errorf("update: nil node for %q", op.ID)
		}
		n := root
		if op.ID != root.ID {
			parent, i := findParent(root, op.ID)
			if parent == nil {
				return nil, fmt.Errorf("update: no node %q", op.ID)
			}
			n = parent.Children[i]
		}
		kids := n.Children
		updated := cloneOwnNode(op.Node)
		updated.Children = kids
		*n = *updated
	case OpInsert:
		if op.Node == nil {
			return nil, fmt.Errorf("insert: nil node in %q", op.Parent)
		}
		parent := NodeOf(root, op.Parent)
		if parent == nil || op.Index < 0 || op.Index > len(parent.Children) {
			return nil, fmt.Errorf("insert: no place %d in %q", op.Index, op.Parent)
		}
		parent.Children = slices.Insert(parent.Children, op.Index, cloneNode(op.Node))
	case OpRemove:
		parent, i := findParent(root, op.ID)
		if parent == nil {
			return nil, fmt.Errorf("remove: no node %q", op.ID)
		}
		parent.Children = slices.Delete(parent.Children, i, i+1)
	case OpMove:
		parent, i := findParent(root, op.ID)
		if parent == nil || parent.ID != op.Parent || op.Index < 0 || op.Index >= len(parent.Children) {
			return nil, fmt.Errorf("move: no node %q in %q", op.ID, op.Parent)
		}
		n := parent.Children[i]
		parent.Children = slices.Delete(parent.Children, i, i+1)
		parent.Children = slices.Insert(parent.Children, op.Index, n)
	default:
		return nil, fmt.Errorf("unknown op %q", op.Op)
	}
	return root, nil
}

// findParent is the parent of the node of id, and its index there; nil when
// no node but the root has it.
func findParent(n *Node, id string) (*Node, int) {
	if n == nil {
		return nil, 0
	}
	for i, c := range n.Children {
		if c.ID == id {
			return n, i
		}
		if p, j := findParent(c, id); p != nil {
			return p, j
		}
	}
	return nil, 0
}

// ownNode returns a deep copy of n without its children.
func ownNode(n *Node) *Node {
	if n == nil {
		return nil
	}
	c := cloneOwnNode(n)
	return c
}

func cloneOwnNode(n *Node) *Node {
	if n == nil {
		return nil
	}
	c := *n
	c.Children = nil
	c.Options = slices.Clone(n.Options)
	c.DropAccept = slices.Clone(n.DropAccept)
	c.Labels = slices.Clone(n.Labels)
	if n.Series != nil {
		c.Series = make([]*Series, len(n.Series))
		for i, s := range n.Series {
			if s != nil {
				sc := *s
				sc.Values = slices.Clone(s.Values)
				c.Series[i] = &sc
			}
		}
	}
	if n.GraphNodes != nil {
		c.GraphNodes = make([]*GraphNode, len(n.GraphNodes))
		for i, gn := range n.GraphNodes {
			if gn != nil {
				gnc := *gn
				c.GraphNodes[i] = &gnc
			}
		}
	}
	if n.GraphEdges != nil {
		c.GraphEdges = make([]*GraphEdge, len(n.GraphEdges))
		for i, ge := range n.GraphEdges {
			if ge != nil {
				gec := *ge
				c.GraphEdges[i] = &gec
			}
		}
	}
	if n.Badges != nil {
		c.Badges = cloneBadges(n.Badges)
	}
	if n.Rows != nil {
		c.Rows = make([]*Row, len(n.Rows))
		for i, r := range n.Rows {
			if r != nil {
				rc := *r
				rc.Parents = slices.Clone(r.Parents)
				rc.Badges = cloneBadges(r.Badges)
				c.Rows[i] = &rc
			}
		}
	}
	return &c
}

func cloneBadges(in []*Badge) []*Badge {
	if in == nil {
		return nil
	}
	out := make([]*Badge, len(in))
	for i, b := range in {
		if b != nil {
			bc := *b
			out[i] = &bc
		}
	}
	return out
}

func cloneNode(n *Node) *Node {
	if n == nil {
		return nil
	}
	c := cloneOwnNode(n)
	if n.Children != nil {
		c.Children = make([]*Node, len(n.Children))
		for i, ch := range n.Children {
			c.Children[i] = cloneNode(ch)
		}
	}
	return c
}

// Equal reports whether n and o are structurally equal, treating nil and
// empty slices as equal.
func (n *Node) Equal(o *Node) bool {
	if n == o {
		return true
	}
	if n == nil || o == nil {
		return false
	}
	if !nodeOwnEqual(n, o) {
		return false
	}
	if len(n.Children) != len(o.Children) {
		return false
	}
	for i := range n.Children {
		if !n.Children[i].Equal(o.Children[i]) {
			return false
		}
	}
	return true
}

func nodeOwnEqual(a, b *Node) bool {
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
		seriesSliceEqual(a.Series, b.Series) &&
		slices.Equal(a.Labels, b.Labels) &&
		graphNodesEqual(a.GraphNodes, b.GraphNodes) &&
		graphEdgesEqual(a.GraphEdges, b.GraphEdges) &&
		badgesEqual(a.Badges, b.Badges) &&
		a.Check == b.Check &&
		a.Checks == b.Checks &&
		a.ContextMenu == b.ContextMenu &&
		a.ContextArg == b.ContextArg &&
		rowsEqual(a.Rows, b.Rows)
}

func seriesSliceEqual(a, b []*Series) bool {
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

func graphNodesEqual(a, b []*GraphNode) bool {
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

func graphEdgesEqual(a, b []*GraphEdge) bool {
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

func badgesEqual(a, b []*Badge) bool {
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

func rowsEqual(a, b []*Row) bool {
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
			!badgesEqual(ra.Badges, rb.Badges) ||
			ra.Tone != rb.Tone ||
			ra.Strong != rb.Strong ||
			ra.Icon != rb.Icon {
			return false
		}
	}
	return true
}
