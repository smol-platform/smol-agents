// Package builders contains pure Build* functions that translate the
// agent-model CRs (Agent, AgentRun, AgentSession, AgentNodePool, ...)
// into typed Kubernetes objects ready for server-side apply.
//
// Builders are pure: no client, no I/O, no time.Now(). This makes them
// trivially testable and lets feature reconcilers compose them.
package builders
