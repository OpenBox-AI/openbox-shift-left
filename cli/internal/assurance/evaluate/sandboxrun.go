package evaluate

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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
	document := buildSandboxPolicy(state.prepared.argv[0], state.prepared.connector.openBoxProvider, state.relay.Port(), state.effectPorts()...)
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

	// Three channels, decided at parse time by what the project declared rather
	// than by a name this function recognises.
	//
	// The real OPENBOX_API_KEY never leaves the gateway: the policy's
	// credential_binding names the OpenBox provider and the proxy resolves it,
	// so the evaluator holds it in no request it sends. A credential-shaped name
	// the project declared PUBLIC is a deliberate stand-in and goes on the
	// placeholder channel — which is also why no inference provider is attached:
	// an attached provider's credential keys must each be bound to an endpoint
	// in this policy, and one unbound key makes OpenShell fail closed and revoke
	// the whole set, including the OpenBox credential that was correctly bound.
	environment := map[string]string{}
	for name, value := range state.prepared.environment {
		if name == "OPENBOX_API_KEY" {
			continue
		}
		environment[name] = value
	}
	placeholders := map[string]string{}
	for name, value := range state.prepared.placeholders {
		placeholders[name] = value
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
		ExpectedPolicy:         identity,
		Environment:            environment,
		PlaceholderEnvironment: placeholders,
		// Only the OpenBox provider, and only because this policy binds its
		// credential to an endpoint. See the environment split above.
		Providers: []string{state.prepared.connector.openBoxProvider},
		// The image's own entrypoint, as the sandbox's MAIN process. That is
		// what makes the provider credentials reach it at all.
		Command: state.prepared.argv,
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

	// The workload is already running — Begin started it as the main process —
	// so this waits for it rather than launching anything.
	result, err := dependencies.Sandbox.WaitCompleted(begunID, readyToken, sandboxExecDeadline)
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

// sandboxLogs renders the supervisor's records for the workload.
//
// A main process has no exec stream, so this is the workload's output. It is
// written as records rather than flattened into a fake stdout stream, because
// calling it stdout would claim a fidelity the source does not have.
func (state *runState) sandboxLogs() []byte {
	if state.sandboxResult == nil || len(state.sandboxResult.Logs) == 0 {
		return nil
	}
	encoded, err := json.Marshal(state.sandboxResult.Logs)
	if err != nil {
		return nil
	}
	return encoded
}
