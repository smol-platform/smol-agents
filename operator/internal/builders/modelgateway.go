package builders

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	resource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
)

// ModelGateway builders (yxh.2): render an operator-managed model/agent gateway
// (ConfigMap + Deployment + Service) from a ModelGateway CR, hardened like a run
// pod. The provider profile supplies the only provider-specific bits — keeping
// the CRD + reconciler generic with hermes as implementation #1.

const (
	modelGatewayConfigVolume = "config"
	modelGatewayDataVolume   = "data"
	modelGatewayConfigPath   = "/config"

	// UI auth-front (spec.ui): an nginx-unprivileged sidecar enforces basic-auth
	// from an htpasswd Secret and proxies the gateway port on loopback (h92.1).
	modelGatewayUIConfigVolume = "ui-config"
	modelGatewayUIAuthVolume   = "ui-auth"
	modelGatewayUINginxConf    = "ui-nginx.conf"
	gatewayUIProxyImage        = "nginxinc/nginx-unprivileged:1.27-alpine"
	gatewayUIOIDCProxyImage    = "quay.io/oauth2-proxy/oauth2-proxy:v7.7.1"
	gatewayUIAuthDefaultKey    = "htpasswd"
)

// uiEnabled reports whether the gateway opts its web UI into authenticated exposure.
func uiEnabled(gw *amv1.ModelGateway) bool { return gw.Spec.UI != nil && gw.Spec.UI.Expose }

// uiSharedSecret / uiOIDCProxy select the auth-front implementation.
func uiSharedSecret(gw *amv1.ModelGateway) bool {
	return uiEnabled(gw) && gw.Spec.UI.Auth.Mode == "sharedSecret"
}
func uiOIDCProxy(gw *amv1.ModelGateway) bool {
	return uiEnabled(gw) && gw.Spec.UI.Auth.Mode == "oidcProxy"
}

// uiUpstreamPort resolves the in-pod port the auth-front proxies to: an explicit
// spec.ui.upstreamPort, else the provider's UI/dashboard port, else the gateway
// port.
func uiUpstreamPort(gw *amv1.ModelGateway) int32 {
	if gw.Spec.UI != nil && gw.Spec.UI.UpstreamPort > 0 {
		return gw.Spec.UI.UpstreamPort
	}
	if p := profileFor(gw.Spec.Provider); p.uiUpstreamPort > 0 {
		return p.uiUpstreamPort
	}
	return gw.Spec.EffectivePort()
}

// uiAuthKey is the htpasswd key within the auth Secret (default "htpasswd").
func uiAuthKey(gw *amv1.ModelGateway) string {
	if k := gw.Spec.UI.Auth.SecretRef.Key; k != "" {
		return k
	}
	return gatewayUIAuthDefaultKey
}

// gatewayProfile carries the per-provider deployment conventions.
type gatewayProfile struct {
	args     []string // container args (image ENTRYPOINT preserved)
	dataDir  string   // writable data dir the config is seeded into
	configIn string   // file the gateway reads (within dataDir)
	fsGroup  int64    // group owning the data dir
	stdEnv   func(port int32) []corev1.EnvVar
	// initCaps are the capabilities the gateway image's own init system needs to
	// step down from root to its unprivileged user (e.g. s6-overlay's
	// s6-applyuidgid → setgroups/setgid/setuid + the data-dir chown). The container
	// still drops ALL and adds back only these; nil = drop everything (Go services).
	initCaps     []corev1.Capability
	allowPrivEsc bool // the init's privilege-drop chain needs no_new_privs OFF
	// uiUpstreamPort is the in-pod port the auth-front proxies to when spec.ui is
	// enabled — the gateway's UI/dashboard, which may differ from the API port
	// (hermes serves its dashboard on a separate 9119). 0 = use the gateway port.
	uiUpstreamPort int32
	// uiEnableEnv is the env the gateway container needs to actually serve its UI
	// when spec.ui is enabled (hermes: turn the dashboard service on, loopback-
	// bound so the operator's auth-front is the sole human gate). Empty = the UI is
	// already served on the gateway port (no extra wiring).
	uiEnableEnv []corev1.EnvVar
}

func hermesProfile() gatewayProfile {
	return gatewayProfile{
		args:     []string{"gateway", "run"},
		dataDir:  "/opt/data",
		configIn: "config.yaml",
		fsGroup:  10000, // the hermes user
		// hermes-agent boots under s6-overlay as root and drops to uid 10000; that
		// chain needs these caps (proven on gtr: dropping ALL → "s6-applyuidgid:
		// unable to set supplementary group list: Operation not permitted" → crash).
		initCaps:     []corev1.Capability{"CHOWN", "SETUID", "SETGID", "DAC_OVERRIDE", "FOWNER", "KILL", "SETPCAP"},
		allowPrivEsc: true,
		// The hermes dashboard is a separate s6 service on 9119, gated off by
		// default. When spec.ui is exposed we turn it on and bind it to loopback —
		// the dashboard's own OAuth gate disengages on loopback binds, so the
		// operator's nginx basic-auth sidecar (proxying 127.0.0.1:9119) is the sole
		// human gate. --insecure guarantees it starts without a registered OAuth
		// provider (safe: it is only reachable through the authenticated sidecar).
		uiUpstreamPort: 9119,
		uiEnableEnv: []corev1.EnvVar{
			{Name: "HERMES_DASHBOARD", Value: "1"},
			{Name: "HERMES_DASHBOARD_HOST", Value: "127.0.0.1"},
			{Name: "HERMES_DASHBOARD_PORT", Value: "9119"},
			{Name: "HERMES_DASHBOARD_INSECURE", Value: "1"},
		},
		stdEnv: func(port int32) []corev1.EnvVar {
			return []corev1.EnvVar{
				{Name: "HERMES_HOME", Value: "/opt/data"},
				{Name: "API_SERVER_ENABLED", Value: "true"},
				{Name: "API_SERVER_HOST", Value: "0.0.0.0"}, // default 127.0.0.1 is unreachable cross-pod
				{Name: "API_SERVER_PORT", Value: itoa(port)},
			}
		},
	}
}

func profileFor(provider string) gatewayProfile {
	// Only hermes today; validation rejects other providers before we render.
	return hermesProfile()
}

// ModelGatewayName is the deterministic name of a gateway's owned resources.
func ModelGatewayName(gw *amv1.ModelGateway) string { return "mgw-" + gw.Name }

func modelGatewayLabels(gw *amv1.ModelGateway) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":                     "modelgateway",
		"app.kubernetes.io/instance":                 gw.Name,
		"app.kubernetes.io/component":                "model-gateway",
		"runtime.agents.smol-agents.ai/modelgateway": gw.Name,
	}
}

func modelGatewaySelector(gw *amv1.ModelGateway) map[string]string {
	return map[string]string{"runtime.agents.smol-agents.ai/modelgateway": gw.Name}
}

// ModelGatewayEndpoint is the in-cluster base URL agents point harness.http.url at.
func ModelGatewayEndpoint(gw *amv1.ModelGateway) string {
	return "http://" + ModelGatewayName(gw) + "." + gw.Namespace + ".svc:" + itoa(gw.Spec.EffectivePort())
}

// BuildModelGatewayConfigMap renders the gateway's config file into a ConfigMap.
func BuildModelGatewayConfigMap(gw *amv1.ModelGateway) *corev1.ConfigMap {
	p := profileFor(gw.Spec.Provider)
	data := map[string]string{p.configIn: gw.Spec.Config}
	if uiSharedSecret(gw) {
		data[modelGatewayUINginxConf] = renderUINginxConf(gw)
	}
	return &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: ModelGatewayName(gw), Namespace: gw.Namespace, Labels: modelGatewayLabels(gw)},
		Data:       data,
	}
}

// renderUINginxConf is the auth-proxy server block: basic-auth (htpasswd) in
// front of a loopback proxy to the gateway port. WebSocket upgrade is preserved
// for dashboards that stream.
func renderUINginxConf(gw *amv1.ModelGateway) string {
	return fmt.Sprintf(`server {
    listen %d;
    location / {
        auth_basic "smol-agents gateway";
        auth_basic_user_file /etc/nginx/auth/.htpasswd;
        proxy_pass http://127.0.0.1:%d;
        # The dashboard's host_header_middleware (DNS-rebind defense) only accepts
        # loopback Host values when bound to loopback — forward "localhost", not $host.
        proxy_set_header Host localhost;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_read_timeout 3600s;
    }
}
`, gw.Spec.EffectiveUIPort(), uiUpstreamPort(gw))
}

// BuildModelGatewayDeployment renders the hardened gateway Deployment. class is
// the resolved RuntimeClass ("" / "runc" = cluster default); the caller resolves
// it fail-closed via resolveSandbox.
func BuildModelGatewayDeployment(gw *amv1.ModelGateway, class string) *appsv1.Deployment {
	p := profileFor(gw.Spec.Provider)
	port := gw.Spec.EffectivePort()
	lbls := modelGatewayLabels(gw)
	replicas := int32(1)
	if gw.Spec.Replicas != nil {
		replicas = *gw.Spec.Replicas
	}

	env := p.stdEnv(port)
	if uiEnabled(gw) {
		env = append(env, p.uiEnableEnv...) // turn the provider's UI/dashboard on
	}
	env = append(env, modelGatewayUserEnv(gw)...) // user env wins (last)

	dep := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: ModelGatewayName(gw), Namespace: gw.Namespace, Labels: lbls},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: modelGatewaySelector(gw)},
			// Single writable data dir → one replica at a time.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lbls},
				Spec: corev1.PodSpec{
					// fsGroup makes the data dir writable by the gateway user;
					// seccomp + dropped caps + no-privesc are the cheap hardening
					// the RCE container tolerates. Real isolation is the
					// RuntimeClass (kata) + the egress floor + NetworkPolicies.
					SecurityContext: &corev1.PodSecurityContext{
						FSGroup:        ptr.To(p.fsGroup),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					InitContainers: []corev1.Container{{
						Name:            "seed-config",
						Image:           "busybox:1.37",
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command:         []string{"sh", "-c", "cp " + modelGatewayConfigPath + "/" + p.configIn + " " + p.dataDir + "/" + p.configIn + " && chmod 0644 " + p.dataDir + "/" + p.configIn},
						SecurityContext: hardenedGatewaySecurityContext(),
						VolumeMounts: []corev1.VolumeMount{
							{Name: modelGatewayConfigVolume, MountPath: modelGatewayConfigPath},
							{Name: modelGatewayDataVolume, MountPath: p.dataDir},
						},
					}},
					Containers: []corev1.Container{{
						Name:            "gateway",
						Image:           gw.Spec.Image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Args:            p.args,
						Env:             env,
						Ports:           []corev1.ContainerPort{{Name: "api", ContainerPort: port, Protocol: corev1.ProtocolTCP}},
						// First boot pulls a large image + runs setup — be patient.
						StartupProbe: &corev1.Probe{
							ProbeHandler:     corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(int(port))}},
							PeriodSeconds:    10,
							FailureThreshold: 60, // up to ~10 min
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler:  corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(int(port))}},
							PeriodSeconds: 10,
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("512Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("2Gi")},
						},
						SecurityContext: gatewayContainerSecurityContext(p),
						VolumeMounts:    []corev1.VolumeMount{{Name: modelGatewayDataVolume, MountPath: p.dataDir}},
					}},
					Volumes: []corev1.Volume{
						{Name: modelGatewayConfigVolume, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: ModelGatewayName(gw)}}}},
						{Name: modelGatewayDataVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
	if class != "" && class != "runc" {
		dep.Spec.Template.Spec.RuntimeClassName = ptr.To(class)
	}
	if uiEnabled(gw) {
		dep.Spec.Template.Spec.Containers = append(dep.Spec.Template.Spec.Containers, gatewayUISidecar(gw))
		dep.Spec.Template.Spec.Volumes = append(dep.Spec.Template.Spec.Volumes, uiVolumes(gw)...)
	}
	return dep
}

// gatewayUISidecar renders the auth-front sidecar for the selected mode. Both
// front the loopback dashboard on the UI port and run fully hardened (drop ALL
// caps, no privesc, nonroot — they bind a >1024 port).
func gatewayUISidecar(gw *amv1.ModelGateway) corev1.Container {
	if uiOIDCProxy(gw) {
		return gatewayOIDCProxySidecar(gw)
	}
	return gatewayNginxSidecar(gw)
}

// gatewayNginxSidecar is the sharedSecret (basic-auth) front: nginx authenticates
// against the mounted htpasswd and proxies the dashboard on loopback.
func gatewayNginxSidecar(gw *amv1.ModelGateway) corev1.Container {
	uiPort := gw.Spec.EffectiveUIPort()
	return corev1.Container{
		Name:            "ui-auth",
		Image:           gatewayUIProxyImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Ports:           []corev1.ContainerPort{{Name: "ui", ContainerPort: uiPort, Protocol: corev1.ProtocolTCP}},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(int(uiPort))}},
			PeriodSeconds: 10,
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("25m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
		},
		SecurityContext: hardenedGatewaySecurityContext(),
		VolumeMounts: []corev1.VolumeMount{
			{Name: modelGatewayUIConfigVolume, MountPath: "/etc/nginx/conf.d"},
			{Name: modelGatewayUIAuthVolume, MountPath: "/etc/nginx/auth", ReadOnly: true},
		},
	}
}

// gatewayOIDCProxySidecar is the oidcProxy front: oauth2-proxy authenticates
// humans against an OIDC IdP (cookie session) and proxies the dashboard on
// loopback. oauth2-proxy defaults to --pass-host-header=true, which forwards the
// browser's Host (e.g. hermes.example.com) to the upstream — the Hermes dashboard's
// DNS-rebind defence rejects that with 400 ("Invalid Host header"). We set
// --pass-host-header=false so the upstream Host becomes the upstream URL's host
// (127.0.0.1:<port>, port-stripped to 127.0.0.1), satisfying the loopback check.
func gatewayOIDCProxySidecar(gw *amv1.ModelGateway) corev1.Container {
	uiPort := gw.Spec.EffectiveUIPort()
	o := gw.Spec.UI.Auth.OIDC
	emailDomain := o.EmailDomain
	if emailDomain == "" {
		emailDomain = "*"
	}
	args := []string{
		"--provider=oidc",
		"--oidc-issuer-url=" + o.Issuer,
		"--client-id=" + o.ClientID,
		"--redirect-url=" + o.RedirectURL,
		"--upstream=http://127.0.0.1:" + itoa(uiUpstreamPort(gw)),
		"--http-address=0.0.0.0:" + itoa(uiPort),
		"--email-domain=" + emailDomain,
		"--cookie-secure=true",
		"--reverse-proxy=true",
		// Rewrite the upstream Host to 127.0.0.1 so the loopback-bound dashboard's
		// DNS-rebind Host check accepts it (default true would pass the browser Host).
		"--pass-host-header=false",
		"--skip-provider-button=true",
		"--scope=openid email profile",
	}
	// Pinned endpoints (all-or-nothing, validated): skip discovery so the
	// back-channel can hit in-cluster HTTP URLs and never trust the issuer's TLS.
	if o.LoginURL != "" && o.RedeemURL != "" && o.JWKSURL != "" {
		args = append(args,
			"--skip-oidc-discovery=true",
			"--login-url="+o.LoginURL,
			"--redeem-url="+o.RedeemURL,
			"--oidc-jwks-url="+o.JWKSURL,
		)
	}
	return corev1.Container{
		Name:            "ui-auth",
		Image:           gatewayUIOIDCProxyImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            args,
		Env: []corev1.EnvVar{
			{Name: "OAUTH2_PROXY_CLIENT_SECRET", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: o.SecretRef.SecretName}, Key: "client-secret"}}},
			{Name: "OAUTH2_PROXY_COOKIE_SECRET", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: o.SecretRef.SecretName}, Key: "cookie-secret"}}},
		},
		Ports: []corev1.ContainerPort{{Name: "ui", ContainerPort: uiPort, Protocol: corev1.ProtocolTCP}},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/ping", Port: intstr.FromInt(int(uiPort))}},
			PeriodSeconds: 10,
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("25m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
		},
		SecurityContext: hardenedGatewaySecurityContext(),
	}
}

// uiVolumes mounts the nginx server block + htpasswd for the sharedSecret front.
// oidcProxy (oauth2-proxy) is configured purely by args/env — no volumes.
func uiVolumes(gw *amv1.ModelGateway) []corev1.Volume {
	if !uiSharedSecret(gw) {
		return nil
	}
	return []corev1.Volume{
		{Name: modelGatewayUIConfigVolume, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: ModelGatewayName(gw)},
			Items:                []corev1.KeyToPath{{Key: modelGatewayUINginxConf, Path: "default.conf"}},
		}}},
		{Name: modelGatewayUIAuthVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: gw.Spec.UI.Auth.SecretRef.SecretName,
			Items:      []corev1.KeyToPath{{Key: uiAuthKey(gw), Path: ".htpasswd"}},
		}}},
	}
}

// hardenedGatewaySecurityContext is the cheap container hardening a helper (the
// busybox config-seed init) tolerates: no privilege escalation, all caps dropped.
func hardenedGatewaySecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// gatewayContainerSecurityContext hardens the RCE gateway container: drop ALL caps
// and add back only the ones the image's init system needs to drop to its own
// unprivileged user (profile.initCaps). The root filesystem stays writable (the
// agent's tools write files) and the uid is the image default; the real isolation
// is the RuntimeClass (kata) + egress floor + NetworkPolicies. With no initCaps it
// is identical to the fully-dropped helper context.
func gatewayContainerSecurityContext(p gatewayProfile) *corev1.SecurityContext {
	if len(p.initCaps) == 0 {
		return hardenedGatewaySecurityContext()
	}
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(p.allowPrivEsc),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: p.initCaps},
	}
}

// BuildModelGatewayService renders the ClusterIP Service agents dial.
func BuildModelGatewayService(gw *amv1.ModelGateway) *corev1.Service {
	port := gw.Spec.EffectivePort()
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: ModelGatewayName(gw), Namespace: gw.Namespace, Labels: modelGatewayLabels(gw)},
		Spec: corev1.ServiceSpec{
			Selector: modelGatewaySelector(gw),
			Ports:    []corev1.ServicePort{{Name: "api", Port: port, TargetPort: intstr.FromInt(int(port)), Protocol: corev1.ProtocolTCP}},
		},
	}
}

// BuildModelGatewayUIService renders the dedicated human-facing UI Service that
// fronts the auth-proxy sidecar (rendered only when spec.ui.expose=true). It is a
// surface distinct from the machine API Service — port-forward this to reach the
// authenticated dashboard.
func BuildModelGatewayUIService(gw *amv1.ModelGateway) *corev1.Service {
	uiPort := gw.Spec.EffectiveUIPort()
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: ModelGatewayName(gw) + "-ui", Namespace: gw.Namespace, Labels: modelGatewayLabels(gw)},
		Spec: corev1.ServiceSpec{
			Selector: modelGatewaySelector(gw),
			Ports:    []corev1.ServicePort{{Name: "ui", Port: uiPort, TargetPort: intstr.FromInt(int(uiPort)), Protocol: corev1.ProtocolTCP}},
		},
	}
}

// ModelGatewayUIEndpoint is the in-cluster base URL of the UI Service (empty
// unless the UI is exposed).
func ModelGatewayUIEndpoint(gw *amv1.ModelGateway) string {
	if !uiEnabled(gw) {
		return ""
	}
	return "http://" + ModelGatewayName(gw) + "-ui." + gw.Namespace + ".svc:" + itoa(gw.Spec.EffectiveUIPort())
}

// BuildModelGatewayIngress restricts ingress to the gateway port to pods in the
// gateway's own namespace — the RCE gateway is not reachable cross-namespace. When
// the UI is exposed, the auth-proxy port is allowed on the same in-namespace terms
// (a future ingress controller / oauth2-proxy lives in-namespace; port-forward
// bypasses NetworkPolicy regardless).
func BuildModelGatewayIngress(gw *amv1.ModelGateway) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt(int(gw.Spec.EffectivePort()))
	ports := []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}}
	if uiEnabled(gw) {
		uiPort := intstr.FromInt(int(gw.Spec.EffectiveUIPort()))
		ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &uiPort})
	}
	return &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: ModelGatewayName(gw) + "-ingress", Namespace: gw.Namespace, Labels: modelGatewayLabels(gw)},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: modelGatewaySelector(gw)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": gw.Namespace}},
				}},
				Ports: ports,
			}},
		},
	}
}

// modelGatewayUserEnv converts the CR's HarnessEnvVar list into pod env. A
// secretRef becomes a secretKeyRef (the key defaults to the env var name); a
// literal value passes through.
func modelGatewayUserEnv(gw *amv1.ModelGateway) []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(gw.Spec.Env))
	for _, e := range gw.Spec.Env {
		if e.SecretRef != nil && e.SecretRef.SecretName != "" {
			key := e.SecretRef.Key
			if key == "" {
				key = e.Name
			}
			out = append(out, corev1.EnvVar{Name: e.Name, ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: e.SecretRef.SecretName},
					Key:                  key,
				},
			}})
			continue
		}
		out = append(out, corev1.EnvVar{Name: e.Name, Value: e.Value})
	}
	return out
}
