package v1

import (
	"errors"
	"fmt"
	"strings"
)

// MemoryStoreKind discriminates the backend modality.
type MemoryStoreKind string

const (
	MemoryStoreVector     MemoryStoreKind = "vector"
	MemoryStoreGraph      MemoryStoreKind = "graph"
	MemoryStoreKV         MemoryStoreKind = "kv"
	MemoryStoreEventLog   MemoryStoreKind = "eventlog"
	MemoryStoreFilesystem MemoryStoreKind = "filesystem"
)

// Valid returns true if k is a known MemoryStoreKind.
func (k MemoryStoreKind) Valid() bool {
	switch k {
	case MemoryStoreVector, MemoryStoreGraph, MemoryStoreKV, MemoryStoreEventLog, MemoryStoreFilesystem:
		return true
	}
	return false
}

// MemoryStoreDriver names the concrete driver for a MemoryStore kind.
// Not all driver/kind combinations are valid; see ValidateMemoryStore.
type MemoryStoreDriver string

const (
	MemoryDriverPgVector MemoryStoreDriver = "pgvector"
	MemoryDriverQdrant   MemoryStoreDriver = "qdrant"
	MemoryDriverNeo4j    MemoryStoreDriver = "neo4j"
	MemoryDriverRedis    MemoryStoreDriver = "redis"
	MemoryDriverAgentFS  MemoryStoreDriver = "agentfs"
)

// validDriversForKind maps each MemoryStoreKind to its allowed drivers.
var validDriversForKind = map[MemoryStoreKind][]MemoryStoreDriver{
	MemoryStoreVector:     {MemoryDriverPgVector, MemoryDriverQdrant},
	MemoryStoreGraph:      {MemoryDriverNeo4j},
	MemoryStoreKV:         {MemoryDriverRedis},
	MemoryStoreEventLog:   {MemoryDriverRedis},
	MemoryStoreFilesystem: {MemoryDriverAgentFS},
}

// driversRequiringAuth lists drivers that require a non-nil AuthRef.
var driversRequiringAuth = map[MemoryStoreDriver]bool{
	MemoryDriverPgVector: true,
	MemoryDriverQdrant:   true,
	MemoryDriverNeo4j:    true,
	MemoryDriverRedis:    true,
	// agentfs uses broker-resolved S3 creds declared in AgentFSSpec.Backup.S3.CredentialsRef
}

// TenancyModel controls how cross-tenant isolation is enforced in the backend.
type TenancyModel string

const (
	TenancyShared    TenancyModel = "shared"
	TenancyDedicated TenancyModel = "dedicated"
)

// Valid returns true if t is a known TenancyModel.
func (t TenancyModel) Valid() bool {
	return t == TenancyShared || t == TenancyDedicated
}

// TenancySpec declares the tenancy model for a MemoryStore.
type TenancySpec struct {
	// Model is required: "shared" (one backend, tenant label key isolates rows/vectors)
	// or "dedicated" (one backend per tenant).
	// +kubebuilder:validation:Enum=shared;dedicated
	Model TenancyModel `json:"model"`

	// TenantLabelKey is the metadata key used to partition records when Model=shared.
	// Required when Model=shared.
	// +optional
	TenantLabelKey string `json:"tenantLabelKey,omitempty"`
}

// MemoryStoreSpec is the canonical CRD shape for a backend declaration.
// Implements R-MEM-API-1.
type MemoryStoreSpec struct {
	// Kind discriminates the backend modality.
	// +kubebuilder:validation:Enum=vector;graph;kv;eventlog;filesystem
	Kind MemoryStoreKind `json:"kind"`

	// Driver selects the concrete implementation within the kind.
	// +kubebuilder:validation:Enum=pgvector;qdrant;neo4j;redis;agentfs
	Driver MemoryStoreDriver `json:"driver"`

	// Endpoint is the backend service address (host:port or URL).
	// Not required for kind=filesystem (the AgentFS sidecar is co-located).
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Auth is a broker-resolved credential reference. Required for most
	// drivers; never a literal secret. For kind=filesystem the S3 creds
	// are declared in AgentFS.Backup.S3.CredentialsRef.
	// +optional
	Auth *AuthRef `json:"auth,omitempty"`

	// Tenancy declares the isolation model for this store.
	Tenancy TenancySpec `json:"tenancy"`

	// AgentFS configures the Turso AgentFS engine when kind=filesystem.
	// Reuses AgentFSSpec (sizeGiB, mountPath, image, backup, restore) verbatim.
	// Required when kind=filesystem, must be nil otherwise.
	// +optional
	AgentFS *AgentFSSpec `json:"agentfs,omitempty"`
}

// MemoryStoreStatus is reported by the controller.
type MemoryStoreStatus struct {
	// Phase is one of Pending, Ready, Degraded.
	Phase string `json:"phase,omitempty"`

	// BoundWorkers is the count of retrieval workers connected to this store.
	BoundWorkers int32 `json:"boundWorkers,omitempty"`

	// ObservedGeneration is the .metadata.generation the controller last acted on.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions follows the standard Kubernetes condition pattern.
	Conditions []MemoryCondition `json:"conditions,omitempty"`

	// Reason is a machine-readable summary of the current phase.
	Reason string `json:"reason,omitempty"`

	// Message is a human-readable elaboration of Reason.
	Message string `json:"message,omitempty"`
}

// MemoryCondition is a standard Kubernetes condition for memory resources.
type MemoryCondition struct {
	// Type is the condition type (e.g. "Ready", "BackendReachable").
	Type string `json:"type"`

	// Status is "True", "False", or "Unknown".
	Status string `json:"status"`

	// Reason is a CamelCase machine-readable reason string.
	// +optional
	Reason string `json:"reason,omitempty"`

	// Message is a human-readable elaboration.
	// +optional
	Message string `json:"message,omitempty"`

	// LastTransitionTime is when this condition last changed.
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}

// ChunkSpec configures text chunking for a retrieval pipeline.
type ChunkSpec struct {
	// Size is the maximum chunk size in tokens or characters (driver-dependent).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default:=512
	// +optional
	Size int32 `json:"size,omitempty"`

	// Overlap is the number of tokens/characters shared between adjacent chunks.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default:=64
	// +optional
	Overlap int32 `json:"overlap,omitempty"`

	// Strategy selects the chunking algorithm: "fixed" (default) | "sentence" | "paragraph".
	// +kubebuilder:validation:Enum=fixed;sentence;paragraph
	// +kubebuilder:default:=fixed
	// +optional
	Strategy string `json:"strategy,omitempty"`
}

// MountSpec configures filesystem mounting into the agent sandbox (R-MEM-FS-2).
// Only meaningful when the MemoryStore kind=filesystem.
type MountSpec struct {
	// Enabled controls whether the AgentFS volume is mounted into agent pods.
	// When false the filesystem is reachable only over MCP.
	// +kubebuilder:default:=true
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// MountPath overrides the default /var/agentfs mount point.
	// +kubebuilder:default:="/var/agentfs"
	// +optional
	MountPath string `json:"mountPath,omitempty"`
}

// MemoryOperation enumerates the operations grantable in a MemoryGrant.
type MemoryOperation string

const (
	MemoryOpRead   MemoryOperation = "read"
	MemoryOpWrite  MemoryOperation = "write"
	MemoryOpDelete MemoryOperation = "delete"
)

// MemoryGrant grants a SPIFFE identity the right to perform specific operations
// on specific namespaces within a retriever. The policy is deny-by-default:
// a call is allowed only when it matches at least one grant.
// Implements R-MEM-AUTH-2.
type MemoryGrant struct {
	// Identity is the full SPIFFE ID (e.g. spiffe://trust-domain/ns/ns/sa/sa)
	// or a prefix ending in "/" to match a sub-tree.
	Identity string `json:"identity"`

	// Operations lists the permitted operations for this identity.
	// +kubebuilder:validation:MinItems=1
	Operations []MemoryOperation `json:"operations"`

	// Namespaces is the allow-list of memory namespaces this grant applies to.
	// Use ["*"] to grant access to all namespaces the retriever exposes.
	// +kubebuilder:validation:MinItems=1
	Namespaces []string `json:"namespaces"`
}

// QuotaSpec declares per-retriever limits enforced by the gateway. R-MEM-QUOTA-1.
type QuotaSpec struct {
	// MaxTopK is the ceiling for per-call topK; requests above this are clamped.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default:=100
	// +optional
	MaxTopK int32 `json:"maxTopK,omitempty"`

	// RequestsPerMinute caps how many calls a single identity may make.
	// 0 means unlimited.
	// +kubebuilder:validation:Minimum=0
	// +optional
	RequestsPerMinute int32 `json:"requestsPerMinute,omitempty"`

	// MaxWriteBytes is the maximum payload size for a single write_memory call.
	// 0 means unlimited. Expressed in bytes.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxWriteBytes int64 `json:"maxWriteBytes,omitempty"`
}

// MemoryRetrieverSpec is the canonical CRD shape for a named retrieval pipeline.
// Implements R-MEM-API-2.
type MemoryRetrieverSpec struct {
	// Stores is the list of MemoryStore names (namespace-local) this retriever
	// binds to. At least one is required.
	// +kubebuilder:validation:MinItems=1
	Stores []string `json:"stores"`

	// ModelProviderRef is the name of an existing ModelProvider used for
	// embedding vectors. Required for vector stores.
	// +optional
	ModelProviderRef string `json:"modelProviderRef,omitempty"`

	// TopK is the default number of results returned per retrieval.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default:=10
	// +optional
	TopK int32 `json:"topK,omitempty"`

	// Namespaces is the allow-list of memory namespaces this retriever exposes.
	// If empty, only the default namespace is accessible.
	// +optional
	Namespaces []string `json:"namespaces,omitempty"`

	// Tenant scopes this retriever to a single tenant. Derived from SPIFFE
	// identity at runtime; must match the caller's attested tenant.
	// +optional
	Tenant string `json:"tenant,omitempty"`

	// Chunking configures how documents are split before indexing.
	// +optional
	Chunking ChunkSpec `json:"chunking,omitempty"`

	// Mount configures filesystem mounting (only meaningful for kind=filesystem stores).
	// +optional
	Mount *MountSpec `json:"mount,omitempty"`

	// Policy is the deny-by-default access control list. Only (identity, op, namespace)
	// tuples explicitly listed here are permitted. Implements R-MEM-AUTH-2.
	// +optional
	Policy []MemoryGrant `json:"policy,omitempty"`

	// Quota declares per-retriever resource limits. Implements R-MEM-QUOTA-1.
	// +optional
	Quota QuotaSpec `json:"quota,omitempty"`

	// MutationsTraT, when true, requires a valid Transaction Token (TraT) for
	// write_memory and delete_memory calls. Implements R-MEM-AUTH-3.
	// +optional
	MutationsTraT bool `json:"mutationsTraT,omitempty"`

	// DefaultMergePolicy is the conflict policy applied when merge_memory_fs
	// omits the onConflict field. When empty the gateway uses "fail".
	// +kubebuilder:validation:Enum=fail;ours;theirs;markers;union
	// +optional
	DefaultMergePolicy string `json:"defaultMergePolicy,omitempty"`

	// AllowedMergePolicies restricts which onConflict values callers may request.
	// Empty means all policies are allowed. The gateway rejects requests whose
	// onConflict is not in this list.
	// +optional
	AllowedMergePolicies []string `json:"allowedMergePolicies,omitempty"`
}

// MemoryRetrieverStatus is reported by the controller.
type MemoryRetrieverStatus struct {
	// Phase is one of Pending, Ready, Degraded.
	Phase string `json:"phase,omitempty"`

	// BoundWorkers is the count of retrieval workers serving this retriever.
	BoundWorkers int32 `json:"boundWorkers,omitempty"`

	// ObservedGeneration is the .metadata.generation the controller last acted on.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions follows the standard Kubernetes condition pattern.
	Conditions []MemoryCondition `json:"conditions,omitempty"`

	// Reason is a machine-readable summary of the current phase.
	Reason string `json:"reason,omitempty"`

	// Message is a human-readable elaboration of Reason.
	Message string `json:"message,omitempty"`
}

// ValidateMemoryStore enforces R-MEM-API-1 field rules.
func ValidateMemoryStore(s MemoryStoreSpec) error {
	var errs []error

	if !s.Kind.Valid() {
		errs = append(errs, fmt.Errorf("memorystore.kind=%q is invalid (want vector|graph|kv|eventlog|filesystem)", s.Kind))
	}

	// Driver must be non-empty and compatible with the kind.
	if s.Driver == "" {
		errs = append(errs, errors.New("memorystore.driver is required"))
	} else if s.Kind.Valid() {
		allowed := validDriversForKind[s.Kind]
		if !driverAllowed(s.Driver, allowed) {
			errs = append(errs, fmt.Errorf("memorystore.driver=%q is not valid for kind=%q", s.Driver, s.Kind))
		}
	}

	// Auth required for drivers that need credentials.
	if driversRequiringAuth[s.Driver] && (s.Auth == nil || s.Auth.SecretName == "") {
		errs = append(errs, fmt.Errorf("memorystore.auth.secretName is required for driver=%q", s.Driver))
	}

	// Endpoint required for non-agentfs drivers.
	if s.Driver != MemoryDriverAgentFS && s.Endpoint == "" {
		errs = append(errs, errors.New("memorystore.endpoint is required for non-agentfs drivers"))
	}

	// Tenancy validation.
	errs = append(errs, validateTenancy(s.Tenancy)...)

	// AgentFS config required iff kind=filesystem.
	if s.Kind == MemoryStoreFilesystem {
		if s.AgentFS == nil {
			errs = append(errs, errors.New("memorystore.agentfs is required when kind=filesystem"))
		} else if err := validateAgentFS(*s.AgentFS); err != nil {
			errs = append(errs, fmt.Errorf("memorystore.agentfs: %w", err))
		}
	} else if s.AgentFS != nil {
		errs = append(errs, fmt.Errorf("memorystore.agentfs must be nil when kind=%q", s.Kind))
	}

	return errors.Join(errs...)
}

// ValidateMemoryRetriever enforces R-MEM-API-2 field rules.
func ValidateMemoryRetriever(r MemoryRetrieverSpec) error {
	var errs []error

	if len(r.Stores) == 0 {
		errs = append(errs, errors.New("memoryretriever.stores is required (at least one)"))
	}
	for i, s := range r.Stores {
		if strings.TrimSpace(s) == "" {
			errs = append(errs, fmt.Errorf("memoryretriever.stores[%d] is empty", i))
		}
	}

	if r.TopK <= 0 {
		// TopK defaults to 10 via kubebuilder, but a zero from code-path is invalid.
		errs = append(errs, errors.New("memoryretriever.topK must be > 0"))
	}

	if r.Quota.MaxTopK < 0 {
		errs = append(errs, errors.New("memoryretriever.quota.maxTopK must be >= 0"))
	}
	if r.Quota.MaxTopK > 0 && r.TopK > r.Quota.MaxTopK {
		errs = append(errs, fmt.Errorf("memoryretriever.topK=%d exceeds quota.maxTopK=%d", r.TopK, r.Quota.MaxTopK))
	}
	if r.Quota.RequestsPerMinute < 0 {
		errs = append(errs, errors.New("memoryretriever.quota.requestsPerMinute must be >= 0"))
	}
	if r.Quota.MaxWriteBytes < 0 {
		errs = append(errs, errors.New("memoryretriever.quota.maxWriteBytes must be >= 0"))
	}

	if r.Chunking.Size < 0 {
		errs = append(errs, errors.New("memoryretriever.chunking.size must be >= 0"))
	}
	if r.Chunking.Overlap < 0 {
		errs = append(errs, errors.New("memoryretriever.chunking.overlap must be >= 0"))
	}
	if r.Chunking.Strategy != "" {
		switch r.Chunking.Strategy {
		case "fixed", "sentence", "paragraph":
		default:
			errs = append(errs, fmt.Errorf("memoryretriever.chunking.strategy=%q invalid (want fixed|sentence|paragraph)", r.Chunking.Strategy))
		}
	}

	if r.Mount != nil && r.Mount.MountPath != "" && !strings.HasPrefix(r.Mount.MountPath, "/") {
		errs = append(errs, errors.New("memoryretriever.mount.mountPath must be absolute"))
	}

	for i, g := range r.Policy {
		if g.Identity == "" {
			errs = append(errs, fmt.Errorf("memoryretriever.policy[%d].identity is required", i))
		}
		if len(g.Operations) == 0 {
			errs = append(errs, fmt.Errorf("memoryretriever.policy[%d].operations is required (at least one)", i))
		}
		for j, op := range g.Operations {
			switch op {
			case MemoryOpRead, MemoryOpWrite, MemoryOpDelete:
			default:
				errs = append(errs, fmt.Errorf("memoryretriever.policy[%d].operations[%d]=%q invalid (want read|write|delete)", i, j, op))
			}
		}
		if len(g.Namespaces) == 0 {
			errs = append(errs, fmt.Errorf("memoryretriever.policy[%d].namespaces is required (at least one)", i))
		}
	}

	return errors.Join(errs...)
}

// validateTenancy checks TenancySpec field rules.
func validateTenancy(t TenancySpec) []error {
	var errs []error
	if !t.Model.Valid() {
		errs = append(errs, fmt.Errorf("tenancy.model=%q invalid (want shared|dedicated)", t.Model))
	}
	if t.Model == TenancyShared && t.TenantLabelKey == "" {
		errs = append(errs, errors.New("tenancy.tenantLabelKey is required when model=shared"))
	}
	if t.Model == TenancyDedicated && t.TenantLabelKey != "" {
		errs = append(errs, errors.New("tenancy.tenantLabelKey must be empty when model=dedicated"))
	}
	return errs
}

// driverAllowed returns true if d is in the allowed list.
func driverAllowed(d MemoryStoreDriver, allowed []MemoryStoreDriver) bool {
	for _, a := range allowed {
		if a == d {
			return true
		}
	}
	return false
}
