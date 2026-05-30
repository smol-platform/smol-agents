package harness

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// imagesFromInput extracts image references from the JSON input's optional
// "images" array. Each entry is {"url":"..."} (an http(s) or data: URL passed
// through) or {"b64":"...","mime":"image/png"} (assembled into a data: URL).
// Returns nil when there are no images, so the text-only path is unaffected.
//
// Delivery note: images ride inside AgentRun.Input, which the operator marshals
// into a ~1 MiB ConfigMap — so a large inline b64 image overflows that cap
// before the pod starts. Prefer a URL for real images; keep inline b64 small.
// http(s) URLs are then subject to screenImages (SSRF gating).
func imagesFromInput(in json.RawMessage) []string {
	if len(in) == 0 {
		return nil
	}
	var m struct {
		Images []struct {
			URL  string `json:"url"`
			B64  string `json:"b64"`
			Mime string `json:"mime"`
		} `json:"images"`
	}
	if err := json.Unmarshal(in, &m); err != nil {
		return nil
	}
	var out []string
	for _, img := range m.Images {
		switch {
		case img.URL != "":
			out = append(out, img.URL)
		case img.B64 != "":
			mime := img.Mime
			if mime == "" {
				mime = "image/png"
			}
			out = append(out, "data:"+mime+";base64,"+img.B64)
		}
	}
	return out
}

// screenImages applies the agent's ImagePolicy to parsed image refs, returning
// the forwardable set or an error naming the first violation. A disallowed image
// FAILS the run (loud) rather than being dropped silently — dropping would
// quietly change the request the caller asked for.
//
// data: URIs are always allowed (self-contained — no fetch). An http(s) URL is
// allowed only when policy.AllowURLs is true, never to a private/loopback/
// link-local/metadata target, and — when AllowedURLHosts is set — only to a
// listed host. This is harness-side best-effort: the GATEWAY is the real fetcher,
// so it can't stop DNS rebinding (a public host resolving to an internal IP);
// the default (data: only) is the actual protection.
func screenImages(images []string, policy *v1.ImagePolicy) ([]string, error) {
	allowURLs := policy != nil && policy.AllowURLs
	var allowedHosts []string
	if policy != nil {
		allowedHosts = policy.AllowedURLHosts
	}
	out := make([]string, 0, len(images))
	for _, img := range images {
		if strings.HasPrefix(img, "data:") {
			out = append(out, img)
			continue
		}
		if err := screenImageURL(img, allowURLs, allowedHosts); err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, nil
}

func screenImageURL(raw string, allowURLs bool, allowedHosts []string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("harness: invalid image url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("harness: image url scheme %q not allowed (use https or an inline data: URI)", u.Scheme)
	}
	if !allowURLs {
		return fmt.Errorf("harness: http(s) image URLs are disabled; send an inline data: URI, or set harness.http.imagePolicy.allowURLs=true to opt in")
	}
	host := strings.ToLower(u.Hostname())
	if isInternalHost(host) {
		return fmt.Errorf("harness: image url host %q targets a private/internal address (always blocked)", host)
	}
	if len(allowedHosts) > 0 && !hostAllowed(host, allowedHosts) {
		return fmt.Errorf("harness: image url host %q not in harness.http.imagePolicy.allowedURLHosts", host)
	}
	return nil
}

// isInternalHost reports whether host is an obvious internal/metadata target:
// localhost, a *.localhost / *.internal name, or an IP literal in a loopback,
// private, link-local (incl. 169.254.169.254 cloud metadata), or unspecified
// range. DNS names that resolve to internal IPs are NOT caught here — that's the
// fetcher's job (the external gateway), which is why http(s) stays opt-in.
func isInternalHost(host string) bool {
	if host == "" || host == "localhost" ||
		strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".internal") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() ||
			ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
	}
	return false
}

func hostAllowed(host string, allowed []string) bool {
	for _, a := range allowed {
		if strings.EqualFold(host, a) {
			return true
		}
	}
	return false
}
