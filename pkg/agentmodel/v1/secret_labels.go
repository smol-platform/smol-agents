package v1

// TenantSecretLabel is the tenant-boundary opt-in marker: the operator refuses
// to read or project any CR-referenced Secret that does not carry
// TenantSecretLabel: "true". Without it, a compromised or careless tenant could
// point a SecretRef (ModelProvider, harness env, Tool auth, AgentFS S3 creds,
// ModelGateway env/UI auth) at any Secret it can name in a namespace it
// controls — including one it doesn't own, e.g. another tenant's or a
// platform-managed one — and have the operator read/project it into a pod.
// Requiring this label is fail-closed by design (decision D3 in
// docs/design/decisions.md): an unlabeled Secret is rejected, not silently
// skipped.
//
// It lives in the pure package (the lowest layer) so the enforcement point (the
// AgentRun/AgentSession/ModelGateway reconcilers) and every fixture that has to
// stamp it (the bench fleet, e2e scenarios) reference one source of truth.
const TenantSecretLabel = "agents.smol-agents.ai/tenant-secret"
