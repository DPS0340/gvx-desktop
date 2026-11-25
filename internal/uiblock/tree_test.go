package uiblock

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestTreeIDsAndActions(t *testing.T) {
	tr, err := Read([]byte(`{"kind":"column","children":[
		{"kind":"text","text":"a"},
		{"kind":"list","key":"runs","children":[{"kind":"row","key":"7","onTap":["open",7],"children":[{"kind":"pill","text":"ok","tone":"success"}]}]},
		{"kind":"input","key":"q","value":"x","placeholder":"Find","onSubmit":["find"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{tr.Root.ID, tr.Root.Children[0].ID, tr.Root.Children[1].Children[0].ID, tr.Root.Children[2].ID}
	if fmt.Sprint(ids) != "[ /#0 /runs/7 /q]" {
		t.Fatalf("ids %v", ids)
	}
	if string(tr.actions["/runs/7"].tap) != `["open",7]` || !tr.Root.Children[1].Children[0].Tap {
		t.Fatalf("the row's tap: %v", tr.actions)
	}
	if !tr.Root.Children[2].Change || tr.Root.Children[2].Text != "Find" {
		t.Fatalf("the input: %v", tr.Root.Children[2])
	}
}

func TestRootKeyAndTrailingJSON(t *testing.T) {
	// Empty/omitted or valid non-empty root key succeeds with root ID == "".
	tr, err := Read([]byte(`{"kind":"text","key":"root-view","text":"hi"}`))
	if err != nil {
		t.Fatalf("valid root key rejected: %v", err)
	}
	if tr.Root.ID != "" {
		t.Fatalf("root ID = %q, want empty", tr.Root.ID)
	}

	// Invalid root keys are rejected.
	for _, bad := range []string{
		`{"kind":"text","key":"bad/key","text":"hi"}`,
		`{"kind":"text","key":"bad#key","text":"hi"}`,
		`{"kind":"text","key":"` + strings.Repeat("k", maxKey+1) + `","text":"hi"}`,
		`{"kind":"text","key":"bad\u202ekey","text":"hi"}`,
	} {
		if _, err := Read([]byte(bad)); err == nil {
			t.Errorf("invalid root key accepted: %s", bad)
		}
	}

	// Trailing JSON values or tokens are rejected.
	for _, bad := range []string{
		`{"kind":"text","text":"a"}{"kind":"text","text":"b"}`,
		`{"kind":"text","text":"a"} 123`,
		`{"kind":"text","text":"a"} trailing`,
	} {
		if _, err := Read([]byte(bad)); err == nil {
			t.Errorf("trailing JSON accepted: %s", bad)
		}
	}
}

func TestPasswordInputAndVariants(t *testing.T) {
	tr, err := Read([]byte(`{"kind":"input","key":"tok","variant":"password","placeholder":"API Token","onChange":["set_tok"]}`))
	if err != nil {
		t.Fatalf("input(variant=password) failed: %v", err)
	}
	if tr.Root.Variant != "password" {
		t.Fatalf("variant = %q, want password", tr.Root.Variant)
	}
	if _, err := Read([]byte(`{"kind":"input","variant":"primary","placeholder":"API Token"}`)); err == nil || !strings.Contains(err.Error(), "takes no variant") {
		t.Fatalf("unexpected result for invalid input variant: %v", err)
	}
	if !strings.Contains(strings.Join(VariantsOf(KindInput), ","), "password") {
		t.Fatalf("VariantsOf(KindInput) = %v, want password", VariantsOf(KindInput))
	}
}

func TestLucideIconValidation(t *testing.T) {
	valid := []string{
		`{"kind":"icon","icon":"workflow"}`,
		`{"kind":"button","text":"Branch","icon":"git-branch","onTap":["go"]}`,
		`{"kind":"text","text":"Changed","badges":[{"text":"1","icon":"file-diff"}]}`,
		`{"kind":"dag","rows":[{"id":"r1","text":"Sync","icon":"refresh-cw","badges":[{"text":"ok","icon":"check"}]}]}`,
	}
	for _, raw := range valid {
		if _, err := Read([]byte(raw)); err != nil {
			t.Errorf("valid Lucide icon tree %s failed: %v", raw, err)
		}
	}

	invalid := []string{
		`{"kind":"icon","icon":"not-a-lucide-icon"}`,
		`{"kind":"button","text":"Bad","icon":"definitely-fake-icon"}`,
		`{"kind":"text","text":"Bad","badges":[{"text":"1","icon":"not-a-lucide-icon"}]}`,
		`{"kind":"dag","rows":[{"id":"r1","text":"Bad","icon":"not-a-lucide-icon"}]}`,
		`{"kind":"dag","rows":[{"id":"r1","text":"Bad","badges":[{"text":"x","icon":"not-a-lucide-icon"}]}]}`,
		`{"kind":"icon","icon":"BadCaps"}`,
	}
	for _, raw := range invalid {
		if _, err := Read([]byte(raw)); err == nil {
			t.Errorf("nonexistent Lucide icon tree accepted: %s", raw)
		}
	}
}

func TestTreeBoundsAndLimits(t *testing.T) {
	// Over 512 KiB JSON.
	oversizeJSON := `{"kind":"text","text":"` + strings.Repeat("a", maxTreeJSON) + `"}`
	if _, err := Read([]byte(oversizeJSON)); err == nil || !strings.Contains(err.Error(), "512 KiB") {
		t.Fatalf("oversize JSON: %v", err)
	}

	// Over 2000 nodes.
	children := make([]string, maxNodes)
	for i := range children {
		children[i] = `{"kind":"text","text":"x"}`
	}
	if _, err := Read([]byte(`{"kind":"column","children":[` + strings.Join(children, ",") + `]}`)); err == nil || !strings.Contains(err.Error(), "2000 nodes") {
		t.Fatalf("over maxNodes: %v", err)
	}

	// Depth > 24.
	deep := `{"kind":"text","text":"leaf"}`
	for range maxDepth {
		deep = `{"kind":"column","children":[` + deep + `]}`
	}
	if _, err := Read([]byte(deep)); err == nil || !strings.Contains(err.Error(), "deeper than 24") {
		t.Fatalf("over maxDepth: %v", err)
	}

	// Node text limits: 8 KiB for text, 32 KiB for markdown/code.
	if _, err := Read([]byte(`{"kind":"text","text":"` + strings.Repeat("a", maxNodeText+1) + `"}`)); err == nil || !strings.Contains(err.Error(), "8 KiB") {
		t.Fatalf("over maxNodeText: %v", err)
	}
	if _, err := Read([]byte(`{"kind":"markdown","text":"` + strings.Repeat("a", maxLongText) + `"}`)); err != nil {
		t.Fatalf("32 KiB markdown should pass: %v", err)
	}
	if _, err := Read([]byte(`{"kind":"code","text":"` + strings.Repeat("a", maxLongText+1) + `"}`)); err == nil || !strings.Contains(err.Error(), "32 KiB") {
		t.Fatalf("over maxLongText: %v", err)
	}

	// Tree total text > 256 KiB.
	var bigKids []string
	for range 9 {
		bigKids = append(bigKids, `{"kind":"code","text":"`+strings.Repeat("a", maxLongText)+`"}`)
	}
	if _, err := Read([]byte(`{"kind":"column","children":[` + strings.Join(bigKids, ",") + `]}`)); err == nil || !strings.Contains(err.Error(), "256 KiB") {
		t.Fatalf("over maxTreeText: %v", err)
	}

	// Child key > 128B or containing / or #.
	for _, badKey := range []string{strings.Repeat("k", maxKey+1), "a/b", "a#b"} {
		raw := fmt.Sprintf(`{"kind":"column","children":[{"kind":"text","key":%q,"text":"a"}]}`, badKey)
		if _, err := Read([]byte(raw)); err == nil {
			t.Errorf("bad child key %q accepted", badKey)
		}
	}

	// Unknown JSON field rejected.
	if _, err := Read([]byte(`{"kind":"text","text":"hi","color":"#fff"}`)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field: %v", err)
	}
}

func TestSpoofingAndPrintable(t *testing.T) {
	for _, r := range []rune{'\x00', '\x1b', '\x07', '\n', '\t', 0x202A, 0x202E, 0x2066, 0x2069, 0x200E, 0x200F, 0x061C, 0x200B, 0x200C, 0x200D, 0xFEFF} {
		if !Spoofing(r) {
			t.Errorf("Spoofing(%U) = false, want true", r)
		}
	}
	for _, r := range []rune{'a', ' ', '가', '✓'} {
		if Spoofing(r) {
			t.Errorf("Spoofing(%U) = true, want false", r)
		}
	}

	plain := "line1\n\tline2"
	if got := Printable(plain); got != plain {
		t.Fatalf("Printable(%q) = %q, want unchanged", plain, got)
	}
	if got := Printable("esc\x1b[31m\u202ebidi\u200b"); got != `esc\x1b[31m\u202ebidi\u200b` {
		t.Fatalf("Printable escaped = %q", got)
	}
}

func TestDragAndDropValidationAndMessages(t *testing.T) {
	tr, err := Read([]byte(`{"kind":"row","children":[
		{"kind":"column","key":"todo","onDrop":["move","todo"],"children":[
			{"kind":"column","key":"c1","drag":"card-1","onTap":["select","c1"],"onDrop":["before","todo","c1"],"children":[
				{"kind":"text","text":"First card"}]}]},
		{"kind":"column","key":"done","onDrop":["move","done"],"children":[
			{"kind":"text","text":"Empty"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	card := NodeOf(tr.Root, "/todo/c1")
	if card == nil || card.Drag != "card-1" || !card.Drop || !card.Tap {
		t.Fatalf("card = %+v", card)
	}
	doneCol := NodeOf(tr.Root, "/done")
	if doneCol == nil || doneCol.Drag != "" || !doneCol.Drop {
		t.Fatalf("done column = %+v", doneCol)
	}
	m, err := tr.Message(Event{Node: "/done", Kind: EventDrop, Value: "card-1"})
	if err != nil || m.Name != "move" || len(m.Args) != 2 || string(m.Args[0]) != `"done"` || string(m.Args[1]) != `"card-1"` {
		t.Fatalf("drop message = %+v, err = %v", m, err)
	}
	for _, ev := range []Event{
		{Node: "/done", Kind: EventDrop, Value: ""},
		{Node: "/done", Kind: EventDrop, Value: "bad\x00id"},
		{Node: "/done/#0", Kind: EventDrop, Value: "card-1"},
	} {
		if _, err := tr.Message(ev); err == nil {
			t.Errorf("event %+v should have been refused", ev)
		}
	}
	for _, bad := range []string{
		`{"kind":"input","drag":"x"}`,
		`{"kind":"input","onDrop":["move"]}`,
		`{"kind":"divider","onDrop":["move"]}`,
		`{"kind":"text","text":"x","drag":"bad\u202eid"}`,
		`{"kind":"text","text":"x","drag":"` + strings.Repeat("a", maxDrag+1) + `"}`,
		`{"kind":"text","text":"x","dragGroup":"card"}`,
		`{"kind":"column","drag":"c1","dragGroup":"Bad Group"}`,
		`{"kind":"column","dropAccept":["card"]}`,
		`{"kind":"column","onDrop":["move"],"dropAccept":["Bad Group"]}`,
	} {
		if _, err := Read([]byte(bad)); err == nil {
			t.Errorf("bad tree %s was accepted", bad)
		}
	}
}

func TestRichUIKindsAndDragGroups(t *testing.T) {
	tr, err := Read([]byte(`{"kind":"column","children":[
		{"kind":"tabs","key":"mode","value":"Board","options":["Board","Table"],"onChange":["mode"]},
		{"kind":"scroll","key":"sc","direction":"vertical","maxHeight":360,"onDrop":["move","todo"],"dropAccept":["card"],"children":[
			{"kind":"card","key":"c1","drag":"c1","dragGroup":"card","onTap":["pick","c1"],"children":[
				{"kind":"text","text":"Card 1"},
				{"kind":"textarea","key":"note","value":"hello","placeholder":"Notes","onChange":["note"]}
			]}
		]},
		{"kind":"table","key":"tbl","labels":["Title","Status"],"children":[
			{"kind":"row","key":"r1","children":[{"kind":"text","text":"Card 1"},{"kind":"pill","text":"Todo"}]}
		]}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	sc := NodeOf(tr.Root, "/sc")
	if sc == nil || sc.Direction != "vertical" || sc.MaxHeight != 360 || len(sc.DropAccept) != 1 || sc.DropAccept[0] != "card" {
		t.Fatalf("scroll = %+v", sc)
	}
	c1 := NodeOf(tr.Root, "/sc/c1")
	if c1 == nil || c1.Kind != KindCard || c1.DragGroup != "card" {
		t.Fatalf("card = %+v", c1)
	}
	vals, err := tr.Values(map[string]string{"/mode": "Table", "/sc/c1/note": "updated\nline 2"})
	if err != nil || vals["/mode"] != "Table" || vals["/sc/c1/note"] != "updated\nline 2" {
		t.Fatalf("values = %v, err = %v", vals, err)
	}
	for _, bad := range []string{
		`{"kind":"scroll","direction":"diagonal","children":[]}`,
		`{"kind":"scroll","maxHeight":-10,"children":[]}`,
		`{"kind":"column","direction":"horizontal","children":[]}`,
		`{"kind":"column","maxHeight":200,"children":[]}`,
		`{"kind":"tabs","options":[]}`,
		`{"kind":"tabs","options":["a","b"],"value":"c"}`,
		`{"kind":"table","labels":["a"],"children":[{"kind":"text","text":"not a row"}]}`,
		`{"kind":"textarea","children":[{"kind":"text","text":"bad"}]}`,
	} {
		if _, err := Read([]byte(bad)); err == nil {
			t.Errorf("bad rich UI tree %s was accepted", bad)
		}
	}
}

func TestRichUIv2KindsEventsAndDiffPatch(t *testing.T) {
	beforeJSON := `{
		"kind": "split",
		"fill": true,
		"direction": "horizontal",
		"ratio": 0.3,
		"children": [
			{
				"kind": "column",
				"key": "left",
				"gap": 8,
				"children": [
					{"kind": "breadcrumb", "key": "crumb", "options": ["Vault", "Notes", "Design"], "onChange": ["nav"]},
					{"kind": "tree", "key": "files", "onDrop": ["moveRoot"], "dropAccept": ["note"], "children": [
						{"kind": "tree_item", "key": "f1", "text": "Projects", "value": "2", "icon": "folder", "tone": "accent", "checked": true, "onTap": ["open", "f1"], "onChange": ["toggle", "f1"], "onDrop": ["moveInto", "f1"], "dropAccept": ["note"], "children": [
							{"kind": "tree_item", "key": "n1", "text": "Spec.md", "value": "4 KB", "icon": "file-text", "drag": "n1", "dragGroup": "note", "onTap": ["open", "n1"]}
						]}
					]},
					{"kind": "collapsible", "key": "meta", "text": "Metadata", "icon": "tag", "tone": "muted", "checked": false, "onChange": ["toggleMeta"], "children": [
						{"kind": "wrap", "key": "tags", "gap": 4, "children": [
							{"kind": "chip", "text": "arch"},
							{"kind": "chip", "text": "v2"}
						]}
					]}
				]
			},
			{
				"kind": "column",
				"key": "right",
				"gap": 8,
				"children": [
					{"kind": "grid", "key": "cards", "columns": 3, "gap": 8, "children": [
						{"kind": "card", "key": "k1", "variant": "outline", "children": [
							{"kind": "button", "key": "iconOnly", "icon": "plus", "size": "small", "variant": "ghost", "onTap": ["add"]}
						]},
						{"kind": "card", "key": "k2", "variant": "primary", "children": [
							{"kind": "button", "key": "save", "text": "Save", "size": "large", "variant": "primary", "onTap": ["save"]}
						]},
						{"kind": "textarea", "key": "ed", "value": "hello", "size": "small", "onChange": ["edit"]}
					]},
					{"kind": "markdown", "key": "md", "text": "See [[Spec]]", "onLink": ["follow"]},
					{"kind": "graph", "key": "g", "text": "Links", "value": "a", "maxHeight": 320, "onChange": ["pickNode"],
						"graphNodes": [
							{"id": "a", "label": "Alpha", "group": "core", "tone": "accent", "subtitle": "root"},
							{"id": "b", "label": "Beta", "group": "leaf", "tone": "success", "weight": 2.5}
						],
						"graphEdges": [
							{"from": "a", "to": "b", "label": "links", "tone": "muted", "dashed": true}
						]
					}
				]
			}
		]
	}`
	tr, err := Read([]byte(beforeJSON))
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if !tr.Root.Fill || tr.Root.Direction != "horizontal" || tr.Root.Ratio != 0.3 {
		t.Fatalf("root split = %+v", tr.Root)
	}
	grid := NodeOf(tr.Root, "/right/cards")
	if grid == nil || grid.Columns != 3 {
		t.Fatalf("grid = %+v", grid)
	}
	iconBtn := NodeOf(tr.Root, "/right/cards/k1/iconOnly")
	if iconBtn == nil || iconBtn.Text != "" || iconBtn.Icon != "plus" || iconBtn.Size != "small" || iconBtn.Variant != "ghost" {
		t.Fatalf("iconBtn = %+v", iconBtn)
	}
	md := NodeOf(tr.Root, "/right/md")
	if md == nil || !md.Change {
		t.Fatalf("markdown = %+v", md)
	}
	g := NodeOf(tr.Root, "/right/g")
	if g == nil || len(g.GraphNodes) != 2 || g.GraphNodes[0].Weight != 1 || g.GraphNodes[1].Weight != 2.5 ||
		len(g.GraphEdges) != 1 || !g.GraphEdges[0].Dashed || g.Value != "a" || g.MaxHeight != 320 || !g.Change {
		t.Fatalf("graph = %+v", g)
	}

	if m, err := tr.Message(Event{Node: "/left/crumb", Kind: EventChange, Value: "1"}); err != nil || m.Name != "nav" || string(m.Args[0]) != `"1"` {
		t.Fatalf("breadcrumb event = %+v, %v", m, err)
	}
	if m, err := tr.Message(Event{Node: "/left/meta", Kind: EventChange, Value: "true"}); err != nil || m.Name != "toggleMeta" || string(m.Args[0]) != "true" {
		t.Fatalf("collapsible event = %+v, %v", m, err)
	}
	if m, err := tr.Message(Event{Node: "/left/files/f1", Kind: EventTap}); err != nil || m.Name != "open" || string(m.Args[0]) != `"f1"` {
		t.Fatalf("tree_item tap = %+v, %v", m, err)
	}
	if m, err := tr.Message(Event{Node: "/left/files/f1", Kind: EventChange, Value: "false"}); err != nil || m.Name != "toggle" || string(m.Args[1]) != "false" {
		t.Fatalf("tree_item change = %+v, %v", m, err)
	}
	if m, err := tr.Message(Event{Node: "/left/files/f1", Kind: EventDrop, Value: "n1"}); err != nil || m.Name != "moveInto" || string(m.Args[1]) != `"n1"` {
		t.Fatalf("tree_item drop = %+v, %v", m, err)
	}
	if m, err := tr.Message(Event{Node: "/right/md", Kind: EventChange, Value: "Spec"}); err != nil || m.Name != "follow" || string(m.Args[0]) != `"Spec"` {
		t.Fatalf("markdown onLink = %+v, %v", m, err)
	}
	if m, err := tr.Message(Event{Node: "/right/g", Kind: EventChange, Value: "b"}); err != nil || m.Name != "pickNode" || string(m.Args[0]) != `"b"` {
		t.Fatalf("graph event = %+v, %v", m, err)
	}

	defSplit, err := Read([]byte(`{"kind":"split","children":[{"kind":"text","text":"L"},{"kind":"text","text":"R"}]}`))
	if err != nil || defSplit.Root.Ratio != 0.5 {
		t.Fatalf("default split ratio = %v, err = %v", defSplit, err)
	}

	next := cloneNode(tr.Root)
	next.Fill = false
	next.Ratio = 0.65
	next.Direction = "vertical"
	NodeOf(next, "/right/cards").Columns = 2
	NodeOf(next, "/right/cards/k1").Variant = "secondary"
	NodeOf(next, "/right/cards/k2/save").Variant = "danger"
	ng := NodeOf(next, "/right/g")
	ng.Value = "b"
	ng.MaxHeight = 480
	ng.GraphNodes = append(ng.GraphNodes, &GraphNode{ID: "c", Label: "Gamma", Tone: "warning", Weight: 1.5})
	ng.GraphEdges = []*GraphEdge{
		{From: "a", To: "b", Label: "refs", Tone: "accent", Dashed: false},
		{From: "b", To: "c", Label: "next"},
	}
	ops := Diff(tr.Root, next)
	if len(ops) == 0 {
		t.Fatal("expected non-empty Op diff for updated rich UI v2 fields")
	}
	got, err := Apply(tr.Root, ops)
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	if !got.Equal(next) {
		t.Fatalf("Apply mismatch:\ngot:  %+v\nwant: %+v", got, next)
	}
}

func TestRichUIv2Rejections(t *testing.T) {
	var manyNodes []string
	for i := range maxGraphNodes + 1 {
		manyNodes = append(manyNodes, fmt.Sprintf(`{"id":"n%d","label":"N%d"}`, i, i))
	}
	var manyEdges []string
	for range maxGraphEdges + 1 {
		manyEdges = append(manyEdges, `{"from":"a","to":"b"}`)
	}
	cases := map[string]struct{ tree, want string }{
		"variant-bad":       {`{"kind":"button","text":"x","variant":"neon"}`, "takes no variant"},
		"variant-text":      {`{"kind":"text","text":"x","variant":"primary"}`, "takes no variant"},
		"button-blank":      {`{"kind":"button"}`, "text or icon"},
		"grid-zero":         {`{"kind":"grid","columns":0,"children":[]}`, "1 to 6 columns"},
		"grid-seven":        {`{"kind":"grid","columns":7,"children":[]}`, "1 to 6 columns"},
		"columns-on-col":    {`{"kind":"column","columns":2,"children":[]}`, "only a grid"},
		"split-one":         {`{"kind":"split","children":[{"kind":"text","text":"a"}]}`, "exactly 2 children"},
		"split-three":       {`{"kind":"split","children":[{"kind":"text","text":"a"},{"kind":"text","text":"b"},{"kind":"text","text":"c"}]}`, "exactly 2 children"},
		"split-ratio-zero":  {`{"kind":"split","ratio":0,"children":[{"kind":"text","text":"a"},{"kind":"text","text":"b"}]}`, "ratio must be"},
		"split-ratio-low":   {`{"kind":"split","ratio":0.05,"children":[{"kind":"text","text":"a"},{"kind":"text","text":"b"}]}`, "ratio must be"},
		"split-ratio-high":  {`{"kind":"split","ratio":0.95,"children":[{"kind":"text","text":"a"},{"kind":"text","text":"b"}]}`, "ratio must be"},
		"ratio-on-row":      {`{"kind":"row","ratio":0.5,"children":[]}`, "only a split"},
		"fill-non-root":     {`{"kind":"column","children":[{"kind":"column","fill":true,"children":[]}]}`, "fill is only allowed"},
		"fill-on-card":      {`{"kind":"card","fill":true,"children":[]}`, "fill is only allowed"},
		"crumb-empty":       {`{"kind":"breadcrumb","options":[]}`, "at least one option"},
		"crumb-spoof":       {`{"kind":"breadcrumb","options":["a\u202eb"]}`, "reordering"},
		"crumb-tap":         {`{"kind":"breadcrumb","options":["a"],"onTap":["x"]}`, "onChange, not onTap"},
		"crumb-drag":        {`{"kind":"breadcrumb","options":["a"],"drag":"x"}`, "does not take drag"},
		"crumb-children":    {`{"kind":"breadcrumb","options":["a"],"children":[{"kind":"text","text":"x"}]}`, "holds no children"},
		"collapsible-tap":   {`{"kind":"collapsible","text":"S","onTap":["x"],"children":[]}`, "onChange, not onTap"},
		"tree-bad-child":    {`{"kind":"tree","children":[{"kind":"text","text":"x"}]}`, "must be a tree_item"},
		"tree-item-root":    {`{"kind":"tree_item","text":"x"}`, "must be inside a tree"},
		"tree-item-in-col":  {`{"kind":"column","children":[{"kind":"tree_item","text":"x"}]}`, "must be inside a tree"},
		"tree-item-child":   {`{"kind":"tree","children":[{"kind":"tree_item","text":"a","children":[{"kind":"text","text":"b"}]}]}`, "must be a tree_item"},
		"tree-item-spoof":   {`{"kind":"tree","children":[{"kind":"tree_item","text":"a","value":"bad\u202eval"}]}`, "reordering"},
		"md-onChange":       {`{"kind":"markdown","text":"hi","onChange":["x"]}`, "takes onLink"},
		"md-onTap":          {`{"kind":"markdown","text":"hi","onTap":["x"]}`, "takes onLink"},
		"md-drag":           {`{"kind":"markdown","text":"hi","drag":"x"}`, "does not take drag"},
		"onLink-on-text":    {`{"kind":"text","text":"hi","onLink":["x"]}`, "only a markdown"},
		"graph-too-many-n":  {`{"kind":"graph","graphNodes":[` + strings.Join(manyNodes, ",") + `]}`, "at most 120 nodes"},
		"graph-too-many-e":  {`{"kind":"graph","graphNodes":[{"id":"a","label":"A"},{"id":"b","label":"B"}],"graphEdges":[` + strings.Join(manyEdges, ",") + `]}`, "at most 400 edges"},
		"graph-dup-id":      {`{"kind":"graph","graphNodes":[{"id":"a","label":"A"},{"id":"a","label":"A2"}]}`, "duplicated"},
		"graph-empty-id":    {`{"kind":"graph","graphNodes":[{"id":"","label":"A"}]}`, "invalid id"},
		"graph-empty-label": {`{"kind":"graph","graphNodes":[{"id":"a","label":""}]}`, "invalid label"},
		"graph-spoof-label": {`{"kind":"graph","graphNodes":[{"id":"a","label":"A\u202eB"}]}`, "invalid label"},
		"graph-bad-tone":    {`{"kind":"graph","graphNodes":[{"id":"a","label":"A","tone":"#fff"}]}`, "colour token"},
		"graph-weight-zero": {`{"kind":"graph","graphNodes":[{"id":"a","label":"A","weight":0}]}`, "finite positive"},
		"graph-weight-neg":  {`{"kind":"graph","graphNodes":[{"id":"a","label":"A","weight":-1}]}`, "finite positive"},
		"graph-bad-from":    {`{"kind":"graph","graphNodes":[{"id":"a","label":"A"}],"graphEdges":[{"from":"x","to":"a"}]}`, "unknown source"},
		"graph-bad-to":      {`{"kind":"graph","graphNodes":[{"id":"a","label":"A"}],"graphEdges":[{"from":"a","to":"y"}]}`, "unknown target"},
		"graph-edge-tone":   {`{"kind":"graph","graphNodes":[{"id":"a","label":"A"}],"graphEdges":[{"from":"a","to":"a","tone":"pink"}]}`, "colour token"},
		"graph-bad-value":   {`{"kind":"graph","value":"missing","graphNodes":[{"id":"a","label":"A"}]}`, "does not match any node"},
		"graph-height-low":  {`{"kind":"graph","maxHeight":80,"graphNodes":[]}`, "120 to 1200"},
		"graph-height-high": {`{"kind":"graph","maxHeight":1500,"graphNodes":[]}`, "120 to 1200"},
		"graph-elsewhere":   {`{"kind":"text","text":"x","graphNodes":[{"id":"a","label":"A"}]}`, "only a graph"},
		"graph-tap":         {`{"kind":"graph","onTap":["x"],"graphNodes":[]}`, "onChange, not onTap"},
		"graph-children":    {`{"kind":"graph","graphNodes":[],"children":[{"kind":"text","text":"x"}]}`, "holds no children"},
	}
	for name, c := range cases {
		if _, err := Read([]byte(c.tree)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want substring %q", name, err, c.want)
		}
	}
}

func TestVariantByKind(t *testing.T) {
	valid := []string{
		`{"kind":"card","variant":"soft","tone":"accent","children":[]}`,
		`{"kind":"pill","text":"on","variant":"soft","icon":"sparkles"}`,
		`{"kind":"pill","text":"on","variant":"solid","tone":"success"}`,
		`{"kind":"pill","text":"on","variant":"outline","tone":"warning"}`,
		`{"kind":"chip","text":"tag","variant":"soft","icon":"tag"}`,
		`{"kind":"chip","text":"tag","variant":"solid"}`,
		`{"kind":"chip","text":"tag","variant":"outline"}`,
		`{"kind":"input","variant":"password","placeholder":"Secret"}`,
		`{"kind":"progress","progress":0,"variant":"spinner","text":"Loading..."}`,
		`{"kind":"scroll","variant":"follow","children":[]}`,
		`{"kind":"tree","children":[{"kind":"tree_item","text":"docs","value":"3","variant":"badge","icon":"folder"}]}`,
	}
	for _, raw := range valid {
		if _, err := Read([]byte(raw)); err != nil {
			t.Errorf("valid %s failed: %v", raw, err)
		}
	}
	invalid := map[string]string{
		"button-soft":     `{"kind":"button","text":"x","variant":"soft"}`,
		"pill-primary":    `{"kind":"pill","text":"x","variant":"primary"}`,
		"chip-spinner":    `{"kind":"chip","text":"x","variant":"spinner"}`,
		"input-primary":   `{"kind":"input","variant":"primary"}`,
		"progress-follow": `{"kind":"progress","progress":0,"variant":"follow"}`,
		"scroll-badge":    `{"kind":"scroll","variant":"badge","children":[]}`,
		"tree-item-soft":  `{"kind":"tree","children":[{"kind":"tree_item","text":"a","variant":"soft"}]}`,
		"row-soft":        `{"kind":"row","variant":"soft","children":[]}`,
	}
	for name, raw := range invalid {
		if _, err := Read([]byte(raw)); err == nil || !strings.Contains(err.Error(), "takes no variant") {
			t.Errorf("%s: got %v, want takes no variant error", name, err)
		}
	}
}

func TestSpoofingChecksAllFieldsAndAllowsMultilineTextarea(t *testing.T) {
	rejected := map[string]string{
		"input-placeholder-bidi":      `{"kind":"input","placeholder":"Name\u202e"}`,
		"input-placeholder-with-text": `{"kind":"input","text":"Name","placeholder":"bad\u001b"}`,
		"input-placeholder-nul":       `{"kind":"input","placeholder":"bad\u0000"}`,
		"textarea-placeholder-bidi":   `{"kind":"textarea","placeholder":"Notes\u202e"}`,
		"input-value-bidi":            `{"kind":"input","value":"v\u202e"}`,
		"input-value-ctrl":            `{"kind":"input","value":"v\u0007"}`,
		"textarea-value-bidi":         `{"kind":"textarea","value":"line1\n\u202eline2"}`,
		"select-value-bidi":           `{"kind":"select","value":"a\u202eb","options":["a","b"]}`,
		"select-option-bidi":          `{"kind":"select","options":["ok","bad\u202eopt"]}`,
		"select-option-newline":       `{"kind":"select","options":["ok","bad\nopt"]}`,
		"tabs-option-bidi":            `{"kind":"tabs","options":["one","two\u202e"]}`,
	}
	for name, raw := range rejected {
		if _, err := Read([]byte(raw)); err == nil || !strings.Contains(err.Error(), "reordering") {
			t.Errorf("%s: got %v, want reordering error", name, err)
		}
	}

	valid := []string{
		`{"kind":"textarea","value":"line 1\n\tindented line 2","placeholder":"first line\n\tsecond line"}`,
		`{"kind":"input","placeholder":"Your name","value":"Ada"}`,
		`{"kind":"select","value":"M","options":["S","M","L"]}`,
		`{"kind":"tabs","value":"Overview","options":["Overview","Logs"]}`,
	}
	for _, raw := range valid {
		if _, err := Read([]byte(raw)); err != nil {
			t.Errorf("valid %s failed: %v", raw, err)
		}
	}
}

func TestRev46KindsEventsAndValidation(t *testing.T) {
	treeJSON := `{
		"kind": "column",
		"fill": true,
		"children": [
			{
				"kind": "row",
				"key": "bar",
				"variant": "toolbar",
				"children": [
					{
						"kind": "menu",
						"key": "branch",
						"text": "main",
						"icon": "git-branch",
						"variant": "ghost",
						"children": [
							{"kind": "menu_item", "key": "m1", "text": "main", "checked": true, "onTap": ["checkout", "main"]},
							{"kind": "divider"},
							{
								"kind": "menu",
								"key": "sub",
								"text": "More",
								"children": [
									{"kind": "menu_item", "key": "new", "text": "New branch", "icon": "plus", "onTap": ["askBranch"]}
								]
							}
						]
					},
					{"kind": "button", "key": "pull", "text": "Pull", "badges": [{"text": "2", "tone": "accent", "variant": "soft"}], "onTap": ["pull"]}
				]
			},
			{
				"kind": "split",
				"key": "sp",
				"direction": "auto",
				"ratio": 0.45,
				"children": [
					{
						"kind": "column",
						"key": "left",
						"children": [
							{
								"kind": "tree",
								"key": "changes",
								"children": [
									{
										"kind": "tree_item",
										"key": "f1",
										"text": "main.go",
										"icon": "file-diff",
										"check": "mixed",
										"onCheck": ["pick", "src"],
										"badges": [
											{"text": "+12", "tone": "success", "variant": "mono"},
											{"text": "-3", "tone": "danger", "variant": "mono"}
										],
										"contextMenu": "file-ctx",
										"contextArg": "src/main.go",
										"onTap": ["open", "src/main.go"]
									}
								]
							},
							{"kind": "checkbox", "key": "amend", "text": "Amend", "check": "off", "onChange": ["toggleAmend"]},
							{"kind": "checkbox", "key": "defcheck", "text": "Default off", "onChange": ["toggleDef"]}
						]
					},
					{
						"kind": "dag",
						"key": "history",
						"value": "c1",
						"maxHeight": 500,
						"contextMenu": "commit-ctx",
						"onChange": ["selectCommit"],
						"rows": [
							{
								"id": "c1",
								"parents": ["c0"],
								"text": "Initial commit",
								"detail": "Ada",
								"meta": "2m",
								"strong": true,
								"tone": "accent",
								"icon": "git-commit-horizontal",
								"badges": [{"text": "HEAD -> main", "tone": "accent", "variant": "soft", "icon": "git-branch"}]
							},
							{"id": "c0", "text": "Root"}
						]
					}
				]
			},
			{
				"kind": "dialog",
				"key": "dlg",
				"text": "Discard changes?",
				"variant": "danger",
				"onChange": ["closeDialog"],
				"children": [
					{"kind": "text", "text": "Are you sure?"},
					{"kind": "button", "key": "ok", "text": "Discard", "variant": "danger", "onTap": ["confirmDiscard"]}
				]
			},
			{
				"kind": "menu",
				"key": "file-ctx",
				"variant": "context",
				"children": [
					{"kind": "menu_item", "key": "discard", "text": "Discard", "tone": "danger", "onTap": ["discardFile", "worktree"]},
					{
						"kind": "menu",
						"key": "more",
						"text": "More",
						"children": [
							{"kind": "menu_item", "key": "ignore", "text": "Ignore", "onTap": ["ignoreFile"]}
						]
					}
				]
			},
			{
				"kind": "menu",
				"key": "commit-ctx",
				"variant": "context",
				"children": [
					{"kind": "menu_item", "key": "copy", "text": "Copy hash", "onTap": ["copyHash"]}
				]
			},
			{
				"kind": "menu",
				"key": "unused-ctx",
				"variant": "context",
				"children": [
					{"kind": "menu_item", "key": "never", "text": "Never", "onTap": ["never"]}
				]
			}
		]
	}`

	tr, err := Read([]byte(treeJSON))
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	bar := NodeOf(tr.Root, "/bar")
	if bar == nil || bar.Variant != "toolbar" {
		t.Fatalf("toolbar = %+v", bar)
	}
	sp := NodeOf(tr.Root, "/sp")
	if sp == nil || sp.Direction != "auto" || sp.Ratio != 0.45 {
		t.Fatalf("split = %+v", sp)
	}
	f1 := NodeOf(tr.Root, "/sp/left/changes/f1")
	if f1 == nil || f1.Check != "mixed" || !f1.Checks || len(f1.Badges) != 2 || f1.ContextMenu != "file-ctx" || f1.ContextArg != "src/main.go" {
		t.Fatalf("tree_item = %+v", f1)
	}
	defCheck := NodeOf(tr.Root, "/sp/left/defcheck")
	if defCheck == nil || defCheck.Check != "off" {
		t.Fatalf("default checkbox check = %+v", defCheck)
	}
	hist := NodeOf(tr.Root, "/sp/history")
	if hist == nil || len(hist.Rows) != 2 || !hist.Rows[0].Strong || hist.Rows[0].Badges[0].Text != "HEAD -> main" {
		t.Fatalf("dag = %+v", hist)
	}

	if m, err := tr.Message(Event{Node: "/bar/branch/m1", Kind: EventTap, Value: "ignored"}); err != nil || m.Name != "checkout" || len(m.Args) != 1 || string(m.Args[0]) != `"main"` {
		t.Fatalf("dropdown menu_item tap = %+v, %v", m, err)
	}
	if m, err := tr.Message(Event{Node: "/file-ctx/discard", Kind: EventTap, Value: "src/main.go"}); err != nil || m.Name != "discardFile" || len(m.Args) != 2 || string(m.Args[0]) != `"worktree"` || string(m.Args[1]) != `"src/main.go"` {
		t.Fatalf("context menu_item tap = %+v, %v", m, err)
	}
	if m, err := tr.Message(Event{Node: "/file-ctx/more/ignore", Kind: EventTap, Value: "src/main.go"}); err != nil || m.Name != "ignoreFile" || len(m.Args) != 1 || string(m.Args[0]) != `"src/main.go"` {
		t.Fatalf("context submenu_item tap = %+v, %v", m, err)
	}
	if _, err := tr.Message(Event{Node: "/file-ctx/discard", Kind: EventTap, Value: "../../etc/passwd"}); err == nil {
		t.Fatal("forged context_arg was accepted")
	}
	if _, err := tr.Message(Event{Node: "/unused-ctx/never", Kind: EventTap, Value: ""}); err == nil {
		t.Fatal("unreferenced context menu tap was accepted")
	}
	if m, err := tr.Message(Event{Node: "/commit-ctx/copy", Kind: EventTap, Value: "c1"}); err != nil || m.Name != "copyHash" || len(m.Args) != 1 || string(m.Args[0]) != `"c1"` {
		t.Fatalf("dag context menu tap = %+v, %v", m, err)
	}
	if _, err := tr.Message(Event{Node: "/commit-ctx/copy", Kind: EventTap, Value: "deadbeef"}); err == nil {
		t.Fatal("forged dag row context_arg was accepted")
	}
	if m, err := tr.Message(Event{Node: "/sp/left/changes/f1", Kind: EventCheck, Value: "on"}); err != nil || m.Name != "pick" || len(m.Args) != 2 || string(m.Args[0]) != `"src"` || string(m.Args[1]) != `"on"` {
		t.Fatalf("tree_item check = %+v, %v", m, err)
	}
	if _, err := tr.Message(Event{Node: "/sp/left/changes/f1", Kind: EventCheck, Value: "mixed"}); err == nil {
		t.Fatal("invalid check event value 'mixed' was accepted")
	}
	if m, err := tr.Message(Event{Node: "/sp/left/amend", Kind: EventChange, Value: "on"}); err != nil || m.Name != "toggleAmend" || len(m.Args) != 1 || string(m.Args[0]) != `"on"` {
		t.Fatalf("checkbox change = %+v, %v", m, err)
	}
	if _, err := tr.Message(Event{Node: "/sp/left/amend", Kind: EventChange, Value: "true"}); err == nil {
		t.Fatal("invalid checkbox change value 'true' was accepted")
	}
	if vals, err := tr.Values(map[string]string{"/sp/left/amend": "on"}); err != nil || vals["/sp/left/amend"] != "on" {
		t.Fatalf("checkbox form values = %v, %v", vals, err)
	}
	if m, err := tr.Message(Event{Node: "/dlg", Kind: EventChange, Value: "dismiss"}); err != nil || m.Name != "closeDialog" || len(m.Args) != 1 || string(m.Args[0]) != `"dismiss"` {
		t.Fatalf("dialog dismiss = %+v, %v", m, err)
	}
	if _, err := tr.Message(Event{Node: "/dlg", Kind: EventChange, Value: "other"}); err == nil {
		t.Fatal("invalid dialog change value was accepted")
	}
	if m, err := tr.Message(Event{Node: "/sp/history", Kind: EventChange, Value: "c0"}); err != nil || m.Name != "selectCommit" || len(m.Args) != 1 || string(m.Args[0]) != `"c0"` {
		t.Fatalf("dag select = %+v, %v", m, err)
	}
	if _, err := tr.Message(Event{Node: "/sp/history", Kind: EventChange, Value: "missing"}); err == nil {
		t.Fatal("dag unknown row selection was accepted")
	}
}

func TestRev46Rejections(t *testing.T) {
	var manyBadges []string
	for i := range maxBadges + 1 {
		manyBadges = append(manyBadges, fmt.Sprintf(`{"text":"b%d"}`, i))
	}
	var manyRowBadges []string
	for i := range maxDagRowBadges + 1 {
		manyRowBadges = append(manyRowBadges, fmt.Sprintf(`{"text":"b%d"}`, i))
	}
	var manyRows []string
	for i := range maxDagRows + 1 {
		manyRows = append(manyRows, fmt.Sprintf(`{"id":"r%d","text":"t"}`, i))
	}
	var manyParents []string
	for i := range maxDagParents + 1 {
		manyParents = append(manyParents, fmt.Sprintf(`"p%d"`, i))
	}

	cases := map[string]struct{ tree, want string }{
		"scroll-auto":          {`{"kind":"scroll","direction":"auto","children":[]}`, "not vertical or horizontal"},
		"split-bad-dir":        {`{"kind":"split","direction":"diagonal","children":[{"kind":"text","text":"a"},{"kind":"text","text":"b"}]}`, "vertical, horizontal or auto"},
		"menu-blank":           {`{"kind":"menu","children":[]}`, "menu requires text or icon"},
		"menu-ctx-no-key":      {`{"kind":"column","children":[{"kind":"menu","variant":"context","children":[]}]}`, "context menu requires a valid key"},
		"menu-ctx-dup-key":     {`{"kind":"column","children":[{"kind":"menu","key":"m","variant":"context","children":[]},{"kind":"column","children":[{"kind":"menu","key":"m","variant":"context","children":[]}]}]}`, "duplicate context menu key"},
		"menu-ctx-nested":      {`{"kind":"menu","text":"M","children":[{"kind":"menu","key":"c","variant":"context","children":[]}]}`, "context menu cannot be nested"},
		"menu-depth-3":         {`{"kind":"menu","text":"L1","children":[{"kind":"menu","text":"L2","children":[{"kind":"menu","text":"L3","children":[]}]}]}`, "menu nesting exceeds depth 2"},
		"menu-bad-child":       {`{"kind":"menu","text":"M","children":[{"kind":"text","text":"nope"}]}`, "must be a menu_item, divider or menu"},
		"menu-onTap":           {`{"kind":"menu","text":"M","onTap":["x"],"children":[]}`, "sends no messages"},
		"menu-item-root":       {`{"kind":"menu_item","text":"x"}`, "must be inside a menu"},
		"menu-item-in-col":     {`{"kind":"column","children":[{"kind":"menu_item","text":"x"}]}`, "must be inside a menu"},
		"menu-item-blank":      {`{"kind":"menu","text":"M","children":[{"kind":"menu_item","text":""}]}`, "menu_item requires text"},
		"menu-item-onChange":   {`{"kind":"menu","text":"M","children":[{"kind":"menu_item","text":"a","onChange":["x"]}]}`, "takes onTap, not onChange"},
		"menu-item-children":   {`{"kind":"menu","text":"M","children":[{"kind":"menu_item","text":"a","children":[]}]}`, ""},
		"menu-item-has-child":  {`{"kind":"menu","text":"M","children":[{"kind":"menu_item","text":"a","children":[{"kind":"text","text":"b"}]}]}`, "holds no children"},
		"checkbox-bad-check":   {`{"kind":"checkbox","text":"A","check":"yes"}`, "not on, off or mixed"},
		"checkbox-onTap":       {`{"kind":"checkbox","text":"A","onTap":["x"]}`, "onChange, not onTap"},
		"checkbox-onCheck":     {`{"kind":"checkbox","text":"A","check":"on","onCheck":["x"]}`, "only a tree_item takes onCheck"},
		"check-on-text":        {`{"kind":"text","text":"A","check":"on"}`, "only a checkbox or tree_item"},
		"onCheck-no-check":     {`{"kind":"tree","children":[{"kind":"tree_item","text":"a","onCheck":["x"]}]}`, "onCheck requires check"},
		"dialog-two":           {`{"kind":"column","children":[{"kind":"dialog","text":"1","children":[]},{"kind":"dialog","text":"2","children":[]}]}`, "at most one dialog"},
		"dialog-nested":        {`{"kind":"dialog","text":"1","children":[{"kind":"dialog","text":"2","children":[]}]}`, "at most one dialog"},
		"dialog-bad-variant":   {`{"kind":"dialog","text":"1","variant":"primary","children":[]}`, "takes no variant"},
		"dialog-onTap":         {`{"kind":"dialog","text":"1","onTap":["x"],"children":[]}`, "onChange, not onTap"},
		"badge-too-many":       {`{"kind":"text","text":"x","badges":[` + strings.Join(manyBadges, ",") + `]}`, "at most 8 badges"},
		"badge-empty":          {`{"kind":"text","text":"x","badges":[{}]}`, "requires text or icon"},
		"badge-too-long":       {`{"kind":"text","text":"x","badges":[{"text":"` + strings.Repeat("a", maxBadgeText+1) + `"}]}`, "over 32 runes"},
		"badge-bad-variant":    {`{"kind":"text","text":"x","badges":[{"text":"a","variant":"neon"}]}`, "not soft, solid, outline or mono"},
		"badge-bad-tone":       {`{"kind":"text","text":"x","badges":[{"text":"a","tone":"#f00"}]}`, "colour token"},
		"badge-spoof":          {`{"kind":"text","text":"x","badges":[{"text":"a\u202eb"}]}`, "reordering"},
		"ctx-unknown":          {`{"kind":"text","text":"x","contextMenu":"nope"}`, "unknown context menu"},
		"ctx-arg-no-menu":      {`{"kind":"text","text":"x","contextArg":"abc"}`, "contextArg requires contextMenu"},
		"ctx-arg-long":         {`{"kind":"column","children":[{"kind":"text","text":"x","contextMenu":"m","contextArg":"` + strings.Repeat("a", maxContextArg+1) + `"},{"kind":"menu","key":"m","variant":"context","children":[]}]}`, "over 256 characters"},
		"dag-too-many-rows":    {`{"kind":"dag","rows":[` + strings.Join(manyRows, ",") + `]}`, "at most 1000 rows"},
		"dag-dup-id":           {`{"kind":"dag","rows":[{"id":"a","text":"1"},{"id":"a","text":"2"}]}`, "duplicated"},
		"dag-empty-id":         {`{"kind":"dag","rows":[{"id":"","text":"1"}]}`, "invalid id"},
		"dag-long-id":          {`{"kind":"dag","rows":[{"id":"` + strings.Repeat("a", maxDagID+1) + `","text":"1"}]}`, "invalid id"},
		"dag-too-many-p":       {`{"kind":"dag","rows":[{"id":"a","parents":[` + strings.Join(manyParents, ",") + `],"text":"1"}]}`, "more than 8 parents"},
		"dag-long-text":        {`{"kind":"dag","rows":[{"id":"a","text":"` + strings.Repeat("x", maxDagText+1) + `"}]}`, "over 320 characters"},
		"dag-too-many-b":       {`{"kind":"dag","rows":[{"id":"a","text":"1","badges":[` + strings.Join(manyRowBadges, ",") + `]}]}`, "at most 6 badges"},
		"dag-onTap":            {`{"kind":"dag","onTap":["x"],"rows":[]}`, "onChange, not onTap"},
		"dag-children":         {`{"kind":"dag","rows":[],"children":[{"kind":"text","text":"x"}]}`, "holds no children"},
		"rows-on-text":         {`{"kind":"text","text":"x","rows":[{"id":"a","text":"1"}]}`, "only a dag has rows"},
		"dag-bad-maxHeight":    {`{"kind":"dag","maxHeight":-5,"rows":[]}`, "maxHeight is from 0 to 4000"},
		"placeholder-too-long": {`{"kind":"button","icon":"plus","placeholder":"` + strings.Repeat("a", maxPlaceholder+1) + `"}`, "placeholder is over 256 characters"},
	}
	for name, c := range cases {
		_, err := Read([]byte(c.tree))
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want substring %q", name, err, c.want)
		}
	}
}

func TestPlaceholderPreservedAcrossKinds(t *testing.T) {
	tr, err := Read([]byte(`{"kind":"column","children":[
		{"kind":"button","key":"b","icon":"refresh-cw","placeholder":"Refresh","onTap":["r"]},
		{"kind":"menu","key":"m","icon":"ellipsis","placeholder":"Git actions","children":[{"kind":"menu_item","text":"Fetch","onTap":["f"]}]},
		{"kind":"tree","key":"t","children":[{"kind":"tree_item","key":"ti","text":"a.go","placeholder":"Commit a.go","check":"on","onCheck":["p"]}]},
		{"kind":"checkbox","key":"cb","text":"Amend","placeholder":"Amend last commit","check":"off","onChange":["a"]},
		{"kind":"icon","key":"ic","icon":"info","placeholder":"Details"},
		{"kind":"pill","key":"pl","text":"ok","placeholder":"All checks passed"},
		{"kind":"chip","key":"ch","text":"main","placeholder":"Current branch"},
		{"kind":"input","key":"in","placeholder":"Type here","onChange":["c"]},
		{"kind":"textarea","key":"ta","placeholder":"Commit message","onChange":["c"]}
	]}`))
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	want := map[string]string{
		"/b":    "Refresh",
		"/m":    "Git actions",
		"/t/ti": "Commit a.go",
		"/cb":   "Amend last commit",
		"/ic":   "Details",
		"/pl":   "All checks passed",
		"/ch":   "Current branch",
		"/in":   "Type here",
		"/ta":   "Commit message",
	}
	got := map[string]string{}
	var walk func(n *Node)
	walk = func(n *Node) {
		if n.Placeholder != "" {
			got[n.ID] = n.Placeholder
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(tr.Root)
	for id, wantPH := range want {
		if got[id] != wantPH {
			t.Errorf("node %s placeholder = %q, want %q", id, got[id], wantPH)
		}
	}
}

func TestAChartIsRead(t *testing.T) {
	tr, err := Read([]byte(`{"kind":"column","children":[{"kind":"chart","key":"c","chart":"line","text":"Builds",
		"labels":["mon","tue","wed"],"series":[{"name":"passed","tone":"success","values":[3,4.5,-1]},{"values":[0,1,2]}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	c := tr.Root.Children[0]
	if c.Chart != "line" || len(c.Series) != 2 || c.Series[0].Tone != "success" ||
		fmt.Sprint(c.Series[0].Values) != "[3 4.5 -1]" || strings.Join(c.Labels, ",") != "mon,tue,wed" || c.Text != "Builds" {
		t.Fatalf("the chart is %+v", c)
	}
	next := cloneNode(tr.Root)
	next.Children[0].Series[0].Values = []float64{3, 4.5, 7}
	got, err := Apply(tr.Root, Diff(tr.Root, next))
	if err != nil || !got.Equal(next) {
		t.Fatalf("the patch came to %+v, %v", got, err)
	}
}

func TestChartsOutOfBoundsAreRefused(t *testing.T) {
	values := func(n int) string {
		v := make([]int, n)
		b, _ := json.Marshal(v)
		return string(b)
	}
	series := func(n, points int) string {
		s := make([]string, n)
		for i := range s {
			s[i] = `{"values":` + values(points) + `}`
		}
		return "[" + strings.Join(s, ",") + "]"
	}
	cases := map[string]struct{ tree, want string }{
		"type":      {`{"kind":"chart","chart":"radar","series":[{"values":[1]}]}`, "not one of"},
		"none":      {`{"kind":"chart","chart":"line","series":[]}`, "1 to 4 series"},
		"many":      {`{"kind":"chart","chart":"line","series":` + series(5, 2) + `}`, "1 to 4 series"},
		"long":      {`{"kind":"chart","chart":"line","series":` + series(1, 201) + `}`, "1 to 200 values"},
		"all":       {`{"kind":"chart","chart":"line","series":` + series(4, 160) + `}`, "at most 600 points"},
		"null":      {`{"kind":"chart","chart":"line","series":[{"values":[1,null]}]}`, "finite"},
		"huge":      {`{"kind":"chart","chart":"line","series":[{"values":[1e300]}]}`, "finite"},
		"string":    {`{"kind":"chart","chart":"line","series":[{"values":["1"]}]}`, "does not read"},
		"colour":    {`{"kind":"chart","chart":"bar","series":[{"tone":"#ff0000","values":[1]}]}`, "colour token"},
		"color":     {`{"kind":"chart","chart":"bar","series":[{"color":"#ff0000","values":[1]}]}`, "unknown field"},
		"labels":    {`{"kind":"chart","chart":"bar","labels":["a"],"series":[{"values":[1,2]}]}`, "2 values for 1 labels"},
		"label":     {`{"kind":"chart","chart":"bar","labels":["` + strings.Repeat("x", 33) + `"],"series":[{"values":[1]}]}`, "a label is at most"},
		"spoof":     {`{"kind":"chart","chart":"bar","labels":["a\u202eb"],"series":[{"values":[1]}]}`, "reordering"},
		"name":      {`{"kind":"chart","chart":"bar","series":[{"name":"a\u0007","values":[1]}]}`, "reordering"},
		"elsewhere": {`{"kind":"text","text":"x","series":[{"values":[1]}]}`, "only a chart"},
		"tap":       {`{"kind":"chart","chart":"line","series":[{"values":[1]}],"onTap":["x"]}`, "sends no messages"},
		"children":  {`{"kind":"chart","chart":"line","series":[{"values":[1]}],"children":[{"kind":"text"}]}`, "holds no children"},
	}
	for name, c := range cases {
		if _, err := Read([]byte(c.tree)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

func TestDonutAndPieCharts(t *testing.T) {
	for _, typ := range []string{"donut", "pie"} {
		tr, err := Read([]byte(`{"kind":"chart","chart":"` + typ + `","text":"Breakdown","labels":["Docs","Code","Empty"],"series":[{"name":"count","tone":"accent","values":[12,5,0]}]}`))
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		if tr.Root.Chart != typ || len(tr.Root.Series) != 1 || len(tr.Root.Labels) != 3 {
			t.Fatalf("%s: unexpected node %+v", typ, tr.Root)
		}
		next := cloneNode(tr.Root)
		next.Series[0].Values = []float64{10, 8, 2}
		got, err := Apply(tr.Root, Diff(tr.Root, next))
		if err != nil || !got.Equal(next) {
			t.Fatalf("%s patch: %v %v", typ, got, err)
		}
	}
	var vals17, labels17 []string
	for i := range 17 {
		vals17 = append(vals17, "1")
		labels17 = append(labels17, fmt.Sprintf(`"s%d"`, i))
	}
	bad := map[string]struct{ tree, want string }{
		"two-series": {`{"kind":"chart","chart":"donut","labels":["a"],"series":[{"values":[1]},{"values":[2]}]}`, "1 series"},
		"no-labels":  {`{"kind":"chart","chart":"pie","series":[{"values":[1,2]}]}`, "requires 2 labels"},
		"mismatch":   {`{"kind":"chart","chart":"donut","labels":["a"],"series":[{"values":[1,2]}]}`, "requires 2 labels"},
		"negative":   {`{"kind":"chart","chart":"pie","labels":["a","b"],"series":[{"values":[1,-0.5]}]}`, "non-negative"},
		"too-many":   {`{"kind":"chart","chart":"donut","labels":[` + strings.Join(labels17, ",") + `],"series":[{"values":[` + strings.Join(vals17, ",") + `]}]}`, "1 to 16 values"},
	}
	for name, c := range bad {
		if _, err := Read([]byte(c.tree)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
}

func TestCamelCaseJSONWireFormat(t *testing.T) {
	node := &Node{
		ID:          "/n",
		Kind:        KindScroll,
		DragGroup:   "grp",
		DropAccept:  []string{"a"},
		MaxHeight:   300,
		ContextMenu: "ctx",
		ContextArg:  "arg",
		GraphNodes:  []*GraphNode{{ID: "g1", Label: "G1"}},
		GraphEdges:  []*GraphEdge{{From: "g1", To: "g1"}},
	}
	b, err := json.Marshal(node)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(b)
	for _, wantKey := range []string{`"dragGroup"`, `"dropAccept"`, `"maxHeight"`, `"contextMenu"`, `"contextArg"`, `"graphNodes"`, `"graphEdges"`} {
		if !strings.Contains(raw, wantKey) {
			t.Errorf("marshaled Node missing camelCase key %s: %s", wantKey, raw)
		}
	}
}
