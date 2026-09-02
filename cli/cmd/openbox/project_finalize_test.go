package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/securityreport"
)

func TestProjectFinalizeOfflineFailurePrecedesCredentialAndRunner(t *testing.T) {
	var stdout, stderr bytes.Buffer
	credentialReads := 0
	runnerCalls := 0
	a := &app{
		stdout: &stdout, stderr: &stderr,
		getenv: func(string) string { credentialReads++; return "must-not-be-read" },
		runProjectFinalization: func(context.Context, *securityreport.Prepared, securityreport.RuntimeInput) (securityreport.Result, error) {
			runnerCalls++
			return securityreport.Result{}, nil
		},
	}
	code := a.runProjectFinalize([]string{"--evaluation", filepath.Join(t.TempDir(), "missing"), "--analysis", filepath.Join(t.TempDir(), "missing.json"), "--output", filepath.Join(t.TempDir(), "report")})
	if code != exitError || credentialReads != 0 || runnerCalls != 0 || stdout.Len() != 0 {
		t.Fatalf("offline failure crossed authority boundary: code=%d reads=%d runs=%d stdout=%q stderr=%q", code, credentialReads, runnerCalls, stdout.String(), stderr.String())
	}
}

func TestProjectFinalizeExactFlagsAndSuccessOutput(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	observation := filepath.Join(root, "plans/260825-1623-lean-openshell-project-assurance/evidence/2026-08-26-phase-02-public-mastra-dashboard-observation-04")
	sourceCandidate := filepath.Join(root, "plans/260825-1623-lean-openshell-project-assurance/evidence/2026-08-27-phase-03-installed-codex-candidate.json")
	content, err := os.ReadFile(sourceCandidate)
	if err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(t.TempDir(), "candidate.json")
	if err := os.WriteFile(candidate, content, 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "report")
	var stdout, stderr bytes.Buffer
	runnerCalls := 0
	a := &app{
		stdout: &stdout, stderr: &stderr,
		getenv: func(name string) string {
			switch name {
			case "OPENBOX_BACKEND_URL":
				return "http://127.0.0.1:3000"
			case "OPENBOX_CONTROL_TOKEN":
				return "control-token"
			default:
				return ""
			}
		},
		runProjectFinalization: func(_ context.Context, prepared *securityreport.Prepared, input securityreport.RuntimeInput) (securityreport.Result, error) {
			runnerCalls++
			if prepared.Candidate.Result != "no_supported_issue" || input.BackendURL != "http://127.0.0.1:3000" || input.ControlToken != "control-token" {
				t.Fatalf("wrong finalizer input: %#v %#v", prepared, input)
			}
			return securityreport.Result{Output: prepared.OutputPath, PackDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, nil
		},
	}
	code := a.runProjectFinalize([]string{"--evaluation", observation, "--analysis", candidate, "--output", output})
	if code != exitOK || runnerCalls != 1 || stderr.Len() != 0 {
		t.Fatalf("finalize failed: code=%d calls=%d stderr=%q", code, runnerCalls, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "project security report sealed: ") || !strings.HasSuffix(stdout.String(), "\n  pack_digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n") {
		t.Fatalf("unexpected success output: %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := a.runProjectFinalize([]string{"--evaluation", observation, "--evaluation", observation, "--analysis", candidate, "--output", output}); code != exitError || runnerCalls != 1 {
		t.Fatalf("duplicate flag was accepted: code=%d calls=%d", code, runnerCalls)
	}
}
