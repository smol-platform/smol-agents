// Package agentfs is the persistence + backup layer for Turso AgentFS
// volumes. It treats the underlying SQLite database as the canonical
// state of an agent's filesystem and provides:
//
//   - Backup     — produces a consistent snapshot via SQLite's online
//     backup API and uploads it to S3 with versioning.
//   - Restore    — selects a version (latest, by VersionID, or
//     point-in-time) and writes it back to a fresh DB.
//   - WAL frames — between full backups, a small WAL-frame uploader
//     streams new pages so RPO is bounded by the
//     snapshot interval, not the full-backup cadence.
//   - Retention  — caps the version count and enforces minimum age
//     before deletion.
//
// The package exposes Driver interfaces (Storage, S3) that are easily
// faked. Production wiring uses modernc.org/sqlite and aws-sdk-go-v2.
//
// Implements R-AFS-1..4 from the agent-model spec.
package agentfs
