package plugins

import (
	"encoding/json"
	"strings"
	"testing"
)

func typeOf(t *testing.T, schema string) *Type {
	t.Helper()
	sum, st, err := readOf([]byte(schema))
	if err != nil {
		t.Fatal(err)
	}
	return &Type{summary: sum, status: st}
}

// A type's sentence is filled from what was asked, then from what was seen;
// a part one of whose holes is empty is left out, an optional stretch too,
// and what it names reads by name.
func TestATypeSaysItsOwnSentence(t *testing.T) {
	machine := typeOf(t, `{"x-hangar-summary": ["{kind}", "{cores} cores", "{memory_gb} GB[ ({floor_gb} guaranteed)]",
		"{class=spot?spot}", "{image|image_id}", "keys {key_pairs}"],
		"x-hangar-status": {"field": "running", "on": "running", "off": "stopped"}}`)
	names := map[string]string{"img-00000000000000001": "base", "kp-00000000000000001": "laptop"}
	for _, c := range []struct{ spec, observed, want string }{
		{`{"kind":"container","cores":12,"memory_gb":40,"floor_gb":12,"class":"guaranteed+spot","image":"debian-13"}`, `{}`,
			"container · 12 cores · 40 GB (12 guaranteed) · debian-13"},
		{`{"kind":"vm","cores":2,"memory_gb":4,"class":"spot","image_id":"img-00000000000000001","key_pairs":["kp-00000000000000001","kp-00000000000000002"]}`, `{}`,
			"vm · 2 cores · 4 GB · spot · base · keys laptop, kp-00000000000000002"},
		{`{"kind":"vm","image_id":"img-0000000000000000f"}`, `{"cores":1}`, "vm · 1 cores · img-0000000000000000f"},
		{`{}`, `{}`, ""},
	} {
		if got := machine.Summarize(json.RawMessage(c.spec), json.RawMessage(c.observed), names); got != c.want {
			t.Errorf("%s\n got  %q\n want %q", c.spec, got, c.want)
		}
	}
	volume := typeOf(t, `{"x-hangar-summary": ["{size_gb} GB", "on {machine}[ at {mount}]", "{backup?backed up}"],
		"x-hangar-status": {"field": "machine", "on": "attached", "off": "parked"}}`)
	names = map[string]string{"m-00000000000000001": "dev"}
	for _, c := range []struct{ spec, want, status string }{
		{`{"size_gb":64,"machine":"m-00000000000000001","mount":"/home","backup":true}`, "64 GB · on dev at /home · backed up", "attached"},
		{`{"size_gb":128,"machine":"m-00000000000000001","backup":false}`, "128 GB · on dev", "attached"},
		{`{"size_gb":8,"mount":"/data","backup":false}`, "8 GB", "parked"},
	} {
		if got := volume.Summarize(json.RawMessage(c.spec), nil, names); got != c.want {
			t.Errorf("%s\n got  %q\n want %q", c.spec, got, c.want)
		}
		if word, on, ok := volume.StatusOf(json.RawMessage(c.spec), json.RawMessage(`{}`)); !ok || word != c.status || on != (c.status == "attached") {
			t.Errorf("%s: status %q %v %v", c.spec, word, on, ok)
		}
	}
	// what was seen says the state, before what was asked
	if word, on, _ := machine.StatusOf(json.RawMessage(`{"running":true}`), json.RawMessage(`{"running":false}`)); word != "stopped" || on {
		t.Errorf("asked to run, seen stopped: %q", word)
	}
	if word, _, _ := machine.StatusOf(json.RawMessage(`{"running":true}`), json.RawMessage(`{}`)); word != "running" {
		t.Errorf("asked to run, not seen yet: %q", word)
	}
	// a long value is cut; a type that says nothing has no sentence and no word
	long := typeOf(t, `{"x-hangar-summary": ["{key}"]}`)
	if got := long.Summarize(json.RawMessage(`{"key":"`+strings.Repeat("a", 80)+`"}`), nil, nil); len([]rune(got)) != 48 || !strings.HasSuffix(got, "…") {
		t.Errorf("%q", got)
	}
	plain := typeOf(t, `{"type":"object"}`)
	if _, _, ok := plain.StatusOf(nil, nil); ok || plain.Summarize(json.RawMessage(`{"a":1}`), nil, nil) != "" {
		t.Error("a type that says nothing says something")
	}
}

func TestASentenceThatDoesNotParseIsRefused(t *testing.T) {
	for _, bad := range []string{
		`{"x-hangar-summary": ["{kind"]}`, `{"x-hangar-summary": ["a [b"]}`, `{"x-hangar-summary": ["a ]b"]}`,
		`{"x-hangar-summary": ["[a [b]]"]}`, `{"x-hangar-summary": ["{}"]}`, `{"x-hangar-summary": ["{a b}"]}`,
		`{"x-hangar-summary": ["{a=1}"]}`, `{"x-hangar-summary": ["{a|b?words}"]}`, `{"x-hangar-summary": ["{a?}"]}`,
		`{"x-hangar-status": {"field": "running", "on": "running"}}`,
	} {
		if _, _, err := readOf([]byte(bad)); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
}
