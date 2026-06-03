// Command agentbench is the benchmarking + verification runner for the
// smol-agents platform. It deploys a declared fleet of full-stack agents
// against real LLM backends, submits workloads (AgentRun CRs or agentgateway
// turns), runs correctness oracles + perf metrics against the
// controller-observed pure.RunStatus, and writes results.json + report.md.
//
// Subcommands:
//
//	agentbench lint   --plan PATH                         validate a plan + its oracles
//	agentbench run    --plan PATH --kubeconfig KC [flags] deploy, run, collect, report, teardown
//	agentbench report --out DIR                           re-render report.md from results.json
//
// Honesty rules are enforced in the package (see internal/agentbench): tokens
// are real only for Hermes; usage.toolCalls is never gated; loop-mode tool
// calls are asserted-rejected; a blocked case whose negative oracle stops
// holding FAILs (anti-staleness).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/smol-platform/smol-agents/operator/internal/agentbench"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "lint":
		err = cmdLint(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "report":
		err = cmdReport(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "agentbench: unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentbench: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `agentbench — smol-agents benchmarking + verification runner

usage:
  agentbench lint   --plan PATH
  agentbench run    --plan PATH --kubeconfig KC [--tier T] [--driver run|gateway]
                    [--concurrency N] [--repeat M] [--out DIR] [--allow-blocked] [--record]
                    [--gateway-url URL] [--hermes-url URL] [--s3-url URL] [--metadata-block]
  agentbench report --out DIR
`)
}

// cmdLint validates a plan offline: decode, validate, and confirm every
// oracle.kind in it is registered. No cluster needed.
func cmdLint(args []string) error {
	fs := flag.NewFlagSet("lint", flag.ExitOnError)
	planPath := fs.String("plan", "", "path to a plan.yaml or a plan directory")
	_ = fs.Parse(args)
	if *planPath == "" {
		return fmt.Errorf("lint: --plan is required")
	}
	plan, err := agentbench.LoadPlan(*planPath)
	if err != nil {
		return err
	}
	fmt.Printf("plan %q OK: %d case(s), digest %s\n", plan.Metadata.Name, len(plan.Cases), plan.Digest())
	for _, c := range agentbench.SortCasesStable(plan.Cases) {
		blocked := ""
		if c.Blocked != nil {
			blocked = "  [BLOCKED: " + c.Blocked.Reason + "]"
		}
		fmt.Printf("  - %-28s tier=%-11s oracle=%-22s driver=%s%s\n",
			c.Metadata.Name, c.Tier, c.Oracle.Kind, c.EffectiveDriver(), blocked)
	}
	fmt.Printf("registered oracles: %v\n", agentbench.RegisteredKinds())
	return nil
}

// cmdRun executes the full lifecycle.
func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	planPath := fs.String("plan", "", "path to a plan.yaml or a plan directory")
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig path (default: $KUBECONFIG / in-cluster)")
	kubeContext := fs.String("context", "", "kubeconfig context override")
	tier := fs.String("tier", "", "only run cases in this tier (correctness|perf|scale|isolation|future)")
	driver := fs.String("driver", "", "override the per-case driver (run|gateway)")
	concurrency := fs.Int("concurrency", 1, "max in-flight samples per case")
	repeat := fs.Int("repeat", 1, "multiply each case's samples by this")
	out := fs.String("out", "./results", "output directory root")
	allowBlocked := fs.Bool("allow-blocked", false, "run future/blocked-tier cases")
	record := fs.Bool("record", false, "retain each sample's full RunStatus in results.json")
	gatewayURL := fs.String("gateway-url", "", "agentgateway base URL (enables the gateway driver + cap)")
	hermesURL := fs.String("hermes-url", "", "hermes gateway URL (for the hermes cap probe)")
	s3URL := fs.String("s3-url", "", "minio/s3 endpoint URL (for the s3 cap probe)")
	metadataBlock := fs.Bool("metadata-block", false, "assert the CNI enforces default-deny egress (enables the metadata-block cap)")
	_ = fs.Parse(args)

	if *planPath == "" {
		return fmt.Errorf("run: --plan is required")
	}
	plan, err := agentbench.LoadPlan(*planPath)
	if err != nil {
		return err
	}

	cl, err := buildClient(*kubeconfig, *kubeContext)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	// Probe capabilities (fail-closed: undetectable caps stay false).
	caps := agentbench.ProbeCaps(ctx, cl, agentbench.ProbeOptions{
		GatewayURL:        *gatewayURL,
		HermesURL:         *hermesURL,
		S3URL:             *s3URL,
		MetadataBlockable: *metadataBlock,
	})
	fmt.Printf("caps: %s (node kernel %q, runtime %s)\n", caps.String(), caps.NodeKernel, caps.Runtime)

	runID := newRunID()
	ns := "agentbench-" + runID
	nonce := agentbench.NewNonce()
	scheme := agentbench.NewScheme()

	fleet := agentbench.NewFleet(cl, scheme, ns, plan.Dir(), map[string]string{
		"{{NONCE}}":     nonce,
		"{{NAMESPACE}}": ns,
	})

	fmt.Printf("deploying fleet into namespace %s …\n", ns)
	if derr := fleet.Deploy(ctx, plan); derr != nil {
		// Best-effort teardown of a partial deploy.
		_ = fleet.Teardown(context.Background())
		return fmt.Errorf("deploy failed: %w", derr)
	}
	defer func() {
		fmt.Printf("tearing down namespace %s …\n", ns)
		tctx, tcancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer tcancel()
		if terr := fleet.Teardown(tctx); terr != nil {
			fmt.Fprintf(os.Stderr, "agentbench: teardown: %v\n", terr)
		}
	}()

	results, err := runCases(ctx, plan, fleet, caps, runID, nonce, RunFlags{
		Tier:         agentbench.Tier(*tier),
		Driver:       agentbench.DriverKind(*driver),
		Concurrency:  *concurrency,
		Repeat:       *repeat,
		AllowBlocked: *allowBlocked,
		Record:       *record,
		GatewayURL:   *gatewayURL,
	})
	if err != nil {
		return err
	}

	outDir := filepath.Join(*out, runID)
	jsonPath, err := results.WriteJSON(outDir)
	if err != nil {
		return fmt.Errorf("write results.json: %w", err)
	}
	mdPath, err := results.WriteMarkdown(outDir)
	if err != nil {
		return fmt.Errorf("write report.md: %w", err)
	}
	fmt.Printf("\nverdict: %s — total %d passed %d failed %d skipped %d blocked %d\n",
		results.Verdict, results.Summary.Total, results.Summary.Passed,
		results.Summary.Failed, results.Summary.Skipped, results.Summary.Blocked)
	fmt.Printf("wrote %s\nwrote %s\n", jsonPath, mdPath)
	if results.Verdict == "FAIL" {
		os.Exit(1)
	}
	return nil
}

// RunFlags carries the resolved run-command flags into runCases.
type RunFlags struct {
	Tier         agentbench.Tier
	Driver       agentbench.DriverKind
	Concurrency  int
	Repeat       int
	AllowBlocked bool
	Record       bool
	GatewayURL   string
}

// runCases drives every selected case and aggregates the results. Cases run
// sequentially (each case fans its samples out with --concurrency); fs_roundtrip
// cases receive the prior case's representative output threaded in plan order.
func runCases(ctx context.Context, plan *agentbench.BenchPlan, fleet *agentbench.Fleet, caps agentbench.CapsResult, runID, nonce string, fl RunFlags) (*agentbench.Results, error) {
	res := &agentbench.Results{
		RunID:      runID,
		PlanDigest: plan.Digest(),
		Plan:       plan.Metadata.Name,
		Tier:       fl.Tier,
		Cluster: agentbench.ClusterInfo{
			Runtime: caps.Runtime,
			Caps:    splitCaps(caps.String()),
			Node:    caps.NodeKernel,
		},
		StartedAt: time.Now().UTC(),
	}

	cl := fleet // for client access via fleet helpers

	cases := plan.CasesForTier(fl.Tier, fl.AllowBlocked)
	cases = agentbench.SortCasesStable(cases)

	// priorOutput threads run-N output into the next fs_roundtrip case.
	var priorOutput []byte

	for _, c := range cases {
		samples := c.EffectiveSamples()
		if fl.Repeat > 1 {
			c.Samples = samples * fl.Repeat
		}
		if fl.Driver != "" {
			c.Driver = fl.Driver
		}
		harnessKind := cl.HarnessKind(ctx, c.AgentRef)

		drv, derr := buildDriver(c, fleet, fl)
		if derr != nil {
			return nil, derr
		}
		runner, rerr := agentbench.NewCaseRunner(drv, caps, harnessKind, fl.Concurrency, c.Oracle.Kind)
		if rerr != nil {
			return nil, rerr
		}

		caseNonce := ""
		if c.Input.Nonce {
			caseNonce = nonce
		}
		fmt.Printf("• %-28s tier=%-11s oracle=%-22s …\n", c.Metadata.Name, c.Tier, c.Oracle.Kind)
		cr := runner.RunCase(ctx, c, fleet.Namespace(), caseNonce, priorOutput, fl.Record)
		fmt.Printf("    %s — %s\n", cr.Oracle.Verdict, cr.Oracle.Evidence)
		res.Cases = append(res.Cases, cr)

		// Thread this case's representative output forward for fs_roundtrip.
		if rep := cr.RepOutput(); len(rep) > 0 {
			priorOutput = rep
		}
	}

	res.FinishedAt = time.Now().UTC()
	res.Finalize()
	return res, nil
}

// buildDriver selects the run or gateway driver for a case.
func buildDriver(c agentbench.BenchCase, fleet *agentbench.Fleet, fl RunFlags) (agentbench.Driver, error) {
	switch c.EffectiveDriver() {
	case agentbench.DriverGateway:
		if fl.GatewayURL == "" {
			return nil, fmt.Errorf("case %q uses the gateway driver but --gateway-url is unset", c.Metadata.Name)
		}
		return agentbench.NewGatewayDriver(fl.GatewayURL, 90*time.Second), nil
	default:
		return agentbench.NewRunDriver(fleet.Client(), fleet.Owner()), nil
	}
}

// cmdReport re-renders report.md from an existing results.json.
func cmdReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	out := fs.String("out", "", "directory holding results.json (report.md is (re)written here)")
	_ = fs.Parse(args)
	if *out == "" {
		return fmt.Errorf("report: --out is required")
	}
	raw, err := os.ReadFile(filepath.Join(*out, "results.json"))
	if err != nil {
		return fmt.Errorf("report: read results.json: %w", err)
	}
	var res agentbench.Results
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("report: decode results.json: %w", err)
	}
	path, err := res.WriteMarkdown(*out)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", path)
	return nil
}

// buildClient constructs a controller-runtime client from a kubeconfig path +
// context, falling back to the ambient config when path is empty.
func buildClient(kubeconfig, kubeContext string) (client.Client, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{CurrentContext: kubeContext},
	)
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	return client.New(cfg, client.Options{Scheme: agentbench.NewScheme()})
}

func newRunID() string {
	return time.Now().UTC().Format("20060102T1504Z") + "-" + agentbench.NewNonce()[:4]
}

func splitCaps(s string) []string {
	if s == "" || s == "none" {
		return []string{}
	}
	out := splitComma(s)
	sort.Strings(out)
	return out
}

func splitComma(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
