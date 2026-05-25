package cloud

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// LoadWireGuardConfig reads a wg-quick config file. Returns the file's content
// (to embed in cloud-init) and the first Address IP (without the CIDR suffix)
// so the deploy driver knows what private IP this node will hold.
func LoadWireGuardConfig(path string) (content, address string, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("read wireguard config %s: %w", path, err)
	}
	addr, err := ParseWGAddress(raw)
	if err != nil {
		return "", "", err
	}
	if strings.Contains(string(raw), "AGENTCTL_WG_EOF") {
		return "", "", fmt.Errorf("wireguard config %s contains the cloud-init heredoc delimiter; rename or escape", path)
	}
	return string(raw), addr, nil
}

// ParseWGAddress returns the first Address from a wg-quick config, with the
// CIDR suffix stripped (e.g. "10.0.0.5/24" -> "10.0.0.5"). wg-quick allows
// multiple Address lines (IPv4 + IPv6 dual-stack); we return the first.
func ParseWGAddress(conf []byte) (string, error) {
	s := bufio.NewScanner(strings.NewReader(string(conf)))
	inInterface := false
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inInterface = strings.EqualFold(line, "[Interface]")
			continue
		}
		if !inInterface {
			continue
		}
		k, v, ok := splitKV(line)
		if !ok || !strings.EqualFold(k, "Address") {
			continue
		}
		// Address can be "10.0.0.5/24" or "10.0.0.5/24, fd00::5/64".
		first := strings.TrimSpace(strings.Split(v, ",")[0])
		ip, _, _ := strings.Cut(first, "/")
		if ip == "" {
			return "", fmt.Errorf("wireguard Address line malformed: %q", v)
		}
		return ip, nil
	}
	if err := s.Err(); err != nil {
		return "", fmt.Errorf("scan wireguard config: %w", err)
	}
	return "", fmt.Errorf("no [Interface] Address line in wireguard config")
}

// splitKV splits a "key = value" line into trimmed key and value.
func splitKV(line string) (key, value string, ok bool) {
	i := strings.IndexByte(line, '=')
	if i < 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:]), true
}
