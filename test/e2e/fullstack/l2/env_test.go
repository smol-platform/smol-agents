//go:build e2e_l2

package l2

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/stigen/smol-agents/test/e2e/fullstack/shared"
)

// fakeSSM is a deterministic ssmAPI for unit tests. Each SendCommand
// returns a fresh CommandId; the same id then resolves through
// GetCommandInvocation according to invocations[id]. Callers append
// invocation outcomes via queue() before triggering Apply/Exec.
type fakeSSM struct {
	mu          sync.Mutex
	commands    []*ssm.SendCommandInput
	nextID      int
	invocations map[string][]*ssm.GetCommandInvocationOutput
	sendErr     error
}

func newFakeSSM() *fakeSSM {
	return &fakeSSM{invocations: map[string][]*ssm.GetCommandInvocationOutput{}}
}

func (f *fakeSSM) SendCommand(_ context.Context, in *ssm.SendCommandInput, _ ...func(*ssm.Options)) (*ssm.SendCommandOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	f.commands = append(f.commands, in)
	f.nextID++
	id := fmt.Sprintf("cmd-%d", f.nextID)
	return &ssm.SendCommandOutput{Command: &ssmtypes.Command{CommandId: aws.String(id)}}, nil
}

func (f *fakeSSM) GetCommandInvocation(_ context.Context, in *ssm.GetCommandInvocationInput, _ ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	queue := f.invocations[aws.ToString(in.CommandId)]
	if len(queue) == 0 {
		// Default Success keeps tests that don't enumerate the
		// poll loop concise.
		return &ssm.GetCommandInvocationOutput{
			Status: ssmtypes.CommandInvocationStatusSuccess,
		}, nil
	}
	out := queue[0]
	f.invocations[aws.ToString(in.CommandId)] = queue[1:]
	return out, nil
}

// queue enqueues outcomes for the next SendCommand-issued CommandId.
// Each outcome is read in order by GetCommandInvocation polls.
func (f *fakeSSM) queue(outcomes ...*ssm.GetCommandInvocationOutput) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fmt.Sprintf("cmd-%d", f.nextID+1)
	f.invocations[id] = append(f.invocations[id], outcomes...)
}

func successOut(stdout string) *ssm.GetCommandInvocationOutput {
	return &ssm.GetCommandInvocationOutput{
		Status:                ssmtypes.CommandInvocationStatusSuccess,
		StandardOutputContent: aws.String(stdout),
	}
}

func failedOut(stderr string) *ssm.GetCommandInvocationOutput {
	return &ssm.GetCommandInvocationOutput{
		Status:                ssmtypes.CommandInvocationStatusFailed,
		StandardErrorContent:  aws.String(stderr),
		StandardOutputContent: aws.String(""),
	}
}

func newTestEnv(f *fakeSSM) *l2Env {
	return &l2Env{ssm: f, instanceID: "i-test"}
}

func TestEnv_Apply_PipesManifestThroughKubectlApply(t *testing.T) {
	f := newFakeSSM()
	f.queue(successOut("namespace/tenant-a created\n"))

	env := newTestEnv(f)
	if err := env.Apply(context.Background(),
		[]byte("apiVersion: v1\nkind: Namespace\nmetadata: { name: tenant-a }\n")); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := len(f.commands); got != 1 {
		t.Fatalf("want 1 SendCommand, got %d", got)
	}
	cmd := f.commands[0].Parameters["commands"][0]
	for _, want := range []string{
		"k0s kubectl apply -f -",
		"EOFMANIFEST",
		"kind: Namespace",
		"name: tenant-a",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("Apply payload missing %q\nfull payload:\n%s", want, cmd)
		}
	}
	if got := aws.ToString(f.commands[0].DocumentName); got != ssmDocRunShell {
		t.Errorf("DocumentName: got %q want %q", got, ssmDocRunShell)
	}
}

func TestEnv_Apply_SurfacesKubectlFailure(t *testing.T) {
	f := newFakeSSM()
	f.queue(failedOut("error: the namespace already exists"))

	env := newTestEnv(f)
	err := env.Apply(context.Background(), []byte("dummy"))
	if err == nil || !strings.Contains(err.Error(), "kubectl apply") {
		t.Errorf("want kubectl-apply wrap, got %v", err)
	}
}

func TestEnv_Exec_DirectKubectlWithoutPod(t *testing.T) {
	f := newFakeSSM()
	f.queue(successOut("agent-tenant-a   1/1   Running\n"))

	env := newTestEnv(f)
	out, err := env.Exec(context.Background(), shared.ExecTarget{},
		"-n", "tenant-a", "get", "pods")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(string(out), "Running") {
		t.Errorf("output not propagated: %q", out)
	}

	got := f.commands[0].Parameters["commands"][0]
	want := "k0s kubectl -n tenant-a get pods"
	if got != want {
		t.Errorf("Exec payload: got %q want %q", got, want)
	}
}

func TestEnv_Exec_KubectlExecWithPodAndContainer(t *testing.T) {
	f := newFakeSSM()
	f.queue(successOut("hello-world\n"))

	env := newTestEnv(f)
	out, err := env.Exec(context.Background(),
		shared.ExecTarget{Namespace: "tenant-a", Pod: "agent-x", Container: "runtime"},
		"echo", "hello world")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(string(out), "hello-world") {
		t.Errorf("output not propagated: %q", out)
	}

	got := f.commands[0].Parameters["commands"][0]
	if !strings.HasPrefix(got, "k0s kubectl exec -n tenant-a -c runtime agent-x -- ") {
		t.Errorf("missing kubectl-exec prefix: %q", got)
	}
	if !strings.Contains(got, "'hello world'") {
		t.Errorf("space-containing arg not single-quoted: %q", got)
	}
}

func TestEnv_RunSSM_PollsUntilTerminal(t *testing.T) {
	f := newFakeSSM()
	// First poll = InProgress, second = Success: runSSM must keep
	// polling until it sees a terminal status.
	f.queue(
		&ssm.GetCommandInvocationOutput{Status: ssmtypes.CommandInvocationStatusInProgress},
		&ssm.GetCommandInvocationOutput{
			Status:                ssmtypes.CommandInvocationStatusSuccess,
			StandardOutputContent: aws.String("done\n"),
		},
	)

	env := newTestEnv(f)
	out, err := env.runSSM(context.Background(), "echo done", 5*time.Second)
	if err != nil {
		t.Fatalf("runSSM: %v", err)
	}
	if string(out) != "done\n" {
		t.Errorf("unexpected stdout: %q", out)
	}
}

func TestEnv_RunSSM_SurfacesSendCommandError(t *testing.T) {
	f := newFakeSSM()
	f.sendErr = errors.New("Throttling: rate exceeded")

	env := newTestEnv(f)
	_, err := env.runSSM(context.Background(), "true", 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "SendCommand") {
		t.Errorf("want SendCommand-wrapped error, got %v", err)
	}
}
