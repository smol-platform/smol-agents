package builders

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	resource "k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	v1 "github.com/smol-platform/smol-agents/operator/api/v1"
)

// Terminal-plane wiring (M4.8). When a SmolAgent enables features.terminal, the
// agent runs inside a tmux session ("main") and one or two ttyd sidecars expose
// it: a writable DRIVER terminal and (when ReadOnlyDefault) a read-only VIEWER
// terminal. The cmd/agentterminal gateway (M4.10) is the only thing that should
// reach them — it authenticates the human, resolves an AttachGrant, then
// reverse-proxies WSS to the chosen ttyd, injecting the X-Smol-Attach header
// that ttyd requires (--auth-header). Defense-in-depth: a NetworkPolicy
// (BuildAgentTerminalIngress, M4.9) admits the ttyd ports only from the
// gateway's pod-selector, and ttyd refuses any request lacking the header.
//
// NOTE on bind address: the spec sketch uses `ttyd -i 127.0.0.1`, but a
// loopback-bound ttyd cannot be reached by the *separate* agentterminal
// Deployment (its own pod). We bind the pod IP and confine ttyd with the ingress
// NetworkPolicy + the required auth header — equivalent confinement for a
// Service-fronted, out-of-pod gateway.
const (
	// TerminalDriverPort is the writable ttyd (tmux attached read/write).
	TerminalDriverPort = 7681
	// TerminalViewerPort is the read-only ttyd (tmux attached read-only).
	TerminalViewerPort = 7682
	// TerminalAuthHeader is the header the gateway injects and ttyd requires;
	// a request without it is rejected (ttyd --auth-header).
	TerminalAuthHeader = "X-Smol-Attach"
	// tmuxSocketDir is the shared emptyDir holding the tmux server socket so the
	// agent (server) and the ttyd sidecars (clients) speak to the same session.
	tmuxSocketDir   = "/tmp/tmux"
	tmuxSocketPath  = "/tmp/tmux/agent.sock"
	tmuxSessionName = "main"
	terminalImage   = "terminal-sidecar"
)

// TerminalSidecarImage is the ttyd+tmux+asciinema sidecar image (M4.9).
func TerminalSidecarImage() string { return Image(terminalImage) }

// WireTerminal mutates a serving Pod spec to expose an interactive terminal,
// gated on features.terminal.enabled. It (1) wraps the agent container so it
// runs inside tmux "main", (2) adds a shared tmux-socket volume, (3) appends a
// writable driver ttyd and (when ReadOnlyDefault) a read-only viewer ttyd, and
// (4) when features.terminal.record AND the pod has an AgentFS volume, an
// asciinema recorder sidecar (M4.11). The agent image must carry tmux for the
// bootstrap (the terminal feature is a serving-path opt-in, like the per-kind
// harness bundles). No-op when disabled.
func WireTerminal(spec *corev1.PodSpec, cr *v1.SmolAgent) {
	tf := cr.Spec.Features.Terminal
	if !tf.Enabled {
		return
	}

	// Shared tmux socket dir (writable emptyDir) — agent server + ttyd clients.
	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name:         "tmux-sock",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})
	tmuxMount := corev1.VolumeMount{Name: "tmux-sock", MountPath: tmuxSocketDir}

	// Run the agent inside tmux "main" so the session persists across attach
	// disconnects and supports multiple viewers (multiplex). PID1 stays alive
	// until the tmux session ends, then exits so the pod's restart/complete
	// semantics are unchanged.
	if len(spec.Containers) > 0 && tf.Multiplex {
		wrapAgentInTmux(&spec.Containers[0])
		spec.Containers[0].VolumeMounts = append(spec.Containers[0].VolumeMounts, tmuxMount)
	}

	// Driver = writable ttyd (-W); viewer = a second read-only ttyd (no -W).
	spec.Containers = append(spec.Containers, ttydSidecar("ttyd-driver", TerminalDriverPort, true, tmuxMount))
	if tf.ReadOnlyDefault {
		spec.Containers = append(spec.Containers, ttydSidecar("ttyd-viewer", TerminalViewerPort, false, tmuxMount))
	}

	// asciinema recorder → AgentFS (M4.11), gated on record AND the workspace
	// volume being present (recording has nowhere durable to land otherwise).
	if tf.Record && hasVolumeNamed(spec.Volumes, storageFSVolumeName) {
		spec.Containers = append(spec.Containers, recorderSidecar(tmuxMount))
	}
}

// wrapAgentInTmux rewrites the agent container's entrypoint to launch the agent
// inside a detached tmux "main" session on the shared socket, then block PID1
// until that session ends. The original command/args are preserved as the
// session's program.
func wrapAgentInTmux(c *corev1.Container) {
	inner := strings.TrimSpace(strings.Join(append(append([]string{}, c.Command...), c.Args...), " "))
	if inner == "" {
		// Default serving entrypoint when the template left it implicit.
		inner = "/agent --config=/etc/smol-agents/agent.yaml"
	}
	boot := "tmux -S " + tmuxSocketPath + " new-session -d -s " + tmuxSessionName + " " + shellQuote(inner) +
		"; while tmux -S " + tmuxSocketPath + " has-session -t " + tmuxSessionName + " 2>/dev/null; do sleep 2; done"
	c.Command = []string{"/bin/sh", "-c", boot}
	c.Args = nil
}

// ttydSidecar renders a ttyd terminal sidecar on port, attaching tmux "main"
// (writable when driver, else read-only). It checks request Origin (-O) and
// requires the gateway's auth header, and runs under the same hardened PSA as
// the secret-proxy.
func ttydSidecar(name string, port int32, driver bool, tmuxMount corev1.VolumeMount) corev1.Container {
	args := []string{
		"-p", itoa(port),
		"-O",                                // --check-origin: reject cross-origin websockets
		"--auth-header", TerminalAuthHeader, // refuse requests without the gateway header
	}
	tmux := []string{"tmux", "-S", tmuxSocketPath, "attach", "-t", tmuxSessionName}
	if driver {
		args = append(args, "-W") // --writable
	} else {
		tmux = append(tmux, "-r") // read-only tmux attach
	}
	args = append(args, tmux...)

	return corev1.Container{
		Name:            name,
		Image:           TerminalSidecarImage(),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"ttyd"},
		Args:            args,
		Ports:           []corev1.ContainerPort{{Name: name, ContainerPort: port, Protocol: corev1.ProtocolTCP}},
		SecurityContext: hardenedSidecarSecurityContext(),
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("200m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("25m"),
				corev1.ResourceMemory: resource.MustParse("32Mi"),
			},
		},
		VolumeMounts: []corev1.VolumeMount{tmuxMount},
	}
}

// recorderSidecar streams an asciinema cast of tmux "main" to the AgentFS
// workspace (M4.11). It shares the tmux socket; the cast id correlates with the
// attach audit event the gateway emits.
func recorderSidecar(tmuxMount corev1.VolumeMount) corev1.Container {
	castPath := defaultStorageMountPath + "/.smol-session/casts/session.cast"
	rec := "asciinema rec --quiet --command " +
		shellQuote("tmux -S "+tmuxSocketPath+" attach -t "+tmuxSessionName+" -r") + " " + shellQuote(castPath)
	return corev1.Container{
		Name:            "ttyd-recorder",
		Image:           TerminalSidecarImage(),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/bin/sh", "-c"},
		Args:            []string{"mkdir -p " + shellQuote(defaultStorageMountPath+"/.smol-session/casts") + "; exec " + rec},
		SecurityContext: hardenedSidecarSecurityContext(),
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("16Mi"),
			},
		},
		VolumeMounts: []corev1.VolumeMount{tmuxMount, {Name: storageFSVolumeName, MountPath: defaultStorageMountPath}},
	}
}

func hardenedSidecarSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		ReadOnlyRootFilesystem:   ptr.To(true),
		RunAsNonRoot:             ptr.To(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// shellQuote single-quotes s for safe embedding in an `sh -c` string.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
