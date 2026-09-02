package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/evaluate"
)

func TestParseProjectEvaluateArgs(t *testing.T) {
	complete := []string{"--image", "example:local", "--env-file", "evaluation.env", "--openbox-agent", "c59e95b6-2a4e-44a7-8c43-b69bfa77667e", "--output", "result"}
	tests := []struct {
		name     string
		args     []string
		ok       bool
		contains string
	}{
		{name: "complete", args: complete, ok: true},
		{name: "missing", args: complete[2:], contains: "requires --image"},
		{name: "duplicate", args: append(append([]string{}, complete...), "--image", "other:local"), contains: "may be specified only once"},
		{name: "positional", args: append(append([]string{}, complete...), "extra"), contains: "rejects positional"},
		{name: "empty", args: []string{"--image=", "--env-file=x", "--openbox-agent=x", "--output=x"}, contains: "requires --image"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a, stdout, stderr := testApp(nil)
			_, code, ok := a.parseProjectEvaluateArgs(test.args)
			if ok != test.ok || (ok && code != exitOK) || (!ok && code != exitError) {
				t.Fatalf("ok=%v code=%d", ok, code)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout=%q", stdout.String())
			}
			if test.contains != "" && !strings.Contains(stderr.String(), test.contains) {
				t.Fatalf("stderr=%q", stderr.String())
			}
		})
	}
}

func TestProjectEvaluateAdapter(t *testing.T) {
	a, stdout, stderr := testApp(map[string]string{devconfig.EnvBackendURL: "http://127.0.0.1:3000", devconfig.EnvControlToken: "control-only"})
	var got evaluate.Input
	a.runProjectEvaluation = func(_ context.Context, input evaluate.Input) (evaluate.Result, error) {
		got = input
		return evaluate.Result{Output: filepath.Join("/tmp", "record"), Succeeded: true}, nil
	}
	args := []string{"--image", "example:local", "--env-file", "evaluation.env", "--openbox-agent", "c59e95b6-2a4e-44a7-8c43-b69bfa77667e", "--output", "result"}
	if code := a.run([]string{"project", "evaluate", args[0], args[1], args[2], args[3], args[4], args[5], args[6], args[7]}); code != exitOK {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if got.Image != "example:local" || got.EnvFile != "evaluation.env" || got.Output != "result" || got.BackendURL != "http://127.0.0.1:3000" || got.ControlToken != "control-only" || !got.ObservationRequired {
		t.Fatalf("input=%+v", got)
	}
	if !strings.Contains(stdout.String(), "project observation sealed") || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	a, stdout, stderr = testApp(nil)
	a.runProjectEvaluation = func(context.Context, evaluate.Input) (evaluate.Result, error) {
		return evaluate.Result{Output: "/tmp/failed"}, errors.New("command failed")
	}
	if code := a.run(append([]string{"project", "evaluate"}, args...)); code != exitError {
		t.Fatalf("exit=%d", code)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "output retained: /tmp/failed") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
