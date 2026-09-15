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
	observation := readableObservationPack(t, filepath.Join(root, "cli/internal/assurance/testdata/observation-v2"))
	sourceCandidate := filepath.Join(root, "cli/internal/assurance/testdata/observation-v2-candidates/2026-08-27-phase-03-installed-codex-candidate.json")
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

// readableObservationPack reproduces a committed evidence pack at the modes the
// observation reader requires, and returns the copy.
//
// The working-tree path cannot be used directly: git materializes directories
// at 0755 and files at 0644, while runfs demands an exact 0500 root of 0400
// files, so these tests failed on a fresh checkout with "run root mode 0755 is
// not recoverable". Copying also keeps the tests from mutating committed
// evidence to make themselves pass.
func readableObservationPack(t *testing.T, source string) string {
	t.Helper()
	destination := filepath.Join(t.TempDir(), "observation")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("committed pack holds directory %s", entry.Name())
		}
		content, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(destination, entry.Name()), content, 0o400); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(destination, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(destination, 0o700) })
	return destination
}
