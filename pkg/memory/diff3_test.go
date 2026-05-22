package memory

import (
	"strings"
	"testing"
)

// ── isText ────────────────────────────────────────────────────────────────────

func TestIsText(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"empty", nil, true},
		{"plain ascii", []byte("hello world\n"), true},
		{"unicode", []byte("héllo wörld\n"), true},
		{"nul byte", []byte("hel\x00lo"), false},
		{"binary", []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, false}, // PNG magic
		{"invalid utf8", []byte{0xff, 0xfe, 0x61, 0x62}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isText(tc.in)
			if got != tc.want {
				t.Errorf("isText(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// ── splitLines / joinLines round-trip ────────────────────────────────────────

func TestSplitJoinRoundTrip(t *testing.T) {
	cases := []string{
		"",
		"single line without newline",
		"line one\nline two\n",
		"a\nb\nc\n",
		"no trailing newline",
		"a\nb\nc",
	}
	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			got := string(joinLines(splitLines([]byte(tc))))
			if got != tc {
				t.Errorf("round-trip %q → %q", tc, got)
			}
		})
	}
}

// ── mergeLines table-driven tests ────────────────────────────────────────────

// lines converts a multi-line string into a []string slice where each line
// retains its newline (matching splitLines output).
func lines(s string) []string {
	return splitLines([]byte(s))
}

// joined reassembles mergeLines output back to a plain string for assertions.
func joined(ls []string) string {
	return string(joinLines(ls))
}

func TestMergeLines_NoConflict(t *testing.T) {
	// Both sides identical to base: no conflict, output == base.
	t.Run("identical", func(t *testing.T) {
		base := lines("a\nb\nc\n")
		r := mergeLines(base, base, base, false, false)
		if len(r.Conflicts) != 0 {
			t.Errorf("expected no conflicts, got %d", len(r.Conflicts))
		}
		if joined(r.Lines) != "a\nb\nc\n" {
			t.Errorf("got %q, want a/b/c", joined(r.Lines))
		}
	})

	// Only ours changed (theirs unchanged from base): auto-merge takes ours.
	t.Run("only ours changed", func(t *testing.T) {
		base := lines("a\nb\nc\n")
		ours := lines("a\nB\nc\n")   // line 2 changed
		theirs := lines("a\nb\nc\n") // unchanged
		r := mergeLines(base, ours, theirs, false, false)
		if len(r.Conflicts) != 0 {
			t.Errorf("expected no conflicts, got %d: %+v", len(r.Conflicts), r.Conflicts)
		}
		if joined(r.Lines) != "a\nB\nc\n" {
			t.Errorf("expected ours version, got %q", joined(r.Lines))
		}
	})

	// Only theirs changed: auto-merge takes theirs.
	t.Run("only theirs changed", func(t *testing.T) {
		base := lines("a\nb\nc\n")
		ours := lines("a\nb\nc\n")   // unchanged
		theirs := lines("a\nT\nc\n") // line 2 changed
		r := mergeLines(base, ours, theirs, false, false)
		if len(r.Conflicts) != 0 {
			t.Errorf("expected no conflicts, got %d", len(r.Conflicts))
		}
		if joined(r.Lines) != "a\nT\nc\n" {
			t.Errorf("expected theirs version, got %q", joined(r.Lines))
		}
	})

	// Non-overlapping changes on different lines: both auto-merged.
	t.Run("non-overlapping changes", func(t *testing.T) {
		base := lines("a\nb\nc\nd\n")
		ours := lines("A\nb\nc\nd\n")   // line 1 changed by ours
		theirs := lines("a\nb\nc\nD\n") // line 4 changed by theirs
		r := mergeLines(base, ours, theirs, false, false)
		if len(r.Conflicts) != 0 {
			t.Errorf("expected no conflicts, got %d: %+v", len(r.Conflicts), r.Conflicts)
		}
		want := "A\nb\nc\nD\n"
		if joined(r.Lines) != want {
			t.Errorf("got %q, want %q", joined(r.Lines), want)
		}
	})

	// Both sides made the same change: no conflict.
	t.Run("same change both sides", func(t *testing.T) {
		base := lines("a\nb\nc\n")
		ours := lines("a\nX\nc\n")
		theirs := lines("a\nX\nc\n")
		r := mergeLines(base, ours, theirs, false, false)
		if len(r.Conflicts) != 0 {
			t.Errorf("expected no conflicts, got %d", len(r.Conflicts))
		}
		if joined(r.Lines) != "a\nX\nc\n" {
			t.Errorf("got %q, want same-change result", joined(r.Lines))
		}
	})

	// Ours added lines at end, theirs unchanged: no conflict.
	t.Run("ours appends", func(t *testing.T) {
		base := lines("a\n")
		ours := lines("a\nb\n")
		theirs := lines("a\n")
		r := mergeLines(base, ours, theirs, false, false)
		if len(r.Conflicts) != 0 {
			t.Errorf("unexpected conflicts: %+v", r.Conflicts)
		}
		if joined(r.Lines) != "a\nb\n" {
			t.Errorf("got %q, want a/b", joined(r.Lines))
		}
	})

	// Theirs deleted a line, ours unchanged: take deletion.
	t.Run("theirs deletes", func(t *testing.T) {
		base := lines("a\nb\nc\n")
		ours := lines("a\nb\nc\n")
		theirs := lines("a\nc\n") // deleted b
		r := mergeLines(base, ours, theirs, false, false)
		if len(r.Conflicts) != 0 {
			t.Errorf("unexpected conflicts: %+v", r.Conflicts)
		}
		if joined(r.Lines) != "a\nc\n" {
			t.Errorf("got %q, want a/c", joined(r.Lines))
		}
	})
}

func TestMergeLines_Conflicts_Markers(t *testing.T) {
	// Overlapping edit/edit: both sides changed the same line differently.
	t.Run("edit/edit conflict markers", func(t *testing.T) {
		base := lines("a\nb\nc\n")
		ours := lines("a\nOURS\nc\n")
		theirs := lines("a\nTHEIRS\nc\n")
		r := mergeLines(base, ours, theirs, true, false)
		if len(r.Conflicts) != 1 {
			t.Fatalf("expected 1 conflict, got %d: %+v", len(r.Conflicts), r.Conflicts)
		}
		out := joined(r.Lines)
		// Markers must be present.
		if !strings.Contains(out, "<<<<<<< ours") {
			t.Errorf("missing ours marker in %q", out)
		}
		if !strings.Contains(out, "=======") {
			t.Errorf("missing separator in %q", out)
		}
		if !strings.Contains(out, ">>>>>>> theirs") {
			t.Errorf("missing theirs marker in %q", out)
		}
		// Both sides' content must appear.
		if !strings.Contains(out, "OURS") {
			t.Errorf("ours content missing from %q", out)
		}
		if !strings.Contains(out, "THEIRS") {
			t.Errorf("theirs content missing from %q", out)
		}
		// Non-conflicting lines are preserved outside markers.
		if !strings.Contains(out, "a\n") || !strings.Contains(out, "c\n") {
			t.Errorf("surrounding context lines missing from %q", out)
		}
	})
}

func TestMergeLines_Conflicts_Union(t *testing.T) {
	// Overlapping edit/edit: union emits ours lines then theirs lines.
	t.Run("edit/edit union", func(t *testing.T) {
		base := lines("a\nb\nc\n")
		ours := lines("a\nOURS\nc\n")
		theirs := lines("a\nTHEIRS\nc\n")
		r := mergeLines(base, ours, theirs, false, true)
		if len(r.Conflicts) != 1 {
			t.Fatalf("expected 1 conflict, got %d", len(r.Conflicts))
		}
		out := joined(r.Lines)
		// Both lines present, no markers.
		if strings.Contains(out, "<<<<") {
			t.Errorf("union should have no markers: %q", out)
		}
		if !strings.Contains(out, "OURS") || !strings.Contains(out, "THEIRS") {
			t.Errorf("both sides should appear: %q", out)
		}
		// Ours before theirs.
		oursIdx := strings.Index(out, "OURS")
		theirsIdx := strings.Index(out, "THEIRS")
		if oursIdx >= theirsIdx {
			t.Errorf("ours should precede theirs in union output: %q", out)
		}
	})
}

func TestMergeLines_EmptyBase(t *testing.T) {
	// add/add: both sides produced different content from an empty base.
	t.Run("add/add markers", func(t *testing.T) {
		base := lines("")
		ours := lines("from-ours\n")
		theirs := lines("from-theirs\n")
		r := mergeLines(base, ours, theirs, true, false)
		if len(r.Conflicts) != 1 {
			t.Fatalf("expected 1 conflict, got %d", len(r.Conflicts))
		}
		out := joined(r.Lines)
		if !strings.Contains(out, "<<<<<<< ours") {
			t.Errorf("missing marker: %q", out)
		}
	})
}

func TestMergeLines_MultipleConflicts(t *testing.T) {
	// Two non-adjacent conflicts: both reported, auto-merge between them.
	base := lines("1\n2\n3\n4\n5\n")
	ours := lines("O1\n2\nO3\n4\n5\n")
	theirs := lines("T1\n2\nT3\n4\n5\n")
	r := mergeLines(base, ours, theirs, true, false)
	if len(r.Conflicts) != 2 {
		t.Fatalf("expected 2 conflicts, got %d: %+v", len(r.Conflicts), r.Conflicts)
	}
	out := joined(r.Lines)
	// Both conflict markers present.
	count := strings.Count(out, "<<<<<<< ours")
	if count != 2 {
		t.Errorf("expected 2 conflict markers, got %d in %q", count, out)
	}
}
