package main

import "testing"

func TestObserved(t *testing.T) {
	o := &observed{}
	o.record("spiffe://x/a")
	o.record("spiffe://x/b")
	got := o.snapshot()
	if len(got) != 2 || got[0] != "spiffe://x/a" || got[1] != "spiffe://x/b" {
		t.Errorf("snapshot = %v", got)
	}
	// Mutating returned slice must not affect internal state.
	got[0] = "mutated"
	if o.snapshot()[0] == "mutated" {
		t.Error("snapshot leaked internal slice")
	}
}

func TestBearer(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		{"Bearer abc.def.ghi", "abc.def.ghi"},
		{"bearer xyz", "xyz"},
		{"BEARER  spaced  ", "spaced"},
		{"Basic foo", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := bearer(tc.header); got != tc.want {
			t.Errorf("bearer(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}
