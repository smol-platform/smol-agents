// Package memory — in-tree 3-way line-level diff3 engine.
//
// This file implements a compact diff3 merge over []string (lines). No external
// dependency is used. The LCS computation is standard O(n*m) DP.
//
// Terminology mirrors classic diff3:
//
//	base (B)   — common ancestor (fork-point snapshot)
//	ours (O)   — current destination branch content
//	theirs (T) — incoming source branch content
//
// The algorithm works in three passes:
//
//  1. diff(base, ours)   → per-base-line mapping: which ours-line corresponds.
//  2. diff(base, theirs) → per-base-line mapping: which theirs-line corresponds.
//  3. Scan base lines from 0..n and at each position decide:
//     - Both sides equal at this base position → keep (auto-merge).
//     - Only one side changed → take that side (auto-merge).
//     - Both sides changed differently → conflict.
//
// "Changed" means the base line was deleted or replaced, or new lines were
// inserted before the next equal run. This approach is correct for
// non-overlapping changes and produces conflict hunks for true overlaps.
package memory

import (
	"unicode/utf8"
)

// hunkConflict describes one conflicting region in the 3-way merge output.
type hunkConflict struct {
	OursLines   []string
	TheirsLines []string
}

// diff3Result is the output of mergeLines.
type diff3Result struct {
	// Lines is the merged line sequence (includes conflict markers or union
	// content depending on the mode used).
	Lines []string
	// Conflicts records the raw conflict regions (populated even for markers
	// and union so callers know where edits were).
	Conflicts []hunkConflict
}

// isText returns true when content is valid UTF-8 and contains no NUL bytes.
func isText(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	for _, b := range data {
		if b == 0 {
			return false
		}
	}
	return utf8.Valid(data)
}

// splitLines splits content into lines, each retaining its trailing newline.
// A final non-empty segment without a newline is included as-is.
func splitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	var out []string
	start := 0
	for i, b := range data {
		if b == '\n' {
			out = append(out, string(data[start:i+1]))
			start = i + 1
		}
	}
	if start < len(data) {
		out = append(out, string(data[start:]))
	}
	return out
}

// joinLines reassembles a line slice into []byte.
func joinLines(lines []string) []byte {
	total := 0
	for _, l := range lines {
		total += len(l)
	}
	out := make([]byte, 0, total)
	for _, l := range lines {
		out = append(out, l...)
	}
	return out
}

// ── LCS ─────────────────────────────────────────────────────────────────────

// lcsIndices computes the Longest Common Subsequence of two string slices and
// returns the matching indices into a and b respectively.
//
// Uses the standard O(n*m) DP approach. Fine for typical file sizes in agentfs
// (agent memory documents, not Linux kernel source).
func lcsIndices(a, b []string) (aIdx, bIdx []int) {
	n, m := len(a), len(b)
	if n == 0 || m == 0 {
		return nil, nil
	}

	// Flat DP table dp[i*(m+1)+j] = LCS length of a[:i],b[:j].
	dp := make([]int, (n+1)*(m+1))
	w := m + 1

	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			if a[i-1] == b[j-1] {
				dp[i*w+j] = dp[(i-1)*w+(j-1)] + 1
			} else if dp[(i-1)*w+j] >= dp[i*w+j-1] {
				dp[i*w+j] = dp[(i-1)*w+j]
			} else {
				dp[i*w+j] = dp[i*w+j-1]
			}
		}
	}

	lenLCS := dp[n*w+m]
	if lenLCS == 0 {
		return nil, nil
	}

	aIdx = make([]int, 0, lenLCS)
	bIdx = make([]int, 0, lenLCS)
	i, j := n, m
	for i > 0 && j > 0 {
		if a[i-1] == b[j-1] {
			aIdx = append(aIdx, i-1)
			bIdx = append(bIdx, j-1)
			i--
			j--
		} else if dp[(i-1)*w+j] >= dp[i*w+j-1] {
			i--
		} else {
			j--
		}
	}
	// Reverse: backtracking builds in reverse order.
	for lo, hi := 0, len(aIdx)-1; lo < hi; lo, hi = lo+1, hi-1 {
		aIdx[lo], aIdx[hi] = aIdx[hi], aIdx[lo]
		bIdx[lo], bIdx[hi] = bIdx[hi], bIdx[lo]
	}
	return aIdx, bIdx
}

// ── side-diff helper ─────────────────────────────────────────────────────────

// sideMap describes how one side (ours or theirs) maps to base lines.
// baseToSide[i] is the side-index corresponding to base[i], or -1 if that
// base line was deleted/replaced by this side.
// insertsBefore[i] holds any lines inserted by this side before base[i].
// trailingInserts holds lines inserted after the last base line.
type sideMap struct {
	baseToSide      []int      // len == len(base); -1 = deleted
	insertsBefore   [][]string // len == len(base); may be nil
	trailingInserts []string
}

// buildSideMap diffs side against base and builds the sideMap.
func buildSideMap(base, side []string) sideMap {
	aIdx, bIdx := lcsIndices(base, side)

	sm := sideMap{
		baseToSide:    make([]int, len(base)),
		insertsBefore: make([][]string, len(base)),
	}
	for i := range sm.baseToSide {
		sm.baseToSide[i] = -1
	}
	for k := range aIdx {
		sm.baseToSide[aIdx[k]] = bIdx[k]
	}

	// Walk through side to find insertions: side lines that appear between
	// consecutive common elements (or before the first / after the last).
	lcsPairs := make([][2]int, len(aIdx))
	for k := range aIdx {
		lcsPairs[k] = [2]int{aIdx[k], bIdx[k]}
	}

	prevSide := 0
	for k := range lcsPairs {
		bi, si := lcsPairs[k][0], lcsPairs[k][1]
		// Lines in side[prevSide..si) are inserted before base[bi].
		if si > prevSide {
			sm.insertsBefore[bi] = append(sm.insertsBefore[bi], side[prevSide:si]...)
		}
		prevSide = si + 1
	}
	// Trailing insertions after the last common element.
	if prevSide < len(side) {
		sm.trailingInserts = side[prevSide:]
	}

	return sm
}

// sideAt returns the side's version of base[bi]: inserted lines before bi,
// then the replacement (or nothing if deleted). ok is false when base[bi] was
// deleted on this side.
func (sm *sideMap) sideAt(bi int) (insertsBefore []string, kept bool, sideLine string) {
	insertsBefore = sm.insertsBefore[bi]
	sideIdx := sm.baseToSide[bi]
	if sideIdx < 0 {
		return insertsBefore, false, ""
	}
	// We'd need to know side[] to get the exact line — but since we track that
	// base[bi] maps to side[sideIdx] and if a[i-1]==b[j-1] from LCS, the line
	// is the same. (LCS only matches equal strings.) So sideLine == base[bi].
	return insertsBefore, true, ""
}

// ── 3-way merge ─────────────────────────────────────────────────────────────

// mergeLines performs a 3-way line-level merge.
//
//   - emitMarkers=true  → write git-style conflict markers around conflict hunks.
//   - unionMode=true    → append theirs lines after ours for conflict hunks.
//   - both false        → keep ours (caller handles output separately).
//
// The Conflicts slice is always populated when real conflicts occur, regardless
// of the mode.
func mergeLines(base, ours, theirs []string, emitMarkers bool, unionMode bool) diff3Result {
	oMap := buildSideMap(base, ours)
	tMap := buildSideMap(base, theirs)

	var out []string
	var conflicts []hunkConflict

	// We scan base line by line. At each position we collect what each side
	// wants: insertions before, and whether the base line itself is kept.
	//
	// Conflict detection: when both sides differ from base at the same position.
	// We collect contiguous conflicting runs into a single conflict hunk for
	// cleaner output (fewer markers).

	// emit flushes a conflict hunk (accumulated O and T lines) to output.
	emitConflict := func(oLines, tLines []string) {
		if slicesEqual(oLines, tLines) {
			// Both sides produced the same output — not a real conflict.
			out = append(out, oLines...)
			return
		}
		conflicts = append(conflicts, hunkConflict{OursLines: oLines, TheirsLines: tLines})
		switch {
		case emitMarkers:
			out = append(out, "<<<<<<< ours\n")
			out = append(out, oLines...)
			out = append(out, "=======\n")
			out = append(out, tLines...)
			out = append(out, ">>>>>>> theirs\n")
		case unionMode:
			out = append(out, oLines...)
			out = append(out, tLines...)
		default:
			// keep ours
			out = append(out, oLines...)
		}
	}

	// pendingO / pendingT accumulate lines for a multi-line conflict region.
	var pendingO, pendingT []string
	inConflict := false

	flushConflict := func() {
		if inConflict && (len(pendingO) > 0 || len(pendingT) > 0) {
			emitConflict(pendingO, pendingT)
		}
		pendingO, pendingT = nil, nil
		inConflict = false
	}

	for bi := 0; bi < len(base); bi++ {
		oInserts := oMap.insertsBefore[bi]
		tInserts := tMap.insertsBefore[bi]
		oKept := oMap.baseToSide[bi] >= 0
		tKept := tMap.baseToSide[bi] >= 0
		baseLine := base[bi]

		// Classify the state of this base position across both sides.
		// We categorise:
		//   insertBefore: lines to insert before base[bi]
		//   replaces:     base[bi] is replaced/deleted
		// "unchanged" means: no inserts before AND kept with same content.

		// First handle insertions before base[bi].
		// If both sides insert different content before base[bi], that is a conflict.
		// If only one side inserts, that's an auto-merge.
		oInsChanged := len(oInserts) > 0
		tInsChanged := len(tInserts) > 0

		switch {
		case oInsChanged && tInsChanged:
			if slicesEqual(oInserts, tInserts) {
				// Both sides inserted same lines — safe to auto-merge.
				flushConflict()
				out = append(out, oInserts...)
			} else {
				// Conflict in the insertions.
				pendingO = append(pendingO, oInserts...)
				pendingT = append(pendingT, tInserts...)
				inConflict = true
			}
		case oInsChanged && !tInsChanged:
			flushConflict()
			out = append(out, oInserts...)
		case !oInsChanged && tInsChanged:
			flushConflict()
			out = append(out, tInserts...)
		default:
			// No insertions before this line from either side.
		}

		// Now handle base[bi] itself.
		switch {
		case oKept && tKept:
			// Both kept base[bi] unchanged (base[bi] == the line on both sides by LCS).
			flushConflict()
			out = append(out, baseLine)

		case !oKept && !tKept:
			// Both deleted base[bi].
			// If they're in a conflict region, the "delete" from both is agreement.
			if inConflict {
				// Both sides agree to delete this line in their conflict region.
				// Don't add to either pending.
			} else {
				// Clean delete by both: nothing to emit.
			}

		case oKept && !tKept:
			// O kept base[bi], T deleted it.
			// O didn't change this line vs base (it's still there). T deleted it.
			// If there were no inserts around it either, this is T's delete of an
			// unchanged line → auto-merge: take T (delete wins).
			// But if O has inserts around it, we need to decide.
			// Standard diff3: T deleted it = take T's deletion.
			if inConflict {
				pendingO = append(pendingO, baseLine)
				// T side: line deleted, nothing to append to pendingT.
			} else {
				// Only T changed (deleted); O is unchanged. Take T's deletion.
				// (Don't emit baseLine.)
			}

		case !oKept && tKept:
			// O deleted base[bi], T kept it.
			// Only O changed (deleted); T is unchanged. Take O's deletion.
			if inConflict {
				// T side kept this line; O didn't.
				pendingT = append(pendingT, baseLine)
				// O side: line deleted, nothing to append to pendingO.
			} else {
				// Only O deleted; nothing to emit.
			}
		}
	}
	flushConflict()

	// Handle trailing insertions (lines added by either/both sides after base).
	oTrailing := oMap.trailingInserts
	tTrailing := tMap.trailingInserts

	switch {
	case len(oTrailing) > 0 && len(tTrailing) > 0:
		if slicesEqual(oTrailing, tTrailing) {
			out = append(out, oTrailing...)
		} else {
			emitConflict(oTrailing, tTrailing)
		}
	case len(oTrailing) > 0:
		out = append(out, oTrailing...)
	case len(tTrailing) > 0:
		out = append(out, tTrailing...)
	}

	return diff3Result{Lines: out, Conflicts: conflicts}
}

// slicesEqual returns true when a and b have the same length and all elements equal.
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
