package evaluate

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/sandboxclient"
)

// sandboxPolicyIdentity names the policy this lane generates.
//
// The lane owns the document, so it owns the identity: the service attests that
// the policy it loaded is byte-identical to the one sent, and a name the caller
// invented is exactly as verifiable as one it was given.
const sandboxPolicyIdentity = "openbox-project-evaluation"

// runThroughSandbox executes the image's own entrypoint through the sandbox
// service and retains the typed result.
//
// This replaces an attached `openshell sandbox create`, a phase poll over
// `openshell sandbox get -o json`, and a log tail over `openshell logs`. What
// the lane gets back instead of scraped text is an exit code, bounded output,
// and the provider's own isolation evidence — the difference between reading a
// gateway's prose and being told what it decided.
func (state *runState) runThroughSandbox(ctx context.Context, dependencies Dependencies) error {
	if dependencies.Sandbox == nil {
		return fail("not_runnable", "project evaluate: no sandbox service is configured")
	}
	document := buildSandboxPolicy(state.prepared.argv[0], state.relay.Port(), state.effectPorts()...)
	digest := sha256.Sum256(document)
	identity := sandboxclient.PolicyIdentity{
		ID:      sandboxPolicyIdentity,
		Version: 1,
		SHA256:  hex.EncodeToString(digest[:]),
	}
	state.policy = document
	if err := state.workspace.WritePrivateFile("policy.yaml", document); err != nil {
		return &classifiedError{class: "output_failure", err: err}
	}
	state.policyWritten = true

	runID, err := sandboxclient.NewRunID()
	if err != nil {
		return fail("not_runnable", "project evaluate: generate sandbox run identity")
	}
	state.sandboxRunID = runID

	// The API key is never sent. It reaches the guest through the provider the
	// policy binds, so the evaluator holds no credential to leak.
	environment := map[string]string{}
	for name, value := range state.prepared.environment {
		if name == "OPENBOX_API_KEY" {
			continue
		}
		environment[name] = value
	}

	state.phase(dependencies, "sandbox_creating")
	state.sandboxMayExist = true
	begunID, token, err := dependencies.Sandbox.Begin(sandboxclient.ProjectRunSpec{
		RunID:    runID,
		Template: state.immutableReference,
		PolicyDocument: sandboxclient.PolicyDocument{
			MediaType: "application/yaml",
			Base64:    base64.StdEncoding.EncodeToString(document),
		},
		ExpectedPolicy: identity,
		Environment:    environment,
		Providers:      []string{OpenBoxProvider},
	}, sandboxBeginDeadline)
	if err != nil {
		return &classifiedError{class: "sandbox_create_failure", err: err}
	}
	state.sandboxRunID = begunID

	readyToken, err := dependencies.Sandbox.WaitReady(begunID, token, identity, sandboxReadyDeadline)
	if err != nil {
		return &classifiedError{class: "readiness_failure", err: err}
	}
	state.phase(dependencies, "ready")

	result, err := dependencies.Sandbox.Exec(begunID, readyToken, state.prepared.argv,
		sandboxCommandTimeout, sandboxclient.DefaultOutputLimits(), sandboxExecDeadline)
	if err != nil {
		return &classifiedError{class: "command_failure", err: err}
	}
	state.sandboxResult = result
	state.record.CommandExitCode = intPointer(result.ExitCode)
	state.phase(dependencies, "command_exited")
	if ctx.Err() != nil {
		return &classifiedError{class: contextClassification(ctx.Err()), err: errors.New("project evaluate: interrupted while the image command was running")}
	}
	if result.ExitCode != 0 {
		return failf("command_nonzero", "project evaluate: image command exited with status %d", result.ExitCode)
	}
	return nil
}

func (state *runState) effectPorts() []int {
	if state.effectRelay == nil {
		return nil
	}
	return []int{state.effectRelay.Port()}
}

// deleteSandbox asks for deletion and then requires terminal absence.
//
// Two calls because they answer different questions: an accepted delete is not
// proof the sandbox is gone, and this lane's cleanup record claims absence.
func (state *runState) deleteSandbox(dependencies Dependencies) error {
	if dependencies.Sandbox == nil || state.sandboxRunID == "" {
		return nil
	}
	state.record.Cleanup.SandboxDeleteAttempted = true
	deleteErr := dependencies.Sandbox.Delete(state.sandboxRunID, sandboxDeleteTimeout)
	absent := dependencies.Sandbox.WaitDeleted(state.sandboxRunID, sandboxDeleteTimeout) == nil
	state.record.Cleanup.SandboxAbsent = absent
	if absent {
		return nil
	}
	if deleteErr != nil {
		return fmt.Errorf("sandbox delete failed and absence was not proven: %w", deleteErr)
	}
	return errors.New("sandbox remains after cleanup")
}

// The typed result replaces three scraped artifacts: the attached process's
// stdout and stderr, and the gateway log tail. Absent before a run completes,
// which callers must treat as "not observed" rather than empty.
func (state *runState) sandboxStdout() []byte {
	if state.sandboxResult == nil {
		return nil
	}
	return state.sandboxResult.Stdout
}

func (state *runState) sandboxStderr() []byte {
	if state.sandboxResult == nil {
		return nil
	}
	return state.sandboxResult.Stderr
}

func (state *runState) sandboxEvidence() []byte {
	if state.sandboxResult == nil || len(state.sandboxResult.SandboxEvidence) == 0 {
		return nil
	}
	return state.sandboxResult.SandboxEvidence
}
