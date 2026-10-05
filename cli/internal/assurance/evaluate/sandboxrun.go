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

// The lane owns the document, so it owns the identity: the service attests the
// loaded policy is byte-identical to the one sent, which makes an invented name
// exactly as verifiable as a given one.
const sandboxPolicyIdentity = "openbox-project-evaluation"

// runThroughSandbox executes the image's own entrypoint through the sandbox
// service, replacing an attached `openshell sandbox create`, a phase poll and a
// log tail with one typed result.
func (state *runState) runThroughSandbox(ctx context.Context, dependencies Dependencies) error {
	if dependencies.Sandbox == nil {
		return fail("not_runnable", "project evaluate: no sandbox service is configured")
	}
	document := buildSandboxPolicy(state.prepared.argv[0], state.prepared.connector.openBoxProvider, state.relay.Port())
	digest := sha256.Sum256(document)
	identity := sandboxclient.PolicyIdentity{
		ID:      sandboxPolicyIdentity,
		Version: 1,
		SHA256:  hex.EncodeToString(digest[:]),
	}

	runID, err := sandboxclient.NewRunID()
	if err != nil {
		return fail("not_runnable", "project evaluate: generate sandbox run identity")
	}
	state.sandboxRunID = runID

	// OPENBOX_API_KEY is held out because the policy binds it: the proxy
	// resolves it, so the evaluator sends it in no request. Everything else is
	// ordinary guest environment, warned about by name before the run starts.
	environment := map[string]string{}
	for name, value := range state.prepared.environment {
		if name == "OPENBOX_API_KEY" {
			continue
		}
		environment[name] = value
	}

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
		// Only what this policy binds. An attached provider with an unbound
		// credential key makes OpenShell fail closed and revoke the whole set.
		Providers: []string{state.prepared.connector.openBoxProvider},
		// MAIN process, not an exec: only the main process gets provider env.
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

	// Begin already started it; this waits rather than launching.
	result, err := dependencies.Sandbox.WaitCompleted(begunID, readyToken, sandboxExecDeadline)
	if err != nil {
		return &classifiedError{class: "command_failure", err: err}
	}
	if ctx.Err() != nil {
		return &classifiedError{class: contextClassification(ctx.Err()), err: errors.New("project evaluate: interrupted while the image command was running")}
	}
	if result.ExitCode != 0 {
		return failf("command_nonzero", "project evaluate: image command exited with status %d", result.ExitCode)
	}
	return nil
}

// deleteSandbox asks for deletion and then requires terminal absence: an
// accepted delete is not proof, and a leaked sandbox must fail the run.
func (state *runState) deleteSandbox(dependencies Dependencies) error {
	if dependencies.Sandbox == nil || state.sandboxRunID == "" {
		return nil
	}
	deleteErr := dependencies.Sandbox.Delete(state.sandboxRunID, sandboxDeleteTimeout)
	if dependencies.Sandbox.WaitDeleted(state.sandboxRunID, sandboxDeleteTimeout) == nil {
		return nil
	}
	if deleteErr != nil {
		return fmt.Errorf("sandbox delete failed and absence was not proven: %w", deleteErr)
	}
	return errors.New("sandbox remains after cleanup")
}
