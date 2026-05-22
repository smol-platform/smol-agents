// Package memory provides the operator controller for MemoryStore and
// MemoryRetriever CRDs. It reconciles each MemoryRetriever into:
//   - a memory-worker Deployment (data plane: embed/chunk/index/retrieve)
//   - a memory-mcp Deployment + Service (agent-facing MCP gateway)
//   - a ServiceAccount owned by the retriever
//
// Owner references cascade deletion: removing a MemoryRetriever removes only
// its worker/mcp resources; the shared MemoryStore is left untouched.
//
// Implements R-MEM-CTRL-1, R-MEM-API-3.
package memory

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	resource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

const (
	// workerImage is the placeholder image for the retrieval worker.
	// Overridden by WorkerImageOverride in the reconciler for testing.
	defaultWorkerImage = "docker.io/smol-agents/memory-worker:dev"

	// mcpImage is the placeholder image for the MCP gateway.
	defaultMCPImage = "docker.io/smol-agents/memory-mcp:dev"

	// fieldOwner is the SSA field manager for memory resources.
	fieldOwner = "smol-agents-memory-operator"

	// workerPort is the gRPC port the retrieval worker listens on.
	workerPort = 9090

	// mcpPort is the HTTP port the MCP gateway listens on.
	mcpPort = 8080

	// workerReplicas is the default Deployment replica count.
	workerReplicas = int32(1)
)

// resourceName returns a deterministic name for a resource owned by the
// given MemoryRetriever.
func resourceName(retriever *amv1.MemoryRetriever, suffix string) string {
	return fmt.Sprintf("mr-%s-%s", retriever.Name, suffix)
}

// workerLabels returns labels for memory-worker resources.
func workerLabels(retriever *amv1.MemoryRetriever) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":             "memory-worker",
		"app.kubernetes.io/instance":         retriever.Name,
		"app.kubernetes.io/managed-by":       "smol-agents-operator",
		"runtime.agents.smol-agents.ai/retriever": retriever.Name,
		"runtime.agents.smol-agents.ai/component": "memory-worker",
	}
}

// mcpLabels returns labels for memory-mcp resources.
func mcpLabels(retriever *amv1.MemoryRetriever) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":             "memory-mcp",
		"app.kubernetes.io/instance":         retriever.Name,
		"app.kubernetes.io/managed-by":       "smol-agents-operator",
		"runtime.agents.smol-agents.ai/retriever": retriever.Name,
		"runtime.agents.smol-agents.ai/component": "memory-mcp",
	}
}

// BuildServiceAccount renders the ServiceAccount owned by a MemoryRetriever.
// The worker and mcp Pods run as this ServiceAccount.
func BuildServiceAccount(retriever *amv1.MemoryRetriever) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName(retriever, "sa"),
			Namespace: retriever.Namespace,
			Labels:    workerLabels(retriever),
		},
	}
}

// workerArgs builds the CLI flag list for the memory-worker container.
// Flags are derived from the bound MemoryStore kinds (backend flags) and the
// retriever configuration (embed model, chunking). Broker cred flags are
// passed only by reference (secret name) — never the literal value —
// consistent with R-MEM-SEC-1.
func workerArgs(retriever *amv1.MemoryRetriever, stores []*amv1.MemoryStore) []string {
	args := []string{
		fmt.Sprintf("--retriever-ref=%s/%s", retriever.Namespace, retriever.Name),
		fmt.Sprintf("--grpc-addr=:%d", workerPort),
	}

	// Derive backend-kind flags from the bound stores.
	for _, s := range stores {
		switch s.Spec.Kind {
		case pure.MemoryStoreVector:
			args = append(args,
				fmt.Sprintf("--backend=%s", s.Spec.Driver),
				fmt.Sprintf("--backend-endpoint=%s", s.Spec.Endpoint),
			)
			if s.Spec.Auth != nil && s.Spec.Auth.SecretName != "" {
				// Pass the secret name; the worker resolves creds via broker.
				args = append(args,
					fmt.Sprintf("--backend-auth-secret=%s", s.Spec.Auth.SecretName))
			}
		case pure.MemoryStoreFilesystem:
			args = append(args, "--backend=agentfs")
		}
	}

	if retriever.Spec.ModelProviderRef != "" {
		args = append(args,
			fmt.Sprintf("--model-provider=%s", retriever.Spec.ModelProviderRef))
	}
	if retriever.Spec.Chunking.Size > 0 {
		args = append(args,
			fmt.Sprintf("--chunk-size=%d", retriever.Spec.Chunking.Size))
	}
	if retriever.Spec.Chunking.Overlap > 0 {
		args = append(args,
			fmt.Sprintf("--chunk-overlap=%d", retriever.Spec.Chunking.Overlap))
	}
	if retriever.Spec.Chunking.Strategy != "" {
		args = append(args,
			fmt.Sprintf("--chunk-strategy=%s", retriever.Spec.Chunking.Strategy))
	}
	return args
}

// mcpArgs builds the CLI flag list for the memory-mcp container.
func mcpArgs(retriever *amv1.MemoryRetriever, workerSvcName string) []string {
	return []string{
		fmt.Sprintf("--worker-url=grpc://%s.%s.svc.cluster.local:%d",
			workerSvcName, retriever.Namespace, workerPort),
		fmt.Sprintf("--retriever-ref=%s/%s", retriever.Namespace, retriever.Name),
		fmt.Sprintf("--http-addr=:%d", mcpPort),
		fmt.Sprintf("--top-k=%d", retriever.Spec.TopK),
	}
}

// BuildWorkerDeployment renders the retrieval-worker Deployment for a
// MemoryRetriever. Pure builder: no side effects, no API calls.
//
// The Deployment is keyed by the retriever (owned via controller ref set
// outside this function). It embeds backend flags derived from the list of
// resolved MemoryStore objects (never their raw credentials). The workerImage
// parameter lets callers (and tests) override the container image.
func BuildWorkerDeployment(
	retriever *amv1.MemoryRetriever,
	stores []*amv1.MemoryStore,
	workerImage string,
) *appsv1.Deployment {
	if workerImage == "" {
		workerImage = defaultWorkerImage
	}
	saName := resourceName(retriever, "sa")
	lbls := workerLabels(retriever)

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName(retriever, "worker"),
			Namespace: retriever.Namespace,
			Labels:    lbls,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(workerReplicas),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"runtime.agents.smol-agents.ai/retriever": retriever.Name,
					"runtime.agents.smol-agents.ai/component": "memory-worker",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lbls},
				Spec: corev1.PodSpec{
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
					Containers: []corev1.Container{
						{
							Name:            "memory-worker",
							Image:           workerImage,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Args:            workerArgs(retriever, stores),
							Ports: []corev1.ContainerPort{
								{
									Name:          "grpc",
									ContainerPort: workerPort,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/readyz",
										Port: intstr.FromInt(workerPort),
									},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       10,
								FailureThreshold:    6,
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/healthz",
										Port: intstr.FromInt(workerPort),
									},
								},
								InitialDelaySeconds: 30,
								PeriodSeconds:       10,
							},
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("1"),
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
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "tmp", MountPath: "/tmp"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "tmp",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
					},
				},
			},
		},
	}
}

// BuildWorkerService renders the headless ClusterIP Service for the
// retrieval worker. The MCP gateway resolves the worker via this Service.
func BuildWorkerService(retriever *amv1.MemoryRetriever) *corev1.Service {
	lbls := workerLabels(retriever)
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName(retriever, "worker"),
			Namespace: retriever.Namespace,
			Labels:    lbls,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"runtime.agents.smol-agents.ai/retriever": retriever.Name,
				"runtime.agents.smol-agents.ai/component": "memory-worker",
			},
			Ports: []corev1.ServicePort{
				{
					Name:       "grpc",
					Port:       workerPort,
					TargetPort: intstr.FromInt(workerPort),
					Protocol:   corev1.ProtocolTCP,
				},
			},
			ClusterIP: "None", // headless: stable DNS, no kube-proxy LB
		},
	}
}

// BuildMCPDeployment renders the memory-mcp gateway Deployment for a
// MemoryRetriever. The gateway is stateless; it proxies MCP calls to the
// worker over gRPC. The mcpImage parameter lets callers override the image.
func BuildMCPDeployment(
	retriever *amv1.MemoryRetriever,
	mcpImage string,
) *appsv1.Deployment {
	if mcpImage == "" {
		mcpImage = defaultMCPImage
	}
	saName := resourceName(retriever, "sa")
	workerSvcName := resourceName(retriever, "worker")
	lbls := mcpLabels(retriever)

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName(retriever, "mcp"),
			Namespace: retriever.Namespace,
			Labels:    lbls,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(workerReplicas),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"runtime.agents.smol-agents.ai/retriever": retriever.Name,
					"runtime.agents.smol-agents.ai/component": "memory-mcp",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lbls},
				Spec: corev1.PodSpec{
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
					Containers: []corev1.Container{
						{
							Name:            "memory-mcp",
							Image:           mcpImage,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Args:            mcpArgs(retriever, workerSvcName),
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: mcpPort,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/readyz",
										Port: intstr.FromInt(mcpPort),
									},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       10,
								FailureThreshold:    6,
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/healthz",
										Port: intstr.FromInt(mcpPort),
									},
								},
								InitialDelaySeconds: 30,
								PeriodSeconds:       10,
							},
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("50m"),
									corev1.ResourceMemory: resource.MustParse("64Mi"),
								},
							},
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: ptr.To(false),
								ReadOnlyRootFilesystem:   ptr.To(true),
								RunAsNonRoot:             ptr.To(true),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "tmp", MountPath: "/tmp"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "tmp",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
					},
				},
			},
		},
	}
}

// BuildMCPService renders the ClusterIP Service for the memory-mcp gateway.
// Agents reach the MCP gateway through their agentnet sidecar via this Service.
func BuildMCPService(retriever *amv1.MemoryRetriever) *corev1.Service {
	lbls := mcpLabels(retriever)
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName(retriever, "mcp"),
			Namespace: retriever.Namespace,
			Labels:    lbls,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"runtime.agents.smol-agents.ai/retriever": retriever.Name,
				"runtime.agents.smol-agents.ai/component": "memory-mcp",
			},
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Port:       int32(mcpPort),
					TargetPort: intstr.FromInt(mcpPort),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}
