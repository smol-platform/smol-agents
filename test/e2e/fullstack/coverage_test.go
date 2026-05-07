package fullstack

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// TestCoverageGate enforces R-E2E-VRF-1: every R-E2E-* requirement
// in requirements.md MUST be referenced in `Coverage` (this package's
// coverage.go map). New requirements without a planned test fail the
// gate; old requirements that get deleted from the spec also fail
// because their dangling Coverage entry won't match.
//
// Fails noisily with a unified report so the PR author sees exactly
// what's missing.
func TestCoverageGate(t *testing.T) {
	specPath := repoFile("requirements.md")
	want, err := parseRequirementIDs(specPath)
	if err != nil {
		t.Fatalf("parse requirements.md: %v", err)
	}
	if len(want) == 0 {
		t.Fatalf("no R-E2E-* IDs found in %s — coverage gate would be no-op", specPath)
	}

	gotMissing := []string{}
	for id := range want {
		if _, ok := Coverage[id]; !ok {
			gotMissing = append(gotMissing, id)
		}
	}
	sort.Strings(gotMissing)

	dangling := []string{}
	for id := range Coverage {
		if _, ok := want[id]; !ok {
			dangling = append(dangling, id)
		}
	}
	sort.Strings(dangling)

	if len(gotMissing) > 0 {
		t.Errorf("requirement IDs in spec but missing from Coverage map (R-E2E-VRF-1):\n  %s\n"+
			"Add an entry to test/e2e/fullstack/coverage.go for each.",
			strings.Join(gotMissing, "\n  "))
	}
	if len(dangling) > 0 {
		t.Errorf("Coverage map references IDs not in requirements.md:\n  %s\n"+
			"Either re-add to spec or remove from coverage.go.",
			strings.Join(dangling, "\n  "))
	}
}

// parseRequirementIDs scans requirements.md for tokens like
// `**R-E2E-FOO-1**` and returns them as a set.
func parseRequirementIDs(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Match `**R-E2E-...-N**` (the canonical bold form in
	// requirements.md). Use a non-greedy middle so multi-word IDs
	// like R-E2E-SCN-EBPF-DROP are captured.
	re := regexp.MustCompile(`\*\*(R-E2E-[A-Z0-9-]+)\*\*`)
	out := map[string]bool{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		for _, m := range re.FindAllStringSubmatch(scanner.Text(), -1) {
			out[m[1]] = true
		}
	}
	return out, scanner.Err()
}

// repoFile walks up from this file to find the spec dir.
func repoFile(rel string) string {
	_, here, _, _ := runtime.Caller(0)
	dir := filepath.Dir(here)
	for {
		// Look for the spec dir as the anchor (more specific than
		// go.mod, prevents climbing too far on test machines).
		spec := filepath.Join(dir, ".spec-workflow", "specs", "knative-agents-fullstack-e2e")
		if _, err := os.Stat(spec); err == nil {
			return filepath.Join(spec, rel)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// Helper used by Test debugging only — not exported in any other path.
var _ = fmt.Sprintf
