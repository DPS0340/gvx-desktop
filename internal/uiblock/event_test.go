package uiblock

import (
	"fmt"
	"strings"
	"testing"
)

func TestALargeFormsValuesAreTaken(t *testing.T) {
	var fields []string
	in := map[string]string{}
	for i := range 200 {
		key := fmt.Sprintf("f%d", i)
		fields = append(fields, `{"kind":"input","key":"`+key+`","value":""}`)
		in["/"+key] = "some text"
	}
	tr, err := Read([]byte(`{"kind":"column","children":[` + strings.Join(fields, ",") + `]}`))
	if err != nil {
		t.Fatal(err)
	}
	vals, err := tr.Values(in)
	if err != nil || len(vals) != 200 {
		t.Fatalf("200 fields: %d taken, %v", len(vals), err)
	}
	for id := range in {
		in[id] = strings.Repeat("x", 200)
	}
	if _, err := tr.Values(in); err == nil || !strings.Contains(err.Error(), "32 KiB") {
		t.Fatalf("40 KiB of values: %v", err)
	}
	many := map[string]string{}
	for i := range maxNodes + 1 {
		many[fmt.Sprintf("/x%d", i)] = ""
	}
	if _, err := tr.Values(many); err == nil {
		t.Fatal("more values than a tree holds nodes were taken")
	}
}

func TestEventsCarryTheirMessageAndValues(t *testing.T) {
	tr, err := Read([]byte(`{"kind":"column","children":[
		{"kind":"input","key":"name","value":"","onChange":["set_name"]},
		{"kind":"input","key":"pass","variant":"password","value":"","onSubmit":["submit_pass"]},
		{"kind":"toggle","key":"gift","checked":false,"onChange":["gift"]},
		{"kind":"select","key":"size","value":"M","options":["S","M","L"],"onChange":["set_size"]},
		{"kind":"tabs","key":"tab","value":"A","options":["A","B"],"onChange":["set_tab"]},
		{"kind":"text","key":"note","text":"x"},
		{"kind":"button","key":"send","text":"Send","onTap":["submit","form-1"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	m, err := tr.Message(Event{Node: "/send", Kind: EventTap})
	if err != nil || m.Name != "submit" || string(m.Given) != `["submit","form-1"]` || len(m.Args) != 1 {
		t.Fatalf("%+v %v", m, err)
	}
	m, err = tr.Message(Event{Node: "/gift", Kind: EventChange, Value: "true"})
	if err != nil || string(m.Args[len(m.Args)-1]) != "true" {
		t.Fatalf("%+v %v", m, err)
	}
	// Submit on input falls back to onChange when onSubmit is absent.
	m, err = tr.Message(Event{Node: "/name", Kind: EventSubmit, Value: "Ada"})
	if err != nil || m.Name != "set_name" || string(m.Args[len(m.Args)-1]) != `"Ada"` {
		t.Fatalf("submit fallback: %+v %v", m, err)
	}
	// Submit on input uses onSubmit when present.
	m, err = tr.Message(Event{Node: "/pass", Kind: EventSubmit, Value: "s3cr3t"})
	if err != nil || m.Name != "submit_pass" || string(m.Args[len(m.Args)-1]) != `"s3cr3t"` {
		t.Fatalf("submit direct: %+v %v", m, err)
	}

	vals, err := tr.Values(map[string]string{
		"/name": "Ada",
		"/pass": "s3cr3t",
		"/gift": "true",
		"/size": "L",
		"/tab":  "B",
	})
	if err != nil || vals["/name"] != "Ada" || vals["/pass"] != "s3cr3t" || len(vals) != 5 {
		t.Fatalf("%v %v", vals, err)
	}
	// Empty map yields nil, nil.
	if v, err := tr.Values(nil); err != nil || v != nil {
		t.Fatalf("empty Values = %v, %v", v, err)
	}
	for _, bad := range []map[string]string{{"/note": "x"}, {"/name": strings.Repeat("x", MaxEventValue+1)}} {
		if _, err := tr.Values(bad); err == nil {
			t.Errorf("%v was taken", bad)
		}
	}
	// A field the tree no longer has (1-render lag) is omitted; the rest stands.
	if vals, err := tr.Values(map[string]string{"/name": "Ada", "/gone": "x"}); err != nil || len(vals) != 1 || vals["/name"] != "Ada" {
		t.Fatalf("with a field gone: %v %v", vals, err)
	}
	if _, err := tr.Message(Event{Node: "/note", Kind: EventTap}); err == nil {
		t.Error("a tap on text that sends nothing was taken")
	}
	if _, err := tr.Message(Event{Node: "/send", Kind: "unknown"}); err == nil {
		t.Error("unknown event kind was taken")
	}
	if _, err := tr.Message(Event{Node: "/name", Kind: EventChange, Value: strings.Repeat("x", MaxEventValue+1)}); err == nil {
		t.Error("oversize event value was taken")
	}
	var nilTree *Tree
	if _, err := nilTree.Message(Event{Node: "/send", Kind: EventTap}); err == nil {
		t.Error("an event of a block with no tree was taken")
	}
	if v, err := nilTree.Values(map[string]string{"/name": "Ada"}); err != nil || len(v) != 0 {
		t.Fatalf("nilTree.Values = %v, %v", v, err)
	}
}

func TestEventSubmitFallbackRestrictedToInputAndTextarea(t *testing.T) {
	tr, err := Read([]byte(`{"kind":"column","children":[
		{"kind":"input","key":"inp","value":"","onChange":["on_inp"]},
		{"kind":"textarea","key":"ta","value":"","onChange":["on_ta"]},
		{"kind":"toggle","key":"tog","checked":false,"onChange":["on_tog"]},
		{"kind":"checkbox","key":"chk","check":"off","onChange":["on_chk"]},
		{"kind":"dialog","key":"dlg","text":"Confirm","onChange":["on_dlg"]},
		{"kind":"dag","key":"dag","onChange":["on_dag"],"rows":[{"id":"r1","text":"Row 1"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		node string
		want string
		val  string
	}{
		{node: "/inp", want: "on_inp", val: "hello"},
		{node: "/ta", want: "on_ta", val: "multi\nline"},
	} {
		m, err := tr.Message(Event{Node: tc.node, Kind: EventSubmit, Value: tc.val})
		if err != nil {
			t.Fatalf("submit fallback on %s: unexpected error: %v", tc.node, err)
		}
		if m.Name != tc.want {
			t.Fatalf("submit fallback on %s: got message %q, want %q", tc.node, m.Name, tc.want)
		}
	}

	for _, tc := range []struct {
		node string
		val  string
	}{
		{node: "/tog", val: "not-a-bool"},
		{node: "/chk", val: "invalid-check"},
		{node: "/dlg", val: "arbitrary-not-dismiss"},
		{node: "/dag", val: "nonexistent-row"},
	} {
		if m, err := tr.Message(Event{Node: tc.node, Kind: EventSubmit, Value: tc.val}); err == nil {
			t.Fatalf("submit fallback on %s unexpectedly succeeded: %+v", tc.node, m)
		}
	}
}

func TestEventDropRejectsInvalidUTF8(t *testing.T) {
	tr, err := Read([]byte(`{"kind":"column","children":[
		{"kind":"column","key":"drop","onDrop":["dropped"]}]}`))
	if err != nil {
		t.Fatal(err)
	}

	m, err := tr.Message(Event{Node: "/drop", Kind: EventDrop, Value: "valid-item"})
	if err != nil || m.Name != "dropped" {
		t.Fatalf("valid drop failed: %+v %v", m, err)
	}

	for _, bad := range []string{"", "bad\xff", "bad\u202e"} {
		if m, err := tr.Message(Event{Node: "/drop", Kind: EventDrop, Value: bad}); err == nil {
			t.Fatalf("drop with %q unexpectedly succeeded: %+v", bad, m)
		}
	}
}
