package builders

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	resource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	v1 "github.com/stigen/knative-agents/operator/api/v1"
)

// AgentImage returns the container image to use for the agent. Tenants
// can override via spec.image; otherwise we use a stable default.
func AgentImage(cr *v1.KnativeAgent) string {
	if cr.Spec.Image != "" {
		return cr.Spec.Image
	}
	return "knative-agents/agent:0.1.0"
}

// SecretProxyImage is the image for the broker sidecar.
func SecretProxyImage() string {
	return "knative-agents/secret-proxy:0.1.0"
}

// BuildAgentPodSpec is the canonical Pod template shared by Deployment,
// StatefulSet, and Knative Service. It composes:
//
//   - the agent container (image, args, ports, probes, volumeMounts);
//   - the secret-proxy sidecar (kloak-style broker over UDS);
//   - the SPIRE workload-API CSI volume (read-only);
//   - the secret-broker emptyDir shared between the two containers;
//   - the agent ConfigMap mount;
//   - the sandbox RuntimeClassName.
func BuildAgentPodSpec(cr *v1.KnativeAgent) corev1.PodSpec {
	rc := cr.Spec.Features.Sandbox.RuntimeClass
	if rc == "" {
		rc = "kata-fc"
	}
	saName := cr.Name + "-agent"

	agentContainer := corev1.Container{
		Name:            "agent",
		Image:           AgentImage(cr),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            []string{"--config=/etc/knative-agents/agent.yaml"},
		Ports: []corev1.ContainerPort{
			{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
			{Name: "private-mtls", ContainerPort: 8443, Protocol: corev1.ProtocolTCP},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt(8080)},
			},
			InitialDelaySeconds: 1,
			PeriodSeconds:       3,
			FailureThreshold:    20,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt(8080)},
			},
			InitialDelaySeconds: 30,
			PeriodSeconds:       10,
		},
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			RunAsNonRoot:             ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: agentVolumeMounts(),
	}

	containers := []corev1.Container{agentContainer}
	if cr.Spec.Features.Secrets.Enabled {
		containers = append(containers, secretProxyContainer())
	}

	return corev1.PodSpec{
		RuntimeClassName:   ptr.To(rc),
		ServiceAccountName: saName,
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: ptr.To(true),
			RunAsUser:    ptr.To[int64](65532),
			RunAsGroup:   ptr.To[int64](65532),
			FSGroup:      ptr.To[int64](65532),
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
		},
		Containers: containers,
		Volumes:    agentVolumes(cr),
	}
}

func agentVolumeMounts() []corev1.VolumeMount {
	return []corev1.VolumeMount{
		{Name: "spire-agent-socket", MountPath: "/run/spire/agent-sockets", ReadOnly: true},
		{Name: "secret-broker", MountPath: "/run/secret-broker"},
		{Name: "config", MountPath: "/etc/knative-agents", ReadOnly: true},
		{Name: "tmp", MountPath: "/tmp"},
	}
}

func secretProxyContainer() corev1.Container {
	return corev1.Container{
		Name:            "secret-proxy",
		Image:           SecretProxyImage(),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            []string{"--config=/etc/secret-proxy/config.yaml"},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			RunAsNonRoot:             ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("200m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "spire-agent-socket", MountPath: "/run/spire/agent-sockets", ReadOnly: true},
			{Name: "secret-broker", MountPath: "/run/secret-broker"},
			{Name: "config", MountPath: "/etc/secret-proxy", ReadOnly: true},
		},
	}
}

func agentVolumes(cr *v1.KnativeAgent) []corev1.Volume {
	return []corev1.Volume{
		{
			Name: "spire-agent-socket",
			VolumeSource: corev1.VolumeSource{
				CSI: &corev1.CSIVolumeSource{
					Driver:   "csi.spiffe.io",
					ReadOnly: ptr.To(true),
				},
			},
		},
		{Name: "secret-broker", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{
			Name: "config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: cr.Name + "-config"},
				},
			},
		},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
}

// BuildDeployment renders a stateless Deployment for the agent.
// Implements R-DEP-2 (Deployment mode).
func BuildDeployment(cr *v1.KnativeAgent) *appsv1.Deployment {
	replicas := cr.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cr.Name,
			Namespace: cr.Namespace,
			Labels:    Labels(cr),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: Selector(cr)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: Labels(cr)},
				Spec:       BuildAgentPodSpec(cr),
			},
		},
	}
}

// BuildStatefulSet renders a StatefulSet for the agent. Implements
// R-DEP-2 (StatefulSet mode); adds a 1Gi state PVC template.
func BuildStatefulSet(cr *v1.KnativeAgent) *appsv1.StatefulSet {
	replicas := cr.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}
	return &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cr.Name,
			Namespace: cr.Namespace,
			Labels:    Labels(cr),
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: cr.Name,
			Replicas:    ptr.To(replicas),
			Selector:    &metav1.LabelSelector{MatchLabels: Selector(cr)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: Labels(cr)},
				Spec:       BuildAgentPodSpec(cr),
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "state"},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("1Gi"),
							},
						},
					},
				},
			},
		},
	}
}

// KnativeServiceGVK is the Knative Serving Service group/version/kind.
var KnativeServiceGVK = schema.GroupVersionKind{
	Group:   "serving.knative.dev",
	Version: "v1",
	Kind:    "Service",
}

// BuildKnativeService renders a Knative Service as Unstructured so we
// don't take a build-time dep on knative.dev/serving APIs. Implements
// R-DEP-1.
func BuildKnativeService(cr *v1.KnativeAgent) *unstructured.Unstructured {
	pod := BuildAgentPodSpec(cr)

	// Knative wants its own minimal pod-shape; reuse our spec.
	containers := make([]any, 0, len(pod.Containers))
	for _, c := range pod.Containers {
		containers = append(containers, containerToMap(c))
	}
	volumes := make([]any, 0, len(pod.Volumes))
	for _, v := range pod.Volumes {
		volumes = append(volumes, volumeToMap(v))
	}

	min := int32(0)
	max := int32(50)
	if cr.Spec.Features.Knative.MinScale > 0 {
		min = cr.Spec.Features.Knative.MinScale
	}
	if cr.Spec.Features.Knative.MaxScale > 0 {
		max = cr.Spec.Features.Knative.MaxScale
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(KnativeServiceGVK)
	u.SetName(cr.Name)
	u.SetNamespace(cr.Namespace)
	u.SetLabels(Labels(cr))
	u.Object["spec"] = map[string]any{
		"template": map[string]any{
			"metadata": map[string]any{
				"labels": Selector(cr),
				"annotations": map[string]any{
					"autoscaling.knative.dev/min-scale": fmt.Sprintf("%d", min),
					"autoscaling.knative.dev/max-scale": fmt.Sprintf("%d", max),
				},
			},
			"spec": map[string]any{
				"runtimeClassName":   *pod.RuntimeClassName,
				"serviceAccountName": pod.ServiceAccountName,
				"containers":         containers,
				"volumes":            volumes,
			},
		},
	}
	return u
}

// containerToMap is a minimal serialiser; we only emit the fields
// Knative + Kubernetes care about for our agent.
func containerToMap(c corev1.Container) map[string]any {
	out := map[string]any{
		"name":  c.Name,
		"image": c.Image,
	}
	if len(c.Args) > 0 {
		out["args"] = toStringSlice(c.Args)
	}
	if len(c.Ports) > 0 {
		ports := make([]any, 0, len(c.Ports))
		for _, p := range c.Ports {
			ports = append(ports, map[string]any{
				"name":          p.Name,
				"containerPort": int64(p.ContainerPort),
				"protocol":      string(p.Protocol),
			})
		}
		out["ports"] = ports
	}
	if len(c.VolumeMounts) > 0 {
		mounts := make([]any, 0, len(c.VolumeMounts))
		for _, vm := range c.VolumeMounts {
			m := map[string]any{"name": vm.Name, "mountPath": vm.MountPath}
			if vm.ReadOnly {
				m["readOnly"] = true
			}
			mounts = append(mounts, m)
		}
		out["volumeMounts"] = mounts
	}
	return out
}

func volumeToMap(v corev1.Volume) map[string]any {
	out := map[string]any{"name": v.Name}
	switch {
	case v.EmptyDir != nil:
		out["emptyDir"] = map[string]any{}
	case v.ConfigMap != nil:
		out["configMap"] = map[string]any{"name": v.ConfigMap.Name}
	case v.CSI != nil:
		csi := map[string]any{"driver": v.CSI.Driver}
		if v.CSI.ReadOnly != nil && *v.CSI.ReadOnly {
			csi["readOnly"] = true
		}
		out["csi"] = csi
	}
	return out
}

func toStringSlice(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// NodePlacement binds an agent's pod to its AgentNodePool: the pool's node
// label (for nodeAffinity) and the isolation taint it must tolerate. The
// controller resolves it (auto-match by isolation) and applies it to every
// workload kind so kata pods land only on kata-capable nodes. R-PROV-2.
type NodePlacement struct {
	PoolName  string
	Isolation string
}

// DoNotDisruptAnnotation tells Karpenter never to voluntarily disrupt the
// node running this pod — a live Firecracker microVM must not be
// consolidated out from under running work. R-PROV-5.
const DoNotDisruptAnnotation = "karpenter.sh/do-not-disrupt"

func placementNodeAffinity(p NodePlacement) *corev1.NodeAffinity {
	return &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key:      PoolLabelKey,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{p.PoolName},
				}},
			}},
		},
	}
}

func placementToleration(p NodePlacement) corev1.Toleration {
	return corev1.Toleration{
		Key:      IsolationTaintKey,
		Operator: corev1.TolerationOpEqual,
		Value:    p.Isolation,
		Effect:   corev1.TaintEffectNoSchedule,
	}
}

// ApplyPodTemplatePlacement binds a typed pod template (Deployment /
// StatefulSet) to its node pool. No-op when PoolName is empty.
func ApplyPodTemplatePlacement(tpl *corev1.PodTemplateSpec, p NodePlacement) {
	if p.PoolName == "" {
		return
	}
	if tpl.Spec.Affinity == nil {
		tpl.Spec.Affinity = &corev1.Affinity{}
	}
	tpl.Spec.Affinity.NodeAffinity = placementNodeAffinity(p)
	tpl.Spec.Tolerations = append(tpl.Spec.Tolerations, placementToleration(p))
	if tpl.ObjectMeta.Annotations == nil {
		tpl.ObjectMeta.Annotations = map[string]string{}
	}
	tpl.ObjectMeta.Annotations[DoNotDisruptAnnotation] = "true"
}

// ApplyKnativePlacement binds a Knative Service revision template to its
// node pool. Requires the Knative podspec-affinity / -tolerations feature
// flags (the operator verifies them before relying on this). No-op when
// PoolName is empty.
func ApplyKnativePlacement(u *unstructured.Unstructured, p NodePlacement) {
	if p.PoolName == "" {
		return
	}
	spec, _ := u.Object["spec"].(map[string]any)
	if spec == nil {
		return
	}
	tpl, _ := spec["template"].(map[string]any)
	if tpl == nil {
		return
	}
	tplSpec, _ := tpl["spec"].(map[string]any)
	if tplSpec == nil {
		tplSpec = map[string]any{}
		tpl["spec"] = tplSpec
	}
	tplSpec["affinity"] = map[string]any{
		"nodeAffinity": map[string]any{
			"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{
				"nodeSelectorTerms": []any{map[string]any{
					"matchExpressions": []any{map[string]any{
						"key":      PoolLabelKey,
						"operator": "In",
						"values":   []any{p.PoolName},
					}},
				}},
			},
		},
	}
	tplSpec["tolerations"] = []any{map[string]any{
		"key":      IsolationTaintKey,
		"operator": "Equal",
		"value":    p.Isolation,
		"effect":   "NoSchedule",
	}}
	meta, _ := tpl["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		tpl["metadata"] = meta
	}
	ann, _ := meta["annotations"].(map[string]any)
	if ann == nil {
		ann = map[string]any{}
		meta["annotations"] = ann
	}
	ann[DoNotDisruptAnnotation] = "true"
}
