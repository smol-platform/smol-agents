package shared

import "strings"

// ParseProbeLines turns the spiffe-probe Pod's stdout into ProbeLines.
// Each line is `OK <scenario> <detail>` or `FAIL <scenario> <detail>`;
// other lines are skipped so callers can ignore boilerplate logs.
func ParseProbeLines(s string) []ProbeLine {
	var out []ProbeLine
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "OK "):
			rest := strings.TrimPrefix(line, "OK ")
			scen, detail, _ := strings.Cut(rest, " ")
			out = append(out, ProbeLine{OK: true, Scenario: scen, Detail: detail})
		case strings.HasPrefix(line, "FAIL "):
			rest := strings.TrimPrefix(line, "FAIL ")
			scen, detail, _ := strings.Cut(rest, " ")
			out = append(out, ProbeLine{OK: false, Scenario: scen, Detail: detail})
		}
	}
	return out
}

// JSONStringList renders ["a","b"] for embedding into a YAML args
// field — kubectl applies the result as a JSON list inside YAML.
func JSONStringList(items []string) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, s := range items {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(strings.ReplaceAll(s, `"`, `\"`))
		b.WriteByte('"')
	}
	b.WriteByte(']')
	return b.String()
}
