// Command agentctl is a small CLI that talks to a local agent / broker
// for diagnostics.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"
	"time"

	"github.com/stigen/knative-agents/internal/version"
	"github.com/stigen/knative-agents/pkg/secrets"
)

func main() {
	flag.Usage = usage
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "version":
		fmt.Println(version.String())
	case "status":
		os.Exit(cmdStatus(args[1:]))
	case "lease":
		os.Exit(cmdLease(args[1:]))
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: agentctl <command> [args]")
	fmt.Fprintln(os.Stderr, "Commands:")
	fmt.Fprintln(os.Stderr, "  status [-addr http://127.0.0.1:8080]   ping /readyz and /healthz")
	fmt.Fprintln(os.Stderr, "  lease  [-socket path] -name NAME       request a lease from local broker")
	fmt.Fprintln(os.Stderr, "  version                                print version")
}

func cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	addr := fs.String("addr", "http://127.0.0.1:8080", "agent health endpoint")
	_ = fs.Parse(args)

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	defer w.Flush()
	fmt.Fprintln(w, "ENDPOINT\tSTATUS\tLATENCY")
	for _, p := range []string{"/healthz", "/readyz"} {
		t := time.Now()
		resp, err := http.Get(*addr + p)
		dur := time.Since(t).Truncate(time.Microsecond)
		if err != nil {
			fmt.Fprintf(w, "%s\tERROR: %v\t%s\n", p, err, dur)
			continue
		}
		_ = resp.Body.Close()
		fmt.Fprintf(w, "%s\t%d\t%s\n", p, resp.StatusCode, dur)
	}
	return 0
}

func cmdLease(args []string) int {
	fs := flag.NewFlagSet("lease", flag.ExitOnError)
	socket := fs.String("socket", "/run/secret-broker/secret-broker.sock", "broker UDS")
	name := fs.String("name", "", "secret name (required)")
	ttl := fs.Duration("ttl", 0, "requested TTL (0 = broker default)")
	_ = fs.Parse(args)

	if *name == "" {
		fmt.Fprintln(os.Stderr, "lease: -name is required")
		return 2
	}
	c := secrets.NewClient(*socket)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	l, err := c.Lease(ctx, *name, *ttl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lease error: %v\n", err)
		return 1
	}
	// Never print the value to stdout — leaks may be captured by shell history.
	fmt.Printf("name=%s audience=%s issued=%s expires=%s ttl=%s bytes=%d\n",
		l.Name, l.Audience, l.Issued.Format(time.RFC3339), l.ExpiresAt.Format(time.RFC3339),
		l.TTL, len(l.Value))
	return 0
}
