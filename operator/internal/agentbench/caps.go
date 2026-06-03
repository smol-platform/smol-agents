package agentbench

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Capability names a cluster feature a case may require. A case auto-SKIPs
// (loudly) when a required cap is missing — never a silent pass.
type Capability string

const (
	CapKubernetes    Capability = "kubernetes" // apiserver reachable
	CapKataFC        Capability = "runtimeclass:kata-fc"
	CapGateway       Capability = "gateway"        // agentgateway /healthz reachable
	CapHermes        Capability = "hermes"         // hermes gateway reachable
	CapS3            Capability = "s3"             // minio/s3 endpoint reachable
	CapMetadataBlock Capability = "metadata-block" // default-deny egress enforceable
	CapNATS          Capability = "nats"           // session queue reachable
	CapBroker        Capability = "broker"         // secret broker wired
)

// CapsResult is the outcome of a cluster probe.
type CapsResult struct {
	// Present is the set of detected capabilities.
	Present map[Capability]bool
	// NodeKernel is one node's kubelet-reported kernel version (for
	// isolation_kernel); empty if unreadable.
	NodeKernel string
	// ClusterName / Runtime are descriptive fields for the report header.
	Runtime string
	// Notes records why a cap was (not) detected, for the report.
	Notes map[Capability]string
}

// Has reports whether cap was detected.
func (r CapsResult) Has(cap Capability) bool { return r.Present[cap] }

// HasAll reports whether all required caps were detected.
func (r CapsResult) HasAll(req []string) (missing []string, ok bool) {
	for _, c := range req {
		if !r.Present[Capability(c)] {
			missing = append(missing, c)
		}
	}
	sort.Strings(missing)
	return missing, len(missing) == 0
}

// String renders the detected caps as a sorted comma list.
func (r CapsResult) String() string {
	var on []string
	for c, present := range r.Present {
		if present {
			on = append(on, string(c))
		}
	}
	sort.Strings(on)
	if len(on) == 0 {
		return "none"
	}
	return strings.Join(on, ",")
}

// ProbeOptions configures the cluster probe. URLs left empty are not probed
// (the corresponding cap stays false).
type ProbeOptions struct {
	GatewayURL string
	HermesURL  string
	S3URL      string
	// MetadataBlockable asserts the cluster's CNI enforces default-deny egress
	// (operator-known, not probeable from outside); set by the caller/flag.
	MetadataBlockable bool
	// HTTPClient overrides the default reachability client (tests inject one).
	HTTPClient *http.Client
}

// ProbeCaps inspects the cluster for the capabilities the bench plan needs. It
// is best-effort and fail-closed: an undetectable cap stays false so cases that
// need it SKIP rather than silently pass.
func ProbeCaps(ctx context.Context, c client.Client, opts ProbeOptions) CapsResult {
	res := CapsResult{
		Present: map[Capability]bool{},
		Notes:   map[Capability]string{},
		Runtime: "runc",
	}
	httpc := opts.HTTPClient
	if httpc == nil {
		httpc = &http.Client{Timeout: 5 * time.Second}
	}

	// kubernetes + node kernel.
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes); err == nil {
		res.Present[CapKubernetes] = true
		if len(nodes.Items) > 0 {
			res.NodeKernel = nodes.Items[0].Status.NodeInfo.KernelVersion
		}
	} else {
		res.Notes[CapKubernetes] = "node list failed: " + err.Error()
	}

	// kata-fc RuntimeClass.
	var rcs nodev1.RuntimeClassList
	if err := c.List(ctx, &rcs); err == nil {
		for _, rc := range rcs.Items {
			if rc.Name == "kata-fc" {
				res.Present[CapKataFC] = true
				res.Runtime = "kata-fc"
				break
			}
		}
		if !res.Present[CapKataFC] {
			res.Notes[CapKataFC] = "no kata-fc RuntimeClass (runc fallback — isolation cases SKIP)"
		}
	} else {
		res.Notes[CapKataFC] = "RuntimeClass list failed: " + err.Error()
	}

	// HTTP reachability probes.
	if opts.GatewayURL != "" {
		ok, note := probeHTTP(ctx, httpc, strings.TrimRight(opts.GatewayURL, "/")+"/healthz")
		res.Present[CapGateway] = ok
		res.Present[CapNATS] = ok // gateway up implies its NATS backend is wired
		res.Notes[CapGateway] = note
	}
	if opts.HermesURL != "" {
		ok, note := probeHTTP(ctx, httpc, opts.HermesURL)
		res.Present[CapHermes] = ok
		res.Notes[CapHermes] = note
	}
	if opts.S3URL != "" {
		ok, note := probeHTTP(ctx, httpc, opts.S3URL)
		res.Present[CapS3] = ok
		res.Notes[CapS3] = note
	}
	if opts.MetadataBlockable {
		res.Present[CapMetadataBlock] = true
	} else {
		res.Notes[CapMetadataBlock] = "not asserted by caller (--metadata-block); egress cases SKIP"
	}
	return res
}

// probeHTTP does a GET and reports reachability. Any HTTP response (even 4xx)
// counts as reachable — we only care that the endpoint answers.
func probeHTTP(ctx context.Context, c *http.Client, url string) (bool, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, "bad url: " + err.Error()
	}
	resp, err := c.Do(req)
	if err != nil {
		return false, "unreachable: " + err.Error()
	}
	defer resp.Body.Close()
	return true, "reachable (HTTP " + resp.Status + ")"
}
