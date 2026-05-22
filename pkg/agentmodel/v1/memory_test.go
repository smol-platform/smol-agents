package v1

import (
	"strings"
	"testing"
)

// ─── MemoryStoreKind.Valid ──────────────────────────────────────────────────

func TestMemoryStoreKind_Valid(t *testing.T) {
	valid := []MemoryStoreKind{
		MemoryStoreVector, MemoryStoreGraph, MemoryStoreKV,
		MemoryStoreEventLog, MemoryStoreFilesystem,
	}
	for _, k := range valid {
		if !k.Valid() {
			t.Errorf("%s should be valid", k)
		}
	}
	if MemoryStoreKind("blob").Valid() {
		t.Error("unknown kind should be invalid")
	}
}

func TestTenancyModel_Valid(t *testing.T) {
	if !TenancyShared.Valid() {
		t.Error("shared should be valid")
	}
	if !TenancyDedicated.Valid() {
		t.Error("dedicated should be valid")
	}
	if TenancyModel("cluster").Valid() {
		t.Error("unknown model should be invalid")
	}
}

// ─── ValidateMemoryStore helpers ────────────────────────────────────────────

func validVectorStore() MemoryStoreSpec {
	return MemoryStoreSpec{
		Kind:     MemoryStoreVector,
		Driver:   MemoryDriverPgVector,
		Endpoint: "pgvector.infra.svc:5432",
		Auth:     &AuthRef{SecretName: "pgvector-creds"},
		Tenancy: TenancySpec{
			Model:          TenancyShared,
			TenantLabelKey: "tenant",
		},
	}
}

func validFilesystemStore() MemoryStoreSpec {
	return MemoryStoreSpec{
		Kind:   MemoryStoreFilesystem,
		Driver: MemoryDriverAgentFS,
		Tenancy: TenancySpec{
			Model:          TenancyShared,
			TenantLabelKey: "tenant",
		},
		AgentFS: &AgentFSSpec{
			SizeGiB:   10,
			MountPath: "/var/agentfs",
		},
	}
}

// ─── ValidateMemoryStore: happy paths ───────────────────────────────────────

func TestValidateMemoryStore_VectorHappy(t *testing.T) {
	if err := ValidateMemoryStore(validVectorStore()); err != nil {
		t.Errorf("happy vector store: %v", err)
	}
}

func TestValidateMemoryStore_FilesystemHappy(t *testing.T) {
	if err := ValidateMemoryStore(validFilesystemStore()); err != nil {
		t.Errorf("happy filesystem store: %v", err)
	}
}

func TestValidateMemoryStore_QdrantHappy(t *testing.T) {
	s := validVectorStore()
	s.Driver = MemoryDriverQdrant
	s.Endpoint = "qdrant.infra.svc:6333"
	s.Auth = &AuthRef{SecretName: "qdrant-api-key"}
	if err := ValidateMemoryStore(s); err != nil {
		t.Errorf("happy qdrant: %v", err)
	}
}

func TestValidateMemoryStore_DedicatedTenancy(t *testing.T) {
	s := validVectorStore()
	s.Tenancy = TenancySpec{Model: TenancyDedicated}
	if err := ValidateMemoryStore(s); err != nil {
		t.Errorf("dedicated tenancy: %v", err)
	}
}

// ─── ValidateMemoryStore: rejection cases ───────────────────────────────────

func TestValidateMemoryStore_InvalidKind(t *testing.T) {
	s := validVectorStore()
	s.Kind = "blob"
	err := ValidateMemoryStore(s)
	if err == nil || !strings.Contains(err.Error(), "kind=") {
		t.Errorf("expected kind error: %v", err)
	}
}

func TestValidateMemoryStore_EmptyDriver(t *testing.T) {
	s := validVectorStore()
	s.Driver = ""
	err := ValidateMemoryStore(s)
	if err == nil || !strings.Contains(err.Error(), "driver is required") {
		t.Errorf("expected driver required: %v", err)
	}
}

func TestValidateMemoryStore_DriverKindMismatch(t *testing.T) {
	s := validVectorStore()
	s.Driver = MemoryDriverNeo4j // neo4j is for graph, not vector
	err := ValidateMemoryStore(s)
	if err == nil || !strings.Contains(err.Error(), "not valid for kind=") {
		t.Errorf("expected kind/driver mismatch: %v", err)
	}
}

func TestValidateMemoryStore_AuthRequiredForPgVector(t *testing.T) {
	s := validVectorStore()
	s.Auth = nil
	err := ValidateMemoryStore(s)
	if err == nil || !strings.Contains(err.Error(), "auth.secretName is required") {
		t.Errorf("expected auth error: %v", err)
	}
}

func TestValidateMemoryStore_AuthEmptySecretName(t *testing.T) {
	s := validVectorStore()
	s.Auth = &AuthRef{SecretName: ""}
	err := ValidateMemoryStore(s)
	if err == nil || !strings.Contains(err.Error(), "auth.secretName is required") {
		t.Errorf("expected auth.secretName error: %v", err)
	}
}

func TestValidateMemoryStore_EndpointRequiredForNonAgentFS(t *testing.T) {
	s := validVectorStore()
	s.Endpoint = ""
	err := ValidateMemoryStore(s)
	if err == nil || !strings.Contains(err.Error(), "endpoint is required") {
		t.Errorf("expected endpoint error: %v", err)
	}
}

func TestValidateMemoryStore_FilesystemRequiresAgentFSSpec(t *testing.T) {
	s := validFilesystemStore()
	s.AgentFS = nil
	err := ValidateMemoryStore(s)
	if err == nil || !strings.Contains(err.Error(), "agentfs is required") {
		t.Errorf("expected agentfs required: %v", err)
	}
}

func TestValidateMemoryStore_AgentFSMustBeNilForNonFilesystem(t *testing.T) {
	s := validVectorStore()
	s.AgentFS = &AgentFSSpec{SizeGiB: 5}
	err := ValidateMemoryStore(s)
	if err == nil || !strings.Contains(err.Error(), "agentfs must be nil") {
		t.Errorf("expected agentfs-nil error: %v", err)
	}
}

func TestValidateMemoryStore_SharedTenancyRequiresLabelKey(t *testing.T) {
	s := validVectorStore()
	s.Tenancy = TenancySpec{Model: TenancyShared} // missing TenantLabelKey
	err := ValidateMemoryStore(s)
	if err == nil || !strings.Contains(err.Error(), "tenantLabelKey is required") {
		t.Errorf("expected tenantLabelKey error: %v", err)
	}
}

func TestValidateMemoryStore_DedicatedTenancyRejectsLabelKey(t *testing.T) {
	s := validVectorStore()
	s.Tenancy = TenancySpec{Model: TenancyDedicated, TenantLabelKey: "tenant"}
	err := ValidateMemoryStore(s)
	if err == nil || !strings.Contains(err.Error(), "tenantLabelKey must be empty") {
		t.Errorf("expected tenantLabelKey-empty error: %v", err)
	}
}

func TestValidateMemoryStore_InvalidTenancyModel(t *testing.T) {
	s := validVectorStore()
	s.Tenancy = TenancySpec{Model: "cluster"}
	err := ValidateMemoryStore(s)
	if err == nil || !strings.Contains(err.Error(), "tenancy.model=") {
		t.Errorf("expected tenancy model error: %v", err)
	}
}

// ─── ValidateMemoryRetriever helpers ────────────────────────────────────────

func validRetriever() MemoryRetrieverSpec {
	return MemoryRetrieverSpec{
		Stores:           []string{"prod-vectors"},
		ModelProviderRef: "openai-text-embedding-3",
		TopK:             10,
		Namespaces:       []string{"default", "docs"},
		Tenant:           "team-alpha",
		Chunking: ChunkSpec{
			Size:     512,
			Overlap:  64,
			Strategy: "fixed",
		},
		Policy: []MemoryGrant{
			{
				Identity:   "spiffe://stigen.ai/ns/agents/sa/coder",
				Operations: []MemoryOperation{MemoryOpRead, MemoryOpWrite},
				Namespaces: []string{"default"},
			},
		},
		Quota: QuotaSpec{
			MaxTopK:           100,
			RequestsPerMinute: 60,
			MaxWriteBytes:     1 << 20, // 1 MiB
		},
	}
}

// ─── ValidateMemoryRetriever: happy paths ───────────────────────────────────

func TestValidateMemoryRetriever_Happy(t *testing.T) {
	if err := ValidateMemoryRetriever(validRetriever()); err != nil {
		t.Errorf("happy retriever: %v", err)
	}
}

func TestValidateMemoryRetriever_NoPolicyAllowed(t *testing.T) {
	r := validRetriever()
	r.Policy = nil
	if err := ValidateMemoryRetriever(r); err != nil {
		t.Errorf("no-policy retriever: %v", err)
	}
}

func TestValidateMemoryRetriever_MutationsTraT(t *testing.T) {
	r := validRetriever()
	r.MutationsTraT = true
	if err := ValidateMemoryRetriever(r); err != nil {
		t.Errorf("mutationsTraT=true: %v", err)
	}
}

func TestValidateMemoryRetriever_WithMount(t *testing.T) {
	r := validRetriever()
	r.Mount = &MountSpec{Enabled: true, MountPath: "/workspace"}
	if err := ValidateMemoryRetriever(r); err != nil {
		t.Errorf("with mount: %v", err)
	}
}

// ─── ValidateMemoryRetriever: rejection cases ───────────────────────────────

func TestValidateMemoryRetriever_EmptyStores(t *testing.T) {
	r := validRetriever()
	r.Stores = nil
	err := ValidateMemoryRetriever(r)
	if err == nil || !strings.Contains(err.Error(), "stores is required") {
		t.Errorf("expected stores error: %v", err)
	}
}

func TestValidateMemoryRetriever_BlankStoreName(t *testing.T) {
	r := validRetriever()
	r.Stores = []string{""}
	err := ValidateMemoryRetriever(r)
	if err == nil || !strings.Contains(err.Error(), "stores[0] is empty") {
		t.Errorf("expected blank store name error: %v", err)
	}
}

func TestValidateMemoryRetriever_ZeroTopK(t *testing.T) {
	r := validRetriever()
	r.TopK = 0
	err := ValidateMemoryRetriever(r)
	if err == nil || !strings.Contains(err.Error(), "topK must be > 0") {
		t.Errorf("expected topK error: %v", err)
	}
}

func TestValidateMemoryRetriever_TopKExceedsQuota(t *testing.T) {
	r := validRetriever()
	r.TopK = 200
	r.Quota.MaxTopK = 100
	err := ValidateMemoryRetriever(r)
	if err == nil || !strings.Contains(err.Error(), "exceeds quota.maxTopK") {
		t.Errorf("expected topK-quota error: %v", err)
	}
}

func TestValidateMemoryRetriever_NegativeRequestsPerMinute(t *testing.T) {
	r := validRetriever()
	r.Quota.RequestsPerMinute = -1
	err := ValidateMemoryRetriever(r)
	if err == nil || !strings.Contains(err.Error(), "requestsPerMinute") {
		t.Errorf("expected requestsPerMinute error: %v", err)
	}
}

func TestValidateMemoryRetriever_NegativeMaxWriteBytes(t *testing.T) {
	r := validRetriever()
	r.Quota.MaxWriteBytes = -1
	err := ValidateMemoryRetriever(r)
	if err == nil || !strings.Contains(err.Error(), "maxWriteBytes") {
		t.Errorf("expected maxWriteBytes error: %v", err)
	}
}

func TestValidateMemoryRetriever_InvalidChunkingStrategy(t *testing.T) {
	r := validRetriever()
	r.Chunking.Strategy = "word"
	err := ValidateMemoryRetriever(r)
	if err == nil || !strings.Contains(err.Error(), "chunking.strategy=") {
		t.Errorf("expected chunking strategy error: %v", err)
	}
}

func TestValidateMemoryRetriever_MountPathMustBeAbsolute(t *testing.T) {
	r := validRetriever()
	r.Mount = &MountSpec{Enabled: true, MountPath: "relative/path"}
	err := ValidateMemoryRetriever(r)
	if err == nil || !strings.Contains(err.Error(), "mountPath must be absolute") {
		t.Errorf("expected mountPath error: %v", err)
	}
}

func TestValidateMemoryRetriever_PolicyEmptyIdentity(t *testing.T) {
	r := validRetriever()
	r.Policy[0].Identity = ""
	err := ValidateMemoryRetriever(r)
	if err == nil || !strings.Contains(err.Error(), "identity is required") {
		t.Errorf("expected policy identity error: %v", err)
	}
}

func TestValidateMemoryRetriever_PolicyEmptyOperations(t *testing.T) {
	r := validRetriever()
	r.Policy[0].Operations = nil
	err := ValidateMemoryRetriever(r)
	if err == nil || !strings.Contains(err.Error(), "operations is required") {
		t.Errorf("expected policy operations error: %v", err)
	}
}

func TestValidateMemoryRetriever_PolicyInvalidOperation(t *testing.T) {
	r := validRetriever()
	r.Policy[0].Operations = []MemoryOperation{"admin"}
	err := ValidateMemoryRetriever(r)
	if err == nil || !strings.Contains(err.Error(), "invalid (want read|write|delete)") {
		t.Errorf("expected invalid operation error: %v", err)
	}
}

func TestValidateMemoryRetriever_PolicyEmptyNamespaces(t *testing.T) {
	r := validRetriever()
	r.Policy[0].Namespaces = nil
	err := ValidateMemoryRetriever(r)
	if err == nil || !strings.Contains(err.Error(), "namespaces is required") {
		t.Errorf("expected policy namespaces error: %v", err)
	}
}
