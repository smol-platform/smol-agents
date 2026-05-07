package builders

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	resource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	v1 "github.com/stigen/knative-agents/operator/api/v1"
)

// LoaderPreset captures the per-distro defaults we ship in the Helm
// chart. Keeping them here lets the operator's Platform reconciler
// reproduce the chart's behaviour without duplicating values.yaml.
type LoaderPreset struct {
	Capability    string // "privileged" | "minimal"
	HostPathBPFFS string
	MountDebugFS  bool
	MountModules  bool
}

// LoaderPresets is the canonical preset table. Mirrors
// deploy/helm/templates/ebpf-loader-presets.yaml.
var LoaderPresets = map[string]LoaderPreset{
	"generic":          {Capability: "privileged", HostPathBPFFS: "/sys/fs/bpf", MountDebugFS: true, MountModules: true},
	"gke-cos":          {Capability: "privileged", HostPathBPFFS: "/sys/fs/bpf", MountDebugFS: false, MountModules: true},
	"eks-bottlerocket": {Capability: "minimal", HostPathBPFFS: "/sys/fs/bpf", MountDebugFS: true, MountModules: true},
	"aks-mariner":      {Capability: "minimal", HostPathBPFFS: "/sys/fs/bpf", MountDebugFS: true, MountModules: true},
	"k3s":              {Capability: "privileged", HostPathBPFFS: "/sys/fs/bpf", MountDebugFS: true, MountModules: true},
	"openshift":        {Capability: "privileged", HostPathBPFFS: "/sys/fs/bpf", MountDebugFS: true, MountModules: true},
	"talos":            {Capability: "minimal", HostPathBPFFS: "/sys/fs/bpf", MountDebugFS: false, MountModules: false},
}

// BuildEBPFLoaderServiceAccount returns the SA the DaemonSet runs as.
func BuildEBPFLoaderServiceAccount(ns string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{Name: "ebpf-loader", Namespace: ns},
	}
}

// BuildEBPFLoaderClusterRole grants the (optional) node read perms.
func BuildEBPFLoaderClusterRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: "ebpf-loader"},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "list", "watch"}},
		},
	}
}

// BuildEBPFLoaderConfigMap renders the loader's config from the
// platform CR's ebpfLoader spec.
func BuildEBPFLoaderConfigMap(p *v1.KnativeAgentPlatform, ns string) *corev1.ConfigMap {
	pinRoot := p.Spec.EBPFLoader.PinRoot
	if pinRoot == "" {
		pinRoot = "/sys/fs/bpf/knative-agents"
	}
	yaml := "" +
		"pinRoot: " + pinRoot + "\n" +
		"objectsDir: /usr/share/knative-agents/bpf\n" +
		"programs:\n  - syscalls\n  - network\n" +
		"mountBPFFS: true\n" +
		"healthAddr: \"0.0.0.0:8081\"\n"
	return &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "ebpf-loader-config", Namespace: ns},
		Data:       map[string]string{"config.yaml": yaml},
	}
}

// BuildEBPFLoaderDaemonSet renders the privileged DaemonSet that loads
// CO-RE programs and pins maps to bpffs. Mirrors the chart's
// deploy/helm/templates/ebpf-loader-daemonset.yaml.
//
// `presetName` selects per-distro defaults from LoaderPresets.
func BuildEBPFLoaderDaemonSet(p *v1.KnativeAgentPlatform, ns, presetName string) *appsv1.DaemonSet {
	preset, ok := LoaderPresets[presetName]
	if !ok {
		preset = LoaderPresets["generic"]
	}

	image := p.Spec.EBPFLoader.Image
	if image == "" {
		image = "knative-agents/ebpf-loader:0.1.0"
	}

	hostPathDir := corev1.HostPathDirectoryOrCreate
	hostPathDirOnly := corev1.HostPathDirectory

	volumes := []corev1.Volume{
		{Name: "config", VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "ebpf-loader-config"}},
		}},
		{Name: "bpffs", VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: preset.HostPathBPFFS, Type: &hostPathDir},
		}},
		{Name: "kernel-btf", VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: "/sys/kernel/btf", Type: &hostPathDir},
		}},
		{Name: "kernel-tracing", VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: "/sys/kernel/tracing", Type: &hostPathDir},
		}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	if preset.MountDebugFS {
		volumes = append(volumes, corev1.Volume{
			Name: "kernel-debug",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/sys/kernel/debug", Type: &hostPathDir},
			},
		})
	}
	if preset.MountModules {
		volumes = append(volumes, corev1.Volume{
			Name: "modules",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/lib/modules", Type: &hostPathDirOnly},
			},
		})
	}

	mounts := []corev1.VolumeMount{
		{Name: "config", MountPath: "/etc/ebpf-loader", ReadOnly: true},
		{Name: "bpffs", MountPath: preset.HostPathBPFFS, MountPropagation: ptr.To(corev1.MountPropagationBidirectional)},
		{Name: "kernel-btf", MountPath: "/sys/kernel/btf", ReadOnly: true},
		{Name: "kernel-tracing", MountPath: "/sys/kernel/tracing", ReadOnly: true},
		{Name: "tmp", MountPath: "/tmp"},
	}
	if preset.MountDebugFS {
		mounts = append(mounts, corev1.VolumeMount{Name: "kernel-debug", MountPath: "/sys/kernel/debug", ReadOnly: true})
	}
	if preset.MountModules {
		mounts = append(mounts, corev1.VolumeMount{Name: "modules", MountPath: "/lib/modules", ReadOnly: true})
	}

	sec := &corev1.SecurityContext{}
	if preset.Capability == "minimal" {
		sec.Privileged = ptr.To(false)
		sec.AllowPrivilegeEscalation = ptr.To(false)
		sec.RunAsUser = ptr.To[int64](0)
		sec.ReadOnlyRootFilesystem = ptr.To(true)
		sec.Capabilities = &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
			Add:  []corev1.Capability{"BPF", "PERFMON", "NET_ADMIN"},
		}
	} else {
		sec.Privileged = ptr.To(true)
		sec.AllowPrivilegeEscalation = ptr.To(true)
		sec.RunAsUser = ptr.To[int64](0)
		sec.ReadOnlyRootFilesystem = ptr.To(true)
	}

	loaderLabels := map[string]string{
		"app.kubernetes.io/name":      "knative-agents",
		"app.kubernetes.io/component": "ebpf-loader",
	}

	return &appsv1.DaemonSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
		ObjectMeta: metav1.ObjectMeta{Name: "ebpf-loader", Namespace: ns, Labels: loaderLabels},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: loaderLabels},
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{
				Type: appsv1.RollingUpdateDaemonSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDaemonSet{
					MaxUnavailable: ptr.To(intstr.FromInt(1)),
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: loaderLabels},
				Spec: corev1.PodSpec{
					PriorityClassName:  "system-node-critical",
					ServiceAccountName: "ebpf-loader",
					Tolerations:        []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{{
									MatchExpressions: []corev1.NodeSelectorRequirement{{
										Key:      "kubernetes.io/os",
										Operator: corev1.NodeSelectorOpIn,
										Values:   []string{"linux"},
									}},
								}},
							},
						},
					},
					HostPID:     true,
					HostNetwork: false,
					InitContainers: []corev1.Container{{
						Name:    "init-bpffs",
						Image:   image,
						Command: []string{"/bin/sh", "-c", "mountpoint -q " + preset.HostPathBPFFS + " || mount -t bpf bpf " + preset.HostPathBPFFS},
						SecurityContext: &corev1.SecurityContext{
							Privileged: ptr.To(true),
							RunAsUser:  ptr.To[int64](0),
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:             "bpffs",
							MountPath:        preset.HostPathBPFFS,
							MountPropagation: ptr.To(corev1.MountPropagationBidirectional),
						}},
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("50m"),
								corev1.ResourceMemory: resource.MustParse("32Mi"),
							},
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("10m"),
								corev1.ResourceMemory: resource.MustParse("16Mi"),
							},
						},
					}},
					Containers: []corev1.Container{{
						Name:            "ebpf-loader",
						Image:           image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Args:            []string{"--config=/etc/ebpf-loader/config.yaml"},
						Env: []corev1.EnvVar{{
							Name:      "NODE_NAME",
							ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}},
						}},
						Ports: []corev1.ContainerPort{{Name: "health", ContainerPort: 8081, Protocol: corev1.ProtocolTCP}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt(8081)}},
							InitialDelaySeconds: 2, PeriodSeconds: 5, FailureThreshold: 12,
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt(8081)}},
							InitialDelaySeconds: 30, PeriodSeconds: 10,
						},
						SecurityContext: sec,
						VolumeMounts:    mounts,
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("200m"),
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("50m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
						},
					}},
					Volumes:                       volumes,
					TerminationGracePeriodSeconds: ptr.To[int64](30),
				},
			},
		},
	}
}
