// Package runtime coordinates the agent's lifecycle: Start → Ready →
// Drain → Stop. It implements R-RUN-1 (health and readiness) and R-RUN-2
// (graceful shutdown).
package runtime
