package main

import (
	"context"
	"flag"
	"os"
	"time"

	"github.com/smol-platform/smol-agents/pkg/secrets"
)

// runLease is the `agent lease <name>` subcommand (M3.20): it leases one secret
// from the in-pod broker and writes its value to stdout (no trailing newline).
// It backs the claude-code apiKeyHelper — claude runs this command and re-runs it
// on TTL/401, so each call delivers a fresh short-lived credential straight to the
// CLI rather than baking a static key into the pod's env. Authorization is the
// broker's (the same UDS + policy as every other lease); a denied/unknown name
// exits non-zero.
func runLease(args []string) int {
	fs := flag.NewFlagSet("lease", flag.ExitOnError)
	socket := fs.String("secret-socket", "/run/secret-broker/secret-broker.sock", "secret broker UDS")
	ttl := fs.Duration("ttl", 0, "requested lease TTL (0 = broker default)")
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		os.Stderr.WriteString("usage: agent lease <secret-name>\n")
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c := secrets.NewClient(*socket)
	defer c.Close()
	lease, err := c.Lease(ctx, fs.Arg(0), *ttl)
	if err != nil {
		os.Stderr.WriteString("agent lease: " + err.Error() + "\n")
		return 1
	}
	os.Stdout.Write(lease.Value)
	return 0
}
