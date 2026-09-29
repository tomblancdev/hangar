package ids

import "testing"

func TestTheShape(t *testing.T) {
	id := New("box")
	if !Valid(id) || Prefix(id) != "box" || len(id) != len("box-")+17 {
		t.Fatalf("%q", id)
	}
	if New("m") == New("m") {
		t.Fatal("two ids alike")
	}
	for _, bad := range []string{"box-0123", "Box-0123456789abcdef0", "box-0123456789ABCDEF0", "toolongpfx-0123456789abcdef0"} {
		if Valid(bad) {
			t.Errorf("%q passed", bad)
		}
	}
	if ValidPrefix("op1") != true || ValidPrefix("1op") || ValidPrefix("") {
		t.Fatal("prefix shape")
	}
}
