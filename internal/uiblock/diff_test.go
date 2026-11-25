package uiblock

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"testing"
)

// randomTree is a tree of keyed and unkeyed children, for diffing.
func randomTree(r *rand.Rand, depth int) map[string]any {
	kinds := []string{"text", "pill", "chip", "button"}
	n := map[string]any{"kind": kinds[r.IntN(len(kinds))], "text": fmt.Sprint(r.IntN(3))}
	if depth > 0 && r.IntN(3) > 0 {
		n = map[string]any{"kind": []string{"column", "row", "list"}[r.IntN(3)], "gap": float64(r.IntN(2))}
		var kids []any
		keys := r.Perm(8)[:r.IntN(6)]
		for _, k := range keys {
			c := randomTree(r, depth-1)
			if r.IntN(4) > 0 {
				c["key"] = fmt.Sprint(k)
			}
			kids = append(kids, c)
		}
		n["children"] = kids
	}
	return n
}

// TestDiffAppliesToTheNext diffs random trees, keyed and moved about, and
// applies the ops: it must come to the next tree without mutating the original.
func TestDiffAppliesToTheNext(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	read := func(v map[string]any) *Node {
		b, _ := json.Marshal(v)
		tr, err := Read(b)
		if err != nil {
			t.Fatal(err)
		}
		return tr.Root
	}
	for i := range 500 {
		a, b := read(randomTree(r, 3)), read(randomTree(r, 3))
		aSnap := cloneNode(a)
		ops := Diff(a, b)
		got, err := Apply(a, ops)
		if err != nil {
			t.Fatalf("%d: %v", i, err)
		}
		if !a.Equal(aSnap) {
			t.Fatalf("%d: Apply mutated root", i)
		}
		if !got.Equal(b) {
			t.Fatalf("%d: applying %d ops did not come to the next tree", i, len(ops))
		}
		if len(Diff(b, b)) != 0 {
			t.Fatalf("%d: a tree differs from itself", i)
		}
	}
}

func TestMovedKeyedChildIsMoved(t *testing.T) {
	read := func(s string) *Node {
		tr, err := Read([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return tr.Root
	}
	a := read(`{"kind":"list","children":[{"kind":"text","key":"a","text":"A"},{"kind":"text","key":"b","text":"B"},{"kind":"text","key":"c","text":"C"}]}`)
	b := read(`{"kind":"list","children":[{"kind":"text","key":"c","text":"C"},{"kind":"text","key":"a","text":"A"},{"kind":"text","key":"b","text":"B2"}]}`)
	ops := Diff(a, b)
	var kinds []string
	for _, op := range ops {
		kinds = append(kinds, op.Op)
	}
	if fmt.Sprint(kinds) != "[move update]" {
		t.Fatalf("ops %v", kinds)
	}
}

func TestApplyRefusesOpsOfAnotherTreeAndNilGuards(t *testing.T) {
	root := &Node{Kind: "column", Children: []*Node{{ID: "/a", Kind: "text"}}}
	for _, op := range []*Op{
		nil,
		{Op: OpRemove, ID: "/missing"},
		{Op: OpInsert, Parent: "/missing", Node: &Node{}},
		{Op: OpInsert, Parent: "", Index: 5, Node: &Node{}},
		{Op: OpInsert, Parent: "", Index: 0, Node: nil},
		{Op: OpReplace, ID: "/a", Node: nil},
		{Op: OpUpdate, ID: "/a", Node: nil},
		{Op: OpMove, ID: "/a", Parent: "/other", Index: 0},
		{Op: "explode", ID: "/a"},
	} {
		if _, err := Apply(root, []*Op{op}); err == nil {
			t.Errorf("%v was applied", op)
		}
	}
	if _, err := Apply(nil, nil); err == nil {
		t.Error("Apply(nil, nil) succeeded")
	}
}

type uiCase struct {
	name         string
	before, next string
	ops          []*Op
}

var uiCases = []uiCase{
	{name: "same", before: `{"kind":"column","children":[{"kind":"text","text":"a"}]}`,
		next: `{"kind":"column","children":[{"kind":"text","text":"a"}]}`},
	{name: "keyed-move", before: `{"kind":"list","key":"runs","children":[
		{"kind":"row","key":"r1","children":[{"kind":"pill","text":"ok","tone":"success"},{"kind":"text","text":"build 1"}]},
		{"kind":"row","key":"r2","children":[{"kind":"pill","text":"failed","tone":"danger"},{"kind":"text","text":"build 2"}]},
		{"kind":"row","key":"r3","children":[{"kind":"pill","text":"running","tone":"accent"},{"kind":"text","text":"build 3"}]}]}`,
		next: `{"kind":"list","key":"runs","children":[
		{"kind":"row","key":"r3","children":[{"kind":"pill","text":"running","tone":"accent"},{"kind":"text","text":"build 3"}]},
		{"kind":"row","key":"r1","children":[{"kind":"pill","text":"ok","tone":"success"},{"kind":"text","text":"build 1"}]},
		{"kind":"row","key":"r2","children":[{"kind":"pill","text":"failed","tone":"danger"},{"kind":"text","text":"build 2"}]}]}`},
	{name: "insert-remove", before: `{"kind":"column","children":[
		{"kind":"text","key":"a","text":"A"},{"kind":"text","key":"b","text":"B"},{"kind":"text","key":"c","text":"C"}]}`,
		next: `{"kind":"column","children":[
		{"kind":"text","key":"z","text":"Z"},{"kind":"text","key":"b","text":"B"},{"kind":"divider","key":"d"},{"kind":"text","key":"e","text":"E"}]}`},
	{name: "nested-update", before: `{"kind":"column","gap":4,"children":[
		{"kind":"row","key":"head","children":[{"kind":"icon","icon":"git-branch"},{"kind":"text","key":"t","text":"main","weight":"medium"}]},
		{"kind":"progress","key":"p","progress":0.25},
		{"kind":"toggle","key":"watch","text":"Watch","checked":false,"onChange":["watch"]}]}`,
		next: `{"kind":"column","gap":6,"children":[
		{"kind":"row","key":"head","children":[{"kind":"icon","icon":"git-branch","tone":"accent"},{"kind":"text","key":"t","text":"release","weight":"semibold"}]},
		{"kind":"progress","key":"p","progress":0.75},
		{"kind":"toggle","key":"watch","text":"Watch","checked":true,"onChange":["watch"]}]}`},
	{name: "root-replace", before: `{"kind":"column","children":[{"kind":"text","text":"loading"}]}`,
		next: `{"kind":"markdown","text":"# Done\n\nAll **green**."}`},
	{name: "child-replace", before: `{"kind":"column","children":[{"kind":"text","key":"x","text":"x"},{"kind":"text","key":"y","text":"y"}]}`,
		next: `{"kind":"column","children":[{"kind":"text","key":"x","text":"x"},{"kind":"button","key":"y","text":"y","onTap":["y"]}]}`},
	{name: "unkeyed-shift", before: `{"kind":"column","children":[{"kind":"text","text":"one"},{"kind":"text","text":"two"}]}`,
		next: `{"kind":"column","children":[{"kind":"chip","text":"zero"},{"kind":"text","text":"one"},{"kind":"text","text":"two"}]}`},
	{name: "mixed", before: `{"kind":"column","children":[
		{"kind":"list","key":"l","children":[
			{"kind":"text","key":"a","text":"A"},{"kind":"text","key":"b","text":"B"},{"kind":"text","key":"c","text":"C"},{"kind":"text","key":"d","text":"D"}]},
		{"kind":"select","key":"s","value":"x","options":["x","y"],"onChange":["pick"]},
		{"kind":"input","key":"i","value":"","placeholder":"Name","onSubmit":["name"]}]}`,
		next: `{"kind":"column","children":[
		{"kind":"input","key":"i","value":"Ada","placeholder":"Name","onSubmit":["name"]},
		{"kind":"list","key":"l","children":[
			{"kind":"text","key":"d","text":"D!"},{"kind":"text","key":"new","text":"N"},{"kind":"text","key":"b","text":"B"},{"kind":"text","key":"a","text":"A","tone":"muted"}]},
		{"kind":"code","key":"c","text":"go test ./...","language":"sh"}]}`},
	{name: "stack-and-row", before: `{"kind":"stack","children":[{"kind":"row","key":"r","children":[{"kind":"text","text":"a","grow":1},{"kind":"button","key":"go","text":"Go","onTap":["go"]}]}]}`,
		next: `{"kind":"stack","children":[{"kind":"row","key":"r","gap":12,"children":[{"kind":"button","key":"go","text":"Go","icon":"play","onTap":["go"],"disabled":true},{"kind":"text","text":"a","grow":1}]}]}`},
	{name: "chart-points", before: `{"kind":"column","children":[{"kind":"chart","key":"lat","chart":"line","text":"Latency",
		"labels":["09","10","11"],"series":[{"name":"p50","values":[3,4.5,5]},{"name":"p95","tone":"warning","values":[9,12,11]}]}]}`,
		next: `{"kind":"column","children":[{"kind":"chart","key":"lat","chart":"line","text":"Latency",
		"labels":["10","11","12","13"],"series":[{"name":"p50","values":[4.5,5,3,6]},{"name":"p95","tone":"danger","values":[12,11,14,-2]}]}]}`},
	{name: "chart-kind-and-series", before: `{"kind":"column","children":[{"kind":"chart","key":"c","chart":"line",
		"series":[{"name":"a","values":[1,2,3]},{"name":"b","values":[3,2,1]}]}]}`,
		next: `{"kind":"column","children":[{"kind":"chart","key":"c","chart":"bar","text":"Builds","size":"small",
		"labels":["mon","tue","wed"],"series":[{"name":"passed","tone":"success","values":[5,0,7]}]}]}`},
	{name: "chart-insert-remove", before: `{"kind":"column","children":[
		{"kind":"text","key":"t","text":"CPU"},{"kind":"chart","key":"spark","chart":"sparkline","series":[{"values":[0.1,0.4,0.2,0.9]}]}]}`,
		next: `{"kind":"column","children":[
		{"kind":"chart","key":"bars","chart":"bar","labels":["a"],"series":[{"name":"one","values":[42]}]},
		{"kind":"text","key":"t","text":"CPU","tone":"muted"}]}`},
	{name: "drag-drop", before: `{"kind":"row","children":[
		{"kind":"column","key":"todo","onDrop":["move","todo"],"children":[
			{"kind":"column","key":"c1","drag":"c1","onDrop":["before","todo","c1"],"children":[{"kind":"text","text":"Task 1"}]}]},
		{"kind":"column","key":"done","onDrop":["move","done"],"children":[]}]}`,
		next: `{"kind":"row","children":[
		{"kind":"column","key":"todo","onDrop":["move","todo"],"children":[]},
		{"kind":"column","key":"done","onDrop":["move","done"],"children":[
			{"kind":"column","key":"c1","drag":"c1","onDrop":["before","done","c1"],"children":[{"kind":"text","text":"Task 1"}]}]}]}`},
	{name: "rich-ui", before: `{"kind":"column","children":[
		{"kind":"tabs","key":"t","value":"Board","options":["Board","Table"],"onChange":["tab"]},
		{"kind":"scroll","key":"s","direction":"vertical","maxHeight":320,"onDrop":["move","todo"],"dropAccept":["card"],"children":[
			{"kind":"card","key":"c1","drag":"c1","dragGroup":"card","children":[
				{"kind":"text","text":"Alpha"},
				{"kind":"textarea","key":"n","value":"note 1","placeholder":"Notes","onChange":["note"]}]}]},
		{"kind":"table","key":"tbl","labels":["Name","State"],"children":[
			{"kind":"row","key":"r1","children":[{"kind":"text","text":"Alpha"},{"kind":"pill","text":"Todo"}]}]}]}`,
		next: `{"kind":"column","children":[
		{"kind":"tabs","key":"t","value":"Table","options":["Board","Table"],"onChange":["tab"]},
		{"kind":"scroll","key":"s","direction":"horizontal","maxHeight":420,"onDrop":["move","todo"],"dropAccept":["card","lane"],"children":[
			{"kind":"card","key":"c1","drag":"c1","dragGroup":"card","children":[
				{"kind":"text","text":"Alpha updated"},
				{"kind":"textarea","key":"n","value":"note 2","placeholder":"Notes","onChange":["note"]}]}]},
		{"kind":"table","key":"tbl","labels":["Name","State"],"children":[
			{"kind":"row","key":"r1","children":[{"kind":"text","text":"Alpha updated"},{"kind":"pill","text":"Done","tone":"success"}]}]}]}`},
	{name: "rich-ui-v2", before: `{"kind":"split","fill":true,"direction":"horizontal","ratio":0.35,"children":[
		{"kind":"column","key":"nav","children":[
			{"kind":"breadcrumb","key":"bc","options":["Vault","Notes"],"onChange":["crumb"]},
			{"kind":"tree","key":"t","children":[
				{"kind":"tree_item","key":"d1","text":"Docs","value":"1","icon":"folder","checked":true,"onTap":["sel","d1"],"children":[
					{"kind":"tree_item","key":"n1","text":"Intro","icon":"file-text","onTap":["sel","n1"]}]}]}]},
		{"kind":"column","key":"main","children":[
			{"kind":"grid","key":"g","columns":2,"gap":8,"children":[
				{"kind":"card","key":"c1","variant":"outline","children":[{"kind":"button","key":"b1","icon":"plus","variant":"ghost","size":"small","onTap":["add"]}]},
				{"kind":"collapsible","key":"col","text":"Tags","checked":true,"children":[{"kind":"wrap","gap":4,"children":[{"kind":"chip","text":"a"}]}]}]},
			{"kind":"markdown","key":"md","text":"[[Intro]]","onLink":["link"]},
			{"kind":"chart","key":"ch","chart":"donut","labels":["A","B"],"series":[{"values":[3,7]}]},
			{"kind":"graph","key":"gr","value":"a","maxHeight":280,"onChange":["pick"],"graphNodes":[{"id":"a","label":"A","weight":1},{"id":"b","label":"B","weight":2}],"graphEdges":[{"from":"a","to":"b","dashed":true}]}]}]}`,
		next: `{"kind":"split","fill":true,"direction":"vertical","ratio":0.5,"children":[
		{"kind":"column","key":"nav","children":[
			{"kind":"breadcrumb","key":"bc","options":["Vault","Notes","Intro"],"onChange":["crumb"]},
			{"kind":"tree","key":"t","children":[
				{"kind":"tree_item","key":"d1","text":"Docs","value":"2","icon":"folder","checked":true,"onTap":["sel","d1"],"children":[
					{"kind":"tree_item","key":"n1","text":"Intro","icon":"file-text","checked":true,"onTap":["sel","n1"]}]}]}]},
		{"kind":"column","key":"main","children":[
			{"kind":"grid","key":"g","columns":3,"gap":8,"children":[
				{"kind":"card","key":"c1","variant":"primary","children":[{"kind":"button","key":"b1","icon":"plus","variant":"primary","size":"large","onTap":["add"]}]},
				{"kind":"collapsible","key":"col","text":"Tags","checked":false,"children":[{"kind":"wrap","gap":4,"children":[{"kind":"chip","text":"a"},{"kind":"chip","text":"b"}]}]}]},
			{"kind":"markdown","key":"md","text":"[[Intro]] updated","onLink":["link"]},
			{"kind":"chart","key":"ch","chart":"pie","labels":["A","B"],"series":[{"values":[5,5]}]},
			{"kind":"graph","key":"gr","value":"b","maxHeight":360,"onChange":["pick"],"graphNodes":[{"id":"a","label":"A","weight":1},{"id":"b","label":"B","tone":"accent","weight":3}],"graphEdges":[{"from":"a","to":"b","label":"rel","dashed":false}]}]}]}`},
	{name: "rich-ui-v3", before: `{"kind":"column","fill":true,"children":[
		{"kind":"row","key":"bar","variant":"toolbar","children":[
			{"kind":"menu","key":"br","text":"main","icon":"git-branch","variant":"ghost","placeholder":"Branch","children":[
				{"kind":"menu_item","key":"m1","text":"main","checked":true,"onTap":["checkout","main"]},
				{"kind":"divider"},
				{"kind":"menu","key":"more","text":"More","children":[
					{"kind":"menu_item","key":"new","text":"Create branch","icon":"plus","onTap":["askBranch"]}]}]},
			{"kind":"button","key":"pull","text":"Pull","placeholder":"Pull from origin/main","badges":[{"text":"2","tone":"accent","variant":"soft"}],"onTap":["pull"]}]},
		{"kind":"split","key":"body","direction":"auto","ratio":0.45,"children":[
			{"kind":"column","key":"left","children":[
				{"kind":"tree","key":"files","children":[
					{"kind":"tree_item","key":"f1","text":"main.go","icon":"file-diff","placeholder":"Commit main.go","check":"on","onCheck":["pick","main.go"],"badges":[{"text":"+12","tone":"success","variant":"mono"},{"text":"-3","tone":"danger","variant":"mono"}],"contextMenu":"file-menu","contextArg":"main.go","onTap":["open","main.go"]}]},
				{"kind":"checkbox","key":"amend","text":"Amend","check":"off","onChange":["amend"]}]},
			{"kind":"dag","key":"hist","value":"c1","contextMenu":"commit-menu","maxHeight":400,"onChange":["select"],"rows":[
				{"id":"c1","parents":["c0"],"text":"Initial commit","detail":"Ada","meta":"2m","strong":true,"badges":[{"text":"main","tone":"accent","variant":"soft"}]},
				{"id":"c0","text":"Root"}]}]},
		{"kind":"menu","key":"file-menu","variant":"context","children":[
			{"kind":"menu_item","key":"discard","text":"Discard","tone":"danger","onTap":["discard"]}]},
		{"kind":"menu","key":"commit-menu","variant":"context","children":[
			{"kind":"menu_item","key":"copy","text":"Copy hash","onTap":["copy"]}]}]}`,
		next: `{"kind":"column","fill":true,"children":[
		{"kind":"row","key":"bar","variant":"toolbar","children":[
			{"kind":"menu","key":"br","text":"feature","icon":"git-branch","variant":"outline","placeholder":"Branch","children":[
				{"kind":"menu_item","key":"m1","text":"main","checked":false,"onTap":["checkout","main"]},
				{"kind":"menu_item","key":"m2","text":"feature","checked":true,"onTap":["checkout","feature"]},
				{"kind":"divider"},
				{"kind":"menu","key":"more","text":"More","children":[
					{"kind":"menu_item","key":"new","text":"Create branch","icon":"plus","onTap":["askBranch"]}]}]},
			{"kind":"button","key":"pull","text":"Pull","placeholder":"Pull","badges":[{"text":"0","tone":"muted","variant":"soft"}],"onTap":["pull"]}]},
		{"kind":"split","key":"body","direction":"auto","ratio":0.5,"children":[
			{"kind":"column","key":"left","children":[
				{"kind":"tree","key":"files","children":[
					{"kind":"tree_item","key":"f1","text":"main.go","icon":"file-diff","placeholder":"Commit src/main.go","check":"mixed","onCheck":["pick","main.go"],"badges":[{"text":"+15","tone":"success","variant":"mono"}],"contextMenu":"file-menu","contextArg":"src/main.go","onTap":["open","src/main.go"]}]},
				{"kind":"checkbox","key":"amend","text":"Amend","check":"on","onChange":["amend"]}]},
			{"kind":"dag","key":"hist","value":"c2","contextMenu":"commit-menu","maxHeight":480,"onChange":["select"],"rows":[
				{"id":"c2","parents":["c1","c0"],"text":"Merge feature","detail":"Grace","meta":"now","strong":true,"icon":"git-merge","badges":[{"text":"HEAD","tone":"success","variant":"solid"}]},
				{"id":"c1","parents":["c0"],"text":"Initial commit","detail":"Ada","meta":"3m"},
				{"id":"c0","text":"Root"}]}]},
		{"kind":"dialog","key":"confirm","text":"Discard changes?","variant":"danger","onChange":["closeDialog"],"children":[
			{"kind":"text","text":"This cannot be undone."},
			{"kind":"button","key":"ok","text":"Discard","variant":"danger","onTap":["confirmDiscard"]}]},
		{"kind":"menu","key":"file-menu","variant":"context","children":[
			{"kind":"menu_item","key":"discard","text":"Discard","tone":"danger","onTap":["discard"]}]},
		{"kind":"menu","key":"commit-menu","variant":"context","children":[
			{"kind":"menu_item","key":"copy","text":"Copy hash","onTap":["copy"]}]}]}`},

	{name: "update-dag-and-badges", before: `{"kind":"column","children":[
		{"kind":"tree","key":"t","children":[
			{"kind":"tree_item","key":"i","text":"a.go","check":"off","onCheck":["p"],"badges":[{"text":"+1","tone":"success","variant":"mono"}]}]},
		{"kind":"dag","key":"d","rows":[{"id":"r1","text":"one"}]}]}`,
		ops: []*Op{
			{Op: OpUpdate, ID: "/t/i", Node: &Node{ID: "/t/i", Kind: "tree_item", Text: "a.go", Check: "on", Checks: true,
				Badges: []*Badge{{Text: "+4", Tone: "success", Variant: "mono"}, {Text: "-2", Tone: "danger", Variant: "mono"}}}},
			{Op: OpUpdate, ID: "/d", Node: &Node{ID: "/d", Kind: "dag", Value: "r2",
				Rows: []*Row{{ID: "r2", Parents: []string{"r1"}, Text: "two", Strong: true, Badges: []*Badge{{Text: "main", Variant: "soft"}}}}}},
		}},
	{name: "update-keeps-children", before: `{"kind":"column","children":[{"kind":"row","key":"r","children":[{"kind":"text","text":"kept"}]}]}`,
		ops: []*Op{{Op: OpUpdate, ID: "/r", Node: &Node{ID: "/r", Kind: "row", Gap: 3,
			Children: []*Node{{ID: "/r/#9", Kind: "text", Text: "ignored"}}}}}},
	{name: "update-chart-series", before: `{"kind":"column","children":[{"kind":"chart","key":"c","chart":"bar","labels":["x","y"],
		"series":[{"name":"a","values":[1,2]},{"name":"b","values":[2,1]}]}]}`,
		ops: []*Op{{Op: OpUpdate, ID: "/c", Node: &Node{ID: "/c", Kind: "chart", Chart: "line",
			Labels: []string{"x"}, Series: []*Series{{Name: "only", Tone: "accent", Values: []float64{0.5}}}}}}},
	{name: "insert-at-end", before: `{"kind":"column","children":[{"kind":"text","key":"a","text":"A"}]}`,
		ops: []*Op{{Op: OpInsert, Parent: "", Index: 1, Node: &Node{ID: "/b", Kind: "text", Text: "B"}}}},
	{name: "replace-root-by-id", before: `{"kind":"text","text":"old"}`,
		ops: []*Op{{Op: OpReplace, ID: "", Node: &Node{Kind: "text", Text: "new"}}}},
	{name: "fail-remove-missing", before: `{"kind":"column","children":[{"kind":"text","key":"a","text":"A"}]}`,
		ops: []*Op{{Op: OpRemove, ID: "/nope"}}},
	{name: "fail-remove-root", before: `{"kind":"column","children":[{"kind":"text","key":"a","text":"A"}]}`,
		ops: []*Op{{Op: OpRemove, ID: ""}}},
	{name: "fail-update-missing", before: `{"kind":"column","children":[{"kind":"text","key":"a","text":"A"}]}`,
		ops: []*Op{{Op: OpUpdate, ID: "/b", Node: &Node{ID: "/b", Kind: "text"}}}},
	{name: "fail-replace-missing", before: `{"kind":"column","children":[{"kind":"text","key":"a","text":"A"}]}`,
		ops: []*Op{{Op: OpReplace, ID: "/b", Node: &Node{ID: "/b", Kind: "text"}}}},
	{name: "fail-insert-past-end", before: `{"kind":"column","children":[{"kind":"text","key":"a","text":"A"}]}`,
		ops: []*Op{{Op: OpInsert, Parent: "", Index: 2, Node: &Node{ID: "/b", Kind: "text"}}}},
	{name: "fail-insert-no-parent", before: `{"kind":"column","children":[{"kind":"text","key":"a","text":"A"}]}`,
		ops: []*Op{{Op: OpInsert, Parent: "/x", Index: 0, Node: &Node{ID: "/x/b", Kind: "text"}}}},
	{name: "fail-insert-negative", before: `{"kind":"column","children":[{"kind":"text","key":"a","text":"A"}]}`,
		ops: []*Op{{Op: OpInsert, Parent: "", Index: -1, Node: &Node{ID: "/b", Kind: "text"}}}},
	{name: "fail-move-other-parent", before: `{"kind":"column","children":[{"kind":"row","key":"r","children":[{"kind":"text","key":"a","text":"A"}]},{"kind":"text","key":"b","text":"B"}]}`,
		ops: []*Op{{Op: OpMove, ID: "/r/a", Parent: "", Index: 0}}},
	{name: "fail-move-past-end", before: `{"kind":"column","children":[{"kind":"text","key":"a","text":"A"},{"kind":"text","key":"b","text":"B"}]}`,
		ops: []*Op{{Op: OpMove, ID: "/a", Parent: "", Index: 2}}},
	{name: "fail-unknown-op", before: `{"kind":"text","text":"a"}`,
		ops: []*Op{{Op: "swap", ID: ""}}},
	{name: "fail-after-a-good-op", before: `{"kind":"column","children":[{"kind":"text","key":"a","text":"A"}]}`,
		ops: []*Op{{Op: OpRemove, ID: "/a"}, {Op: OpRemove, ID: "/a"}}},
}

func readTree(t *testing.T, src string) *Node {
	t.Helper()
	tree, err := Read([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return tree.Root
}

func TestDiffAndApplyCases(t *testing.T) {
	for _, c := range uiCases {
		t.Run(c.name, func(t *testing.T) {
			before := readTree(t, c.before)
			beforeSnap := cloneNode(before)
			ops := c.ops
			var next *Node
			if c.next != "" {
				next = readTree(t, c.next)
				ops = Diff(before, next)
				if (len(ops) == 0) != (c.name == "same") {
					t.Fatalf("%s: %d ops", c.name, len(ops))
				}
			}
			after, err := Apply(before, ops)
			if !before.Equal(beforeSnap) {
				t.Fatalf("%s: Apply mutated before tree", c.name)
			}
			switch {
			case next != nil && (err != nil || !after.Equal(next)):
				t.Fatalf("%s: the diff does not apply: %v", c.name, err)
			case (err != nil) != (len(c.name) > 5 && c.name[:5] == "fail-"):
				t.Fatalf("%s: %v", c.name, err)
			}
		})
	}
}
