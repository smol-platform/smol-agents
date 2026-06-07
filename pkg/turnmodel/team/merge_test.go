package team

import (
	"bytes"
	"reflect"
	"testing"
)

func b(s string) []byte { return []byte(s) }

func TestThreeWayMerge_DisjointChanges(t *testing.T) {
	base := map[string][]byte{"a.txt": b("A"), "b.txt": b("B"), "common": b("x")}
	ours := map[string][]byte{"a.txt": b("A2"), "b.txt": b("B"), "common": b("x")}   // changed a
	theirs := map[string][]byte{"a.txt": b("A"), "b.txt": b("B2"), "common": b("x")} // changed b
	res := ThreeWayMerge(base, ours, theirs)
	if len(res.Conflicts) != 0 {
		t.Fatalf("disjoint changes must not conflict: %v", res.Conflicts)
	}
	want := map[string][]byte{"a.txt": b("A2"), "b.txt": b("B2"), "common": b("x")}
	if !reflect.DeepEqual(res.Merged, want) {
		t.Fatalf("merged wrong: %v", res.Merged)
	}
}

func TestThreeWayMerge_IdenticalChangeNoConflict(t *testing.T) {
	base := map[string][]byte{"f": b("0")}
	ours := map[string][]byte{"f": b("1")}
	theirs := map[string][]byte{"f": b("1")} // same edit
	res := ThreeWayMerge(base, ours, theirs)
	if len(res.Conflicts) != 0 || !bytes.Equal(res.Merged["f"], b("1")) {
		t.Fatalf("identical change must not conflict: %+v", res)
	}
}

func TestThreeWayMerge_DivergentConflict(t *testing.T) {
	base := map[string][]byte{"f": b("0")}
	ours := map[string][]byte{"f": b("ours")}
	theirs := map[string][]byte{"f": b("theirs")}
	res := ThreeWayMerge(base, ours, theirs)
	if len(res.Conflicts) != 1 || res.Conflicts[0] != "f" {
		t.Fatalf("divergent edit must conflict: %v", res.Conflicts)
	}
	if !bytes.Equal(res.Merged["f"], b("ours")) {
		t.Fatalf("conflict keeps ours tentatively: %q", res.Merged["f"])
	}
}

func TestThreeWayMerge_AddAndDelete(t *testing.T) {
	base := map[string][]byte{"keep": b("k"), "del": b("d")}
	// ours adds new + deletes del; theirs unchanged.
	ours := map[string][]byte{"keep": b("k"), "new": b("n")}
	theirs := map[string][]byte{"keep": b("k"), "del": b("d")}
	res := ThreeWayMerge(base, ours, theirs)
	if len(res.Conflicts) != 0 {
		t.Fatalf("add + clean delete must not conflict: %v", res.Conflicts)
	}
	if _, ok := res.Merged["del"]; ok {
		t.Fatalf("del should be removed: %v", res.Merged)
	}
	if !bytes.Equal(res.Merged["new"], b("n")) {
		t.Fatalf("new file should be added: %v", res.Merged)
	}
}

func TestThreeWayMerge_DeleteModifyConflict(t *testing.T) {
	base := map[string][]byte{"f": b("0")}
	ours := map[string][]byte{}                    // ours deleted f
	theirs := map[string][]byte{"f": b("changed")} // theirs modified f
	res := ThreeWayMerge(base, ours, theirs)
	if len(res.Conflicts) != 1 || res.Conflicts[0] != "f" {
		t.Fatalf("delete/modify must conflict: %v", res.Conflicts)
	}
	// ours (deleted) kept tentatively → f absent.
	if _, ok := res.Merged["f"]; ok {
		t.Fatalf("conflict kept ours (deleted): %v", res.Merged)
	}
}

func TestThreeWayMerge_BothDeleteClean(t *testing.T) {
	base := map[string][]byte{"f": b("0"), "g": b("1")}
	ours := map[string][]byte{"g": b("1")} // both delete f
	theirs := map[string][]byte{"g": b("1")}
	res := ThreeWayMerge(base, ours, theirs)
	if len(res.Conflicts) != 0 {
		t.Fatalf("both-delete must not conflict: %v", res.Conflicts)
	}
	if _, ok := res.Merged["f"]; ok {
		t.Fatalf("f should be gone: %v", res.Merged)
	}
}
