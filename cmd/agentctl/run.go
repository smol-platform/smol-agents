// agentctl run / watch / logs — the demo/dev loop against a live cluster:
// create an AgentRun from a prompt, stream its status transitions, print the
// folded output, and fetch the run pod's logs without hand-written YAML.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// kubeFlags are the kubeconfig/context/namespace flags shared by the
// cluster-facing subcommands (mirrors the deploy target=k8s flags).
type kubeFlags struct {
	kubeconfig *string
	kctx       *string
	namespace  *string
}

func addKubeFlags(fs *flag.FlagSet) kubeFlags {
	return kubeFlags{
		kubeconfig: fs.String("kubeconfig", "", "kubeconfig path (default: $KUBECONFIG or ~/.kube/config)"),
		kctx:       fs.String("context", "", "kubeconfig context (default: current-context)"),
		namespace:  fs.String("n", "default", "namespace"),
	}
}

func (k kubeFlags) restConfig() (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if *k.kubeconfig != "" {
		rules.ExplicitPath = *k.kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: *k.kctx}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
}

func (k kubeFlags) client() (client.Client, error) {
	cfg, err := k.restConfig()
	if err != nil {
		return nil, err
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := amv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return client.New(cfg, client.Options{Scheme: scheme})
}

// parseInterleaved parses fs over args, tolerating positionals before flags
// (stdlib flag stops at the first non-flag; "run <agent> -n ns" is the natural
// call shape). Returns the collected positional arguments.
func parseInterleaved(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		_ = fs.Parse(args)
		if fs.NArg() == 0 {
			return pos
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// cmdRun creates an AgentRun from a prompt and (with -follow) streams its
// status to completion, printing the folded output.
func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	kf := addKubeFlags(fs)
	prompt := fs.String("p", "", "prompt; becomes input {\"prompt\": ...}")
	inputJSON := fs.String("input", "", "raw JSON input (overrides -p)")
	name := fs.String("name", "", "AgentRun name (default: <agent>-<unix-ts>)")
	follow := fs.Bool("follow", false, "stream status transitions until the run is terminal")
	timeout := fs.Duration("timeout", 15*time.Minute, "max time to follow before giving up")
	pos := parseInterleaved(fs, args)

	if len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "run: exactly one <agent> argument is required")
		fs.Usage()
		return 2
	}
	agentRef := pos[0]

	var input json.RawMessage
	switch {
	case *inputJSON != "":
		if !json.Valid([]byte(*inputJSON)) {
			fmt.Fprintln(os.Stderr, "run: -input is not valid JSON")
			return 2
		}
		input = json.RawMessage(*inputJSON)
	case *prompt != "":
		b, _ := json.Marshal(map[string]string{"prompt": *prompt})
		input = b
	default:
		fmt.Fprintln(os.Stderr, "run: -p or -input is required")
		return 2
	}

	runName := *name
	if runName == "" {
		runName = fmt.Sprintf("%s-%d", agentRef, time.Now().Unix())
	}

	cli, err := kf.client()
	if err != nil {
		fmt.Fprintf(os.Stderr, "run: %v\n", err)
		return 1
	}
	run := &amv1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: runName, Namespace: *kf.namespace},
		Spec:       pure.AgentRunSpec{AgentRef: agentRef, Input: input},
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err := cli.Create(ctx, run); err != nil {
		fmt.Fprintf(os.Stderr, "run: create AgentRun: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "agentrun %s/%s created\n", *kf.namespace, runName)
	if !*follow {
		fmt.Println(runName)
		return 0
	}
	return followRun(ctx, cli, types.NamespacedName{Namespace: *kf.namespace, Name: runName})
}

// cmdWatch streams an existing AgentRun's status to completion.
func cmdWatch(args []string) int {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	kf := addKubeFlags(fs)
	timeout := fs.Duration("timeout", 15*time.Minute, "max time to watch")
	pos := parseInterleaved(fs, args)
	if len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "watch: exactly one <agentrun> argument is required")
		return 2
	}
	cli, err := kf.client()
	if err != nil {
		fmt.Fprintf(os.Stderr, "watch: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	return followRun(ctx, cli, types.NamespacedName{Namespace: *kf.namespace, Name: pos[0]})
}

// followRun polls the run, printing each state/reason transition, then the
// folded output + usage when terminal. Exit code mirrors the run outcome.
func followRun(ctx context.Context, cli client.Client, key types.NamespacedName) int {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	last := ""
	for {
		run := &amv1.AgentRun{}
		if err := cli.Get(ctx, key, run); err != nil {
			fmt.Fprintf(os.Stderr, "watch: %v\n", err)
			return 1
		}
		cur := fmt.Sprintf("%s/%s", run.Status.State, run.Status.Reason)
		if cur != last {
			fmt.Fprintf(os.Stderr, "%s  state=%s reason=%s\n",
				time.Now().Format(time.TimeOnly), run.Status.State, run.Status.Reason)
			last = cur
		}
		if run.Status.State.Terminal() {
			if len(run.Status.Output) > 0 {
				fmt.Println(string(run.Status.Output))
			}
			fmt.Fprintf(os.Stderr, "usage: steps=%d tokens=%d wallclock=%s cost=%dmUSD\n",
				run.Status.Usage.Steps, run.Status.Usage.Tokens,
				run.Status.Usage.WallClockUsed, run.Status.Usage.CostUSDMilli)
			if run.Status.State == pure.PhaseCompleted {
				return 0
			}
			fmt.Fprintf(os.Stderr, "run %s: %s\n", run.Status.State, run.Status.TerminationReason)
			return 1
		}
		select {
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "watch: timed out; last state=%s reason=%s\n", run.Status.State, run.Status.Reason)
			return 1
		case <-tick.C:
		}
	}
}

// cmdLogs prints (or follows) the run pod's logs. The run pod shares the
// AgentRun's name.
func cmdLogs(args []string) int {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	kf := addKubeFlags(fs)
	follow := fs.Bool("follow", false, "stream logs")
	container := fs.String("c", "", "container name (default: first container)")
	pos := parseInterleaved(fs, args)
	if len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "logs: exactly one <agentrun> argument is required")
		return 2
	}
	cfg, err := kf.restConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "logs: %v\n", err)
		return 1
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "logs: %v\n", err)
		return 1
	}
	req := cs.CoreV1().Pods(*kf.namespace).GetLogs(pos[0], &corev1.PodLogOptions{
		Follow: *follow, Container: *container,
	})
	rc, err := req.Stream(context.Background())
	if err != nil {
		if apierrors.IsNotFound(err) {
			fmt.Fprintf(os.Stderr, "logs: pod %s/%s not found (run not started, or already cleaned up)\n", *kf.namespace, pos[0])
		} else {
			fmt.Fprintf(os.Stderr, "logs: %v\n", err)
		}
		return 1
	}
	defer rc.Close()
	_, _ = io.Copy(os.Stdout, rc)
	return 0
}
