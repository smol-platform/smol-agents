package builders

import (
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
)

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
	return &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: ModelGatewayName(gw), Namespace: gw.Namespace, Labels: modelGatewayLabels(gw)},
		Data:       map[string]string{p.configIn: gw.Spec.Config},
	}
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

	env := append(p.stdEnv(port), modelGatewayUserEnv(gw)...)

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
	return dep
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

// BuildModelGatewayIngress restricts ingress to the gateway port to pods in the
// gateway's own namespace — the RCE gateway is not reachable cross-namespace.
func BuildModelGatewayIngress(gw *amv1.ModelGateway) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt(int(gw.Spec.EffectivePort()))
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
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
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
