package team

import (
	"bytes"
	"sort"
)

// MergeResult is the outcome of a 3-way merge of two member branches against
// their common base.
type MergeResult struct {
	// Merged is the reconciled file tree (path → content). A path absent here was
	// deleted on both sides (or on one side with the other unchanged).
	Merged map[string][]byte
	// Conflicts lists paths both branches changed differently — kept as "ours" in
	// Merged, surfaced for the coordinator (or a human) to resolve. Sorted.
	Conflicts []string
}

// ThreeWayMerge reconciles two member branches (ours, theirs) against their
// common base, per file. This turns the "two teammates edit one file" overwrite
// risk the Claude Code docs flag into an enforced, conflict-aware merge — the
// platform's advantage over a local agent team. Each map is path → content (a
// flattened AgentFS tree; a deletion is an absent key).
//
// Per path:
//   - both sides equal (incl. both deleted)      → take it, no conflict
//   - only one side changed from base            → take the changed side
//   - both changed differently                   → CONFLICT (keep ours, record path)
//
// Pure + deterministic (Conflicts is sorted), so it is fully unit-testable; the
// live coordinator extracts the three trees from kopia snapshots and applies the
// result (that extraction is the deployment-side wiring).
func ThreeWayMerge(base, ours, theirs map[string][]byte) MergeResult {
	merged := make(map[string][]byte)
	var conflicts []string

	seen := make(map[string]struct{})
	for _, m := range []map[string][]byte{base, ours, theirs} {
		for p := range m {
			seen[p] = struct{}{}
		}
	}

	for p := range seen {
		b, hasB := base[p]
		o, hasO := ours[p]
		t, hasT := theirs[p]

		oursEqBase := hasO == hasB && (!hasO || bytes.Equal(o, b))
		theirsEqBase := hasT == hasB && (!hasT || bytes.Equal(t, b))
		oursEqTheirs := hasO == hasT && (!hasO || bytes.Equal(o, t))

		switch {
		case oursEqTheirs:
			if hasO {
				merged[p] = o // both made the same change (or both deleted → omit)
			}
		case oursEqBase:
			if hasT {
				merged[p] = t // only theirs changed (incl. theirs-deleted → omit)
			}
		case theirsEqBase:
			if hasO {
				merged[p] = o // only ours changed
			}
		default:
			// Both changed differently (incl. delete/modify) — conflict. Keep ours
			// tentatively so the merged tree is usable; the coordinator resolves.
			conflicts = append(conflicts, p)
			if hasO {
				merged[p] = o
			}
		}
	}

	sort.Strings(conflicts)
	return MergeResult{Merged: merged, Conflicts: conflicts}
}
