# Feature Guides

A guided tour of what smol-agents does, one subsystem at a time. Each guide
explains **what** the feature is, **why** it exists, **how** it works, the
**API surface** (CRD fields / package interfaces), a **worked example**, and
links to the runbook, the spec, and the formal invariant that proves it.

Read top-to-bottom for the full picture, or jump to what you need.

| # | Guide | One-line |
|---|---|---|
| 1 | [Runtime & Identity](runtime-and-identity.md) | The hardened core: microVM sandbox, SPIFFE identity, two-rail mTLS, the secret broker, and host eBPF. |
| 2 | [Operator](operator.md) | The control plane: feature-flagged `SmolAgent` / `SmolAgentPlatform` CRs, status conditions, rollouts, drift healing. |
| 3 | [Agent Model](agent-model.md) | Declaring an agent as a CR: model, MCP tools, memory, **hard budgets**, and replayable runs. |
| 4 | [Networking (agentnet)](agentnet.md) | Identity proxy, userspace WireGuard mesh, and the eBPF egress allow-list. |
| 5 | [Egress Credentials](egress-credentials.md) | Token-exchange all the way down: TraT injection + **secretless** provider credentials the agent never holds. |
| 6 | [Memory](memory.md) | Agent memory as an MCP service — vector/graph/KV/eventlog/filesystem backends, tenant isolation, and branchable AgentFS with 3-way merge. |
| 7 | [Node Provisioning](node-provisioning.md) | `AgentNodePool` derives node shape from isolation and programs Karpenter to build exactly those nodes. |
| 8 | [Verification](verification.md) | How "verifiable by default" actually works: Quint specs, `rapid` properties, and the L0/L1/L2 e2e rings. |

## The mental model

smol-agents separates three planes:

- **Substrate** (`agents.smol-agents.ai/v1`) — *where* agents run. The operator,
  the platform defaults, and node provisioning. Guides **2** and **7**.
- **Workload** (`runtime.agents.smol-agents.ai/v1`) — *what* runs. The agent model,
  networking, and memory. Guides **3**, **4**, **5**, **6**.
- **Foundations** (`pkg/*`) — the guarantees every plane inherits: identity,
  transport, sandbox, secrets, eBPF. Guide **1**, and proven in guide **8**.

Each agent is one binary with one identity doing one job, bounded by a microVM,
gated by SPIFFE, and held to a budget the model checker proves it cannot exceed.

## Conventions used in every guide

- **Trust domain** defaults to `smol-agents.ai`; SPIFFE IDs read
  `spiffe://<trust-domain>/ns/<namespace>/sa/<serviceaccount>`.
- **CRD groups**: `agents.smol-agents.ai/v1` (substrate) and
  `runtime.agents.smol-agents.ai/v1` (workload).
- **Examples** are taken from `operator/config/samples/` and are kept runnable.
- **"Proven by"** links a claim to the Quint spec under `spec/quint/` that
  model-checks it.
