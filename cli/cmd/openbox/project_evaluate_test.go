package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/evaluate"
)

const testEvaluateAgent = "c59e95b6-2a4e-44a7-8c43-b69bfa77667e"

func TestParseProjectEvaluateArgs(t *testing.T) {
	complete := []string{"--image", "example:local", "--env-file", "evaluation.env", "--openbox-agent", testEvaluateAgent}
	with := func(extra ...string) []string { return append(append([]string{}, complete...), extra...) }
	tests := []struct {
		name     string
		args     []string
		ok       bool
		wait     bool
		contains string
	}{
		{name: "complete", args: complete, ok: true},
		{name: "wait", args: with("--wait"), ok: true, wait: true},
		{name: "wait explicitly false", args: with("--wait=false"), ok: true},
		{name: "output is gone", args: with("--output", "result"), contains: "flag provided but not defined: -output"},
		{name: "missing", args: complete[2:], contains: "requires --image"},
		{name: "duplicate", args: with("--image", "other:local"), contains: "may be specified only once"},
		{name: "positional", args: with("extra"), contains: "rejects positional"},
		{name: "empty", args: []string{"--image=", "--env-file=x", "--openbox-agent=x"}, contains: "requires --image"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a, stdout, stderr := testApp(nil)
			options, code, ok := a.parseProjectEvaluateArgs(test.args)
			if ok != test.ok || (ok && code != exitOK) || (!ok && code != exitError) {
				t.Fatalf("ok=%v code=%d", ok, code)
			}
			if ok && (options.wait != test.wait || options.image != "example:local" || options.openboxAgent != testEvaluateAgent) {
				t.Fatalf("options=%+v", options)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout=%q", stdout.String())
			}
			if test.contains != "" && !strings.Contains(stderr.String(), test.contains) {
				t.Fatalf("stderr=%q", stderr.String())
			}
			if test.contains != "" && !strings.Contains(stderr.String(), "[--wait]") {
				t.Fatalf("usage was not printed: %q", stderr.String())
			}
		})
	}
}

func TestProjectEvaluateUsageHasNoOutput(t *testing.T) {
	a, _, stderr := testApp(nil)
	if code := a.run([]string{"project", "evaluate", "-h"}); code != exitOK {
		t.Fatalf("code=%d", code)
	}
	if strings.Contains(stderr.String(), "-output") || !strings.Contains(stderr.String(), "openbox project evaluate --image IMAGE --env-file FILE --openbox-agent AGENT_ID [--wait]") {
		t.Fatalf("usage=%q", stderr.String())
	}
}

func TestProjectEvaluateAdapter(t *testing.T) {
	args := []string{"project", "evaluate", "--image", "example:local", "--env-file", "evaluation.env", "--openbox-agent", testEvaluateAgent}

	a, stdout, stderr := testApp(map[string]string{devconfig.EnvBackendURL: "http://127.0.0.1:3000", devconfig.EnvControlToken: "control-only"})
	var got evaluate.Input
	a.runProjectEvaluation = func(_ context.Context, input evaluate.Input) (evaluate.Result, error) {
		got = input
		return evaluate.Result{Succeeded: true}, nil
	}
	if code := a.run(append(append([]string{}, args...), "--wait")); code != exitOK {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if got.Image != "example:local" || got.EnvFile != "evaluation.env" || got.OpenBoxAgent != testEvaluateAgent ||
		got.BackendURL != "http://127.0.0.1:3000" || got.ControlToken != "control-only" || !got.Wait {
		t.Fatalf("input=%+v", got)
	}
	// The evaluation prints its own lines through the writers it is handed; the
	// command adds nothing on success.
	if got.Stdout != stdout || got.Stderr != stderr || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	// Without --wait the request is made and the command ends.
	a, _, _ = testApp(map[string]string{devconfig.EnvControlToken: "control-only"})
	a.runProjectEvaluation = func(_ context.Context, input evaluate.Input) (evaluate.Result, error) {
		got = input
		return evaluate.Result{Succeeded: true}, nil
	}
	if code := a.run(args); code != exitOK || got.Wait {
		t.Fatalf("exit=%d wait=%v", code, got.Wait)
	}

	// A failure reports its class and message on stderr and exits non-zero.
	a, stdout, stderr = testApp(map[string]string{devconfig.EnvControlToken: "control-only"})
	a.runProjectEvaluation = func(context.Context, evaluate.Input) (evaluate.Result, error) {
		return evaluate.Result{}, errors.New("command failed")
	}
	if code := a.run(args); code != exitError {
		t.Fatalf("exit=%d", code)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "project evaluate failed (internal_error): command failed") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "retained") || strings.Contains(stderr.String(), "output") {
		t.Fatalf("stderr mentions an output directory: %q", stderr.String())
	}
}

// The token requirement is enforced where the run starts, not at the command:
// a real Run with an empty token must refuse before touching anything.
func TestProjectEvaluateWithoutControlTokenExitsNonZero(t *testing.T) {
	a, stdout, stderr := testApp(nil)
	a.runProjectEvaluation = func(ctx context.Context, input evaluate.Input) (evaluate.Result, error) {
		return evaluate.Run(ctx, input, evaluate.Dependencies{GOOS: "darwin", GOARCH: "arm64"})
	}
	code := a.run([]string{"project", "evaluate", "--image", "example:local", "--env-file", "evaluation.env", "--openbox-agent", testEvaluateAgent})
	if code != exitError || stdout.Len() != 0 {
		t.Fatalf("code=%d stdout=%q", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "project evaluate failed") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}
