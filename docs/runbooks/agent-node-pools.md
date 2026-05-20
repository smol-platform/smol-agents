# Runbook — Agent node pools (kata-capable nodes via Karpenter)

How to operate `AgentNodePool`: the operator compiles each one into a
Karpenter `NodePool` + `EC2NodeClass` that provisions bare-metal (KVM) nodes
carrying the kata/devmapper layer, and binds sandboxed agents onto them.
Design: [`docs/design/agent-platform.md`](../design/agent-platform.md).

## Prerequisites

1. **Karpenter installed** on the k0s cluster (non-EKS config: explicit
   `clusterName`/`clusterEndpoint`, a control-plane instance profile with the
   Karpenter EC2 permissions). Node→cluster join is owned by *your* Karpenter
   deployment; the operator only adds the kata layer on top of it.
2. **Platform `nodeProvisioning`** set on the singleton `KnativeAgentPlatform`
   so generated `EC2NodeClass`es carry real selectors/role/join:
   ```yaml
   spec:
     nodeProvisioning:
       amiFamily: Custom
       role: KarpenterNodeRole-k0s
       subnetSelectorTags:        { karpenter.sh/discovery: k0s }
       securityGroupSelectorTags: { karpenter.sh/discovery: k0s }
       baseAMISelector: [ { tags: { k0s-join: "true" } } ]   # UserData mode
       allowGvisorFallback: false
       # joinUserData: |
       #   <your existing k0s worker-join + kubelet --provider-id snippet>
   ```
3. **Knative podspec feature-flags** (only for serverless/`knative` agents):
   `kubernetes.podspec-runtimeclassname`, `-affinity`, `-tolerations`,
   `-nodeselector` must be `enabled` in `knative-serving/config-features`, or
   placement is silently dropped (the operator surfaces a warning on the
   agent's Knative feature when it can read the ConfigMap).

## Create a pool

```bash
kubectl apply -f operator/config/samples/agentnodepool_kata_arm64.yaml
kubectl get agentnodepool          # shortname: anp
```

A kata agent then binds automatically — no per-agent config — because the
operator auto-matches `features.sandbox.runtimeClass` to the pool's
`isolation`. Override with `features.sandbox.nodePoolRef` is not yet wired
(P1 uses auto-match only).

## Providers: Karpenter vs Cluster Autoscaler

`spec.provider` selects the backend. The in-cluster workload coupling (pool
label + isolation taint) is identical for both — only how nodes get created
differs:

- **Karpenter** (default): the operator creates owned `NodePool` +
  `EC2NodeClass` objects and Karpenter launches metal nodes on demand.
- **ClusterAutoscaler**: CAS scales a pre-existing cloud ASG, which the
  operator cannot create. Instead it writes the node-group spec to a ConfigMap
  for your IaC to apply to a matching ASG:
  ```bash
  kubectl -n knative-agents-system get cm anp-<name>-clusterautoscaler -o yaml
  ```
  The ConfigMap carries `requiredASGTags` (CAS auto-discovery + node-template
  label/taint, so CAS scales the right ASG for a pending kata pod), the
  `instanceFamilies` (must be metal for kata), and the kata launch-template
  `userData`. Apply those to an ASG + launch template; CAS then scales it when
  kata pods are pending.

## Verify

```bash
kubectl get anp kata-arm64 -o wide                 # Phase=Ready, Capacity
kubectl get nodepool anp-kata-arm64                 # owned by the AgentNodePool
kubectl get ec2nodeclass anp-kata-arm64
# When an agent schedules, Karpenter launches a *.metal node:
kubectl get nodes -l agents.stigen.ai/pool=kata-arm64
kubectl get pod <agent-pod> -o jsonpath='{.spec.runtimeClassName}{"\n"}{.metadata.annotations.karpenter\.sh/do-not-disrupt}{"\n"}'
```

## Status & conditions

`AgentNodePool` status carries `Phase` (`Pending|Reconciling|Ready|Degraded`)
and conditions `Ready` + `KarpenterSynced`.

| Symptom | Cause | Action |
|---|---|---|
| `Degraded`, `KarpenterSynced=False` reason `KarpenterMissing` | Karpenter CRDs not installed | Install/repair Karpenter on the cluster |
| `Degraded`, reason `ApplyFailed` | EC2NodeClass rejected (e.g. empty subnet/SG selectors) | Set `nodeProvisioning` on the Platform |
| Agent stuck, sandbox feature `NoKVMCapacity` | kata agent, no matching pool, fallback disabled | Create a matching `AgentNodePool`, or set `allowGvisorFallback: true` |
| Pool `Ready` but pods Pending forever | No metal capacity (spot intermittency) | Widen `instanceFamilies`/`capacityType`; check Karpenter events |
| kata pod schedules but won't start | kata layer failed at boot (devmapper/containerd) | SSM/console into the node; check the userData log + `dmsetup status kata-thinpool` |
| Placement ignored on a Knative agent | podspec feature-flags off | Enable them in `knative-serving/config-features` |

## gVisor fallback

When `allowGvisorFallback: true`, a kata agent with no matching pool is
rendered with the gVisor runtime instead of being held — useful on managed
clusters without KVM. gVisor is a weaker boundary than a Firecracker
microVM; leave it `false` if you require microVM isolation.

## Bootstrap modes

`AgentNodePool.spec.bootstrap.mode`:

- `UserData` (dev/iteration): the kata recipe is appended to your join
  snippet and installed at boot — slower node-Ready, no image pipeline.
- `PrebakedAMI` (prod): kata is baked into the image (Packer, tagged
  `kata-ready`); only the per-launch thin-pool runs at boot — fastest
  node-Ready, prerequisite for viable serverless cold-start.

The thin-pool is always created at firstboot (instance-store NVMe is blank
per launch); the recipe prefers the local NVMe and falls back to the root
volume.
