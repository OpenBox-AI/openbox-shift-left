package evaluate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/observation"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/runfs"
)

var manifestDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type runState struct {
	prepared            *prepared
	workspace           *runfs.Workspace
	record              executionRecord
	policy              []byte
	policyWritten       bool
	process             CommandResult
	openShellLog        CommandResult
	logTailCaptured     bool
	relay               *coreRelay
	effectRelay         *effectRelay
	registryStarted     bool
	registryAddress     string
	registryTag         string
	manifestDigest      string
	publishedReference  string
	immutableReference  string
	sandboxMayExist     bool
	observationClient   *observation.Client
	observationSnapshot *observation.Snapshot
	observationResult   *observation.Result
	observationWindow   observation.Window
	phaseMu             sync.Mutex
}

type classifiedError struct {
	class string
	err   error
}

func (failure *classifiedError) Error() string { return failure.err.Error() }
func (failure *classifiedError) Unwrap() error { return failure.err }
func fail(class, message string) error {
	return &classifiedError{class: class, err: errors.New(message)}
}
func failf(class, format string, values ...any) error {
	return &classifiedError{class: class, err: fmt.Errorf(format, values...)}
}

// Run performs preflight without mutations, creates one staging output, then
// enters a single cleanup path for every subsequent outcome.
func Run(ctx context.Context, input Input, dependencies Dependencies) (Result, error) {
	prepared, err := prepare(ctx, input, dependencies)
	if err != nil {
		return Result{}, err
	}
	privateOutput := filepath.Join(filepath.Dir(prepared.output), "."+filepath.Base(prepared.output)+"."+prepared.evaluationID+".private")
	workspace, err := runfs.Create(privateOutput)
	if err != nil {
		return Result{}, fmt.Errorf("project evaluate: create output: %w", err)
	}
	started := dependencies.Clock.Now()
	state := &runState{prepared: prepared, workspace: workspace}
	state.initializeRecord(started)
	state.phase(dependencies, "preflighted")
	state.phase(dependencies, "output_created")

	overall, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	runErr := state.preflightObservation(overall, dependencies)
	if runErr == nil {
		state.observationWindow = observation.Window{
			EvaluationID: prepared.evaluationID,
			StartedAt:    dependencies.Clock.Now(),
			Deadline:     started.Add(7 * time.Minute),
		}
		runErr = state.execute(overall, dependencies)
	}
	if runErr == nil && state.observationClient != nil {
		collection, collectCancel := context.WithDeadline(ctx, state.observationWindow.Deadline)
		state.observationResult, err = state.observationClient.Collect(collection, state.observationWindow)
		collectCancel()
		if err != nil {
			runErr = &classifiedError{class: "not_runnable", err: fmt.Errorf("project evaluate: collect backend observation: %w", err)}
		}
	}
	cleanupErr := state.cleanup(dependencies)
	if cleanupErr != nil {
		runErr = errors.Join(runErr, &classifiedError{class: "cleanup_failure", err: cleanupErr})
	}
	if runErr == nil {
		state.record.ExitClassification = "success"
	} else {
		state.record.ExitClassification = classify(runErr)
		state.record.Error = safeError(runErr)
	}
	state.phase(dependencies, "execution_recorded")
	completed := dependencies.Clock.Now()
	state.record.CompletedAt = completed
	state.record.DurationMS = completed.Sub(started).Milliseconds()
	state.finishRecord()
	writeErr := state.publishOutput(runErr == nil, dependencies)
	if writeErr != nil {
		runErr = errors.Join(runErr, fmt.Errorf("project evaluate: write execution output: %w", writeErr))
	}
	result := Result{EvaluationID: prepared.evaluationID, Output: prepared.output, Succeeded: runErr == nil && writeErr == nil}
	if runErr != nil {
		return result, runErr
	}
	if writeErr != nil {
		return result, writeErr
	}
	return result, nil
}

func (state *runState) preflightObservation(ctx context.Context, dependencies Dependencies) error {
	if !state.prepared.input.ObservationRequired {
		return nil
	}
	if dependencies.BackendHTTP == nil {
		return fail("not_runnable", "project evaluate: backend observation HTTP client is unavailable")
	}
	client, err := observation.New(observation.Config{
		BackendURL:      state.prepared.input.BackendURL,
		ControlToken:    state.prepared.input.ControlToken,
		AgentID:         state.prepared.input.OpenBoxAgent,
		HTTP:            dependencies.BackendHTTP,
		ProxyConfigured: state.prepared.input.ProxyConfigured,
	})
	if err != nil {
		return &classifiedError{class: "not_runnable", err: fmt.Errorf("project evaluate: backend preflight: %w", err)}
	}
	state.observationClient = client
	snapshot, err := client.Preflight(ctx, state.prepared.evaluationID)
	if err != nil {
		return &classifiedError{class: "not_runnable", err: fmt.Errorf("project evaluate: backend preflight: %w", err)}
	}
	state.observationSnapshot = snapshot
	state.phase(dependencies, "backend_preflighted")
	return nil
}

func (state *runState) publishOutput(success bool, dependencies Dependencies) error {
	if !success || state.observationClient == nil {
		if err := state.writeOutput(); err != nil {
			return err
		}
		return state.workspace.PublishTo(state.prepared.output)
	}
	execution, err := json.Marshal(state.record)
	if err != nil {
		return err
	}
	receipt := relayReceipt{}
	if state.relay != nil {
		receipt = state.relay.Receipt()
	}
	effect := effectReceipt{}
	if state.effectRelay != nil {
		effect = state.effectRelay.Receipt()
	}
	pack, err := observation.Assemble(observation.PackInput{
		ExecutionJSON: execution,
		OpenShellLog:  combinedLog(state.openShellLog),
		Snapshot:      state.observationSnapshot,
		Backend:       state.observationResult,
		Window:        state.observationWindow,
		Effects: map[string]any{
			"safe_sink":        map[string]any{"status": statusForReceipt(effect.MatchingReceipts), "attempts": effect.Attempts, "matching_receipts": effect.MatchingReceipts, "evaluation_id": state.prepared.evaluationID, "matched_at": effect.MatchedAt.Format(time.RFC3339Nano)},
			"retrieval_poison": map[string]any{"status": "missing", "matching_receipts": 0},
			"model_route":      map[string]any{"status": "observed", "provider": InferenceProvider, "model": InferenceModel, "model_digest": InferenceModelDigest},
			"core_relay":       map[string]any{"status": "observed", "matching_validations": receipt.MatchingValidations, "governance_events": receipt.GovernanceEvents},
		},
		FinalizedAt: dependencies.Clock.Now(),
	})
	if err != nil {
		return fmt.Errorf("assemble observation pack: %w", err)
	}
	if _, err := state.workspace.Cleanup(); err != nil {
		return fmt.Errorf("remove private diagnostic staging: %w", err)
	}
	packRoot := filepath.Join(filepath.Dir(state.prepared.output), "."+filepath.Base(state.prepared.output)+"."+state.prepared.evaluationID+".pack")
	packWorkspace, err := runfs.Create(packRoot)
	if err != nil {
		return err
	}
	if err := packWorkspace.WriteObservationPayloads(pack.Payloads); err != nil {
		return err
	}
	if _, err := packWorkspace.FinalizeObservation(pack.Payloads, pack.Manifest); err != nil {
		return err
	}
	if err := packWorkspace.PublishTo(state.prepared.output); err != nil {
		return err
	}
	_, err = observation.Read(state.prepared.output)
	return err
}

func statusForReceipt(count int) string {
	if count > 0 {
		return "observed"
	}
	return "missing"
}

func (state *runState) initializeRecord(started time.Time) {
	record := &state.record
	record.Schema = Schema
	record.EvaluationID = state.prepared.evaluationID
	record.AgentID = state.prepared.input.OpenBoxAgent
	record.StartedAt = started
	record.Image.Requested = state.prepared.input.Image
	record.Image.LocalID = state.prepared.image.ID
	record.Image.Platform = state.prepared.image.OS + "/" + state.prepared.image.Architecture
	record.Image.WorkingDir = state.prepared.image.Config.WorkingDir
	record.Argv = append([]string(nil), state.prepared.argv...)
	record.EnvironmentNames = append([]string(nil), state.prepared.environmentNames...)
	record.OpenShell.CLIVersion = OpenShellVersion
	record.OpenShell.GatewayVersion = state.prepared.openShellGateway
	record.OpenShell.DriverVersion = state.prepared.openShellDriver
	record.OpenShell.Provider = OpenBoxProvider
	record.Inference.Provider = InferenceProvider
	record.Inference.Model = InferenceModel
	record.Inference.ModelDigest = InferenceModelDigest
	record.CoverageLimitations = []string{
		"development observation only; not a production confinement or enforcement qualification",
		"Core relay credential use is bound to the validated OCI entrypoint executable",
		"landlock best_effort may run without filesystem enforcement",
		"runtime cgroup pids.max availability is not guaranteed",
		"OpenBox evaluation agent uses provider-bound bearer authentication without SDK request signing",
	}
}

func (state *runState) phase(dependencies Dependencies, phase string) {
	state.phaseMu.Lock()
	defer state.phaseMu.Unlock()
	state.record.Phases = append(state.record.Phases, phaseEntry{Phase: phase, At: dependencies.Clock.Now()})
}

func (state *runState) execute(ctx context.Context, dependencies Dependencies) error {
	if err := loadInferenceModel(ctx, dependencies.inferenceHTTP()); err != nil {
		return &classifiedError{class: "model_load_failure", err: err}
	}
	state.phase(dependencies, "model_loaded")

	publication, cancel := context.WithTimeout(ctx, 120*time.Second)
	err := state.startRegistry(publication, dependencies)
	if err == nil {
		state.phase(dependencies, "registry_started")
	}
	if err == nil {
		err = state.publishImage(publication, dependencies)
	}
	cancel()
	if err != nil {
		return err
	}
	state.phase(dependencies, "image_published")

	relay, err := startCoreRelay(dependencies, state.prepared.input.OpenBoxAgent, state.prepared.evaluationID)
	if err != nil {
		return &classifiedError{class: "core_relay_failure", err: err}
	}
	state.relay = relay
	state.prepared.environment["OPENBOX_URL"] = fmt.Sprintf("http://host.openshell.internal:%d", relay.Port())
	state.prepared.environmentNames = environmentInventory(state.prepared.environment)
	state.record.EnvironmentNames = append([]string(nil), state.prepared.environmentNames...)
	state.phase(dependencies, "core_relay_started")
	if state.observationClient != nil {
		effectRelay, effectErr := startEffectRelay(dependencies, state.prepared.evaluationID)
		if effectErr != nil {
			return &classifiedError{class: "receipt_service_failure", err: errors.New("project evaluate: start safe effect receipt service")}
		}
		state.effectRelay = effectRelay
		state.prepared.environment["OPENBOX_SAFE_SINK_URL"] = fmt.Sprintf("http://host.openshell.internal:%d/effects/safe", effectRelay.Port())
		state.prepared.environmentNames = environmentInventory(state.prepared.environment)
		state.record.EnvironmentNames = append([]string(nil), state.prepared.environmentNames...)
		state.phase(dependencies, "safe_effect_sink_started")
		state.policy, err = buildPolicy(state.prepared.applicationRoot, state.prepared.argv[0], relay.Port(), effectRelay.Port())
	} else {
		state.policy, err = buildPolicy(state.prepared.applicationRoot, state.prepared.argv[0], relay.Port())
	}
	if err != nil {
		return &classifiedError{class: "policy_failure", err: err}
	}
	if err := state.workspace.WritePrivateFile("policy.json", state.policy); err != nil {
		return &classifiedError{class: "output_failure", err: err}
	}
	state.policyWritten = true

	state.phase(dependencies, "sandbox_creating")
	state.sandboxMayExist = true
	commandErr := state.runSandbox(ctx, dependencies)
	logErr := state.captureLogs(ctx, dependencies)
	if logErr == nil {
		state.phase(dependencies, "logs_captured")
	}
	if commandErr != nil {
		return commandErr
	}
	if logErr != nil {
		return logErr
	}
	receipt := relay.Receipt()
	if receipt.MatchingValidations < 1 || receipt.GovernanceEvents < 1 {
		return fail("observation_failure", "project evaluate: required matching SDK validation and evaluation governance event were not observed")
	}
	return nil
}

func loadInferenceModel(ctx context.Context, client HTTPDoer) error {
	body := strings.NewReader(`{"model":"` + InferenceModel + `","keep_alive":"5m"}`)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, ollamaGenerateURL, body)
	if err != nil {
		return fmt.Errorf("project evaluate: construct Ollama model load request: %w", err)
	}
	request.Header.Set("content-type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return errors.New("project evaluate: load local Granite model")
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(content) > 64<<10 || response.StatusCode != http.StatusOK {
		return errors.New("project evaluate: load local Granite model")
	}
	var result struct {
		Model      string `json:"model"`
		Done       bool   `json:"done"`
		DoneReason string `json:"done_reason"`
	}
	if json.Unmarshal(content, &result) != nil || result.Model != InferenceModel || !result.Done || result.DoneReason != "load" {
		return errors.New("project evaluate: Ollama returned an invalid model load receipt")
	}
	return nil
}

func (state *runState) runSandbox(ctx context.Context, dependencies Dependencies) error {
	command := state.sandboxCreateCommand()
	processContext, cancel := context.WithCancel(ctx)
	defer cancel()
	process, err := dependencies.Commands.Start(processContext, command)
	if err != nil {
		if ctx.Err() != nil {
			return &classifiedError{class: contextClassification(ctx.Err()), err: errors.New("project evaluate: interrupted before attached OpenShell execution started")}
		}
		return &classifiedError{class: "sandbox_create_failure", err: errors.New("project evaluate: start attached OpenShell sandbox create failed")}
	}
	resultChannel := make(chan CommandResult, 1)
	go func() { resultChannel <- process.Wait() }()
	readyDeadline := dependencies.Clock.Now().Add(90 * time.Second)
	ready := false
	for !ready {
		select {
		case result := <-resultChannel:
			state.process = result
			state.record.CommandExitCode = intPointer(result.ExitCode)
			state.phase(dependencies, "command_exited")
			if ctx.Err() != nil {
				return &classifiedError{class: contextClassification(ctx.Err()), err: errors.New("project evaluate: interrupted while waiting for sandbox readiness")}
			}
			return failf("sandbox_pre_ready_error", "project evaluate: attached command exited before sandbox Ready with status %d", result.ExitCode)
		default:
		}
		phase, exists, phaseErr := state.sandboxPhase(ctx, dependencies)
		if phaseErr == nil && exists {
			switch phase {
			case "Provisioning":
			case "Ready":
				ready = true
				state.phase(dependencies, "ready")
			case "Error":
				cancel()
				state.process = <-resultChannel
				state.record.CommandExitCode = intPointer(state.process.ExitCode)
				return fail("sandbox_pre_ready_error", "project evaluate: sandbox entered Error before Ready")
			default:
				return failf("sandbox_phase_error", "project evaluate: unexpected sandbox phase %q before Ready", phase)
			}
		} else if phaseErr != nil && !isNotFound(phaseErr) {
			return &classifiedError{class: "sandbox_status_failure", err: phaseErr}
		}
		if dependencies.Clock.Now().After(readyDeadline) {
			cancel()
			state.process = <-resultChannel
			state.record.CommandExitCode = intPointer(state.process.ExitCode)
			return fail("readiness_timeout", "project evaluate: sandbox did not become Ready within 90 seconds")
		}
		if err := dependencies.Clock.Sleep(ctx, 250*time.Millisecond); err != nil {
			cancel()
			state.process = <-resultChannel
			state.record.CommandExitCode = intPointer(state.process.ExitCode)
			return &classifiedError{class: contextClassification(err), err: errors.New("project evaluate: interrupted while waiting for sandbox readiness")}
		}
	}

	executionDeadline := dependencies.Clock.Now().Add(180 * time.Second)
	logContext, logCancel := context.WithCancel(ctx)
	logProcess, logStartErr := dependencies.Commands.Start(logContext, Command{Name: "openshell", Args: []string{
		"logs", "--tail", "--source", "all", state.prepared.sandboxName,
	}})
	if logStartErr != nil {
		logCancel()
		return fail("log_collection_failure", "project evaluate: start live OpenShell log collection failed")
	}
	logResultChannel := make(chan CommandResult, 1)
	go func() { logResultChannel <- logProcess.Wait() }()
	state.logTailCaptured = true
	defer func() {
		logCancel()
		state.openShellLog = <-logResultChannel
	}()
	for {
		select {
		case result := <-resultChannel:
			state.process = result
			state.record.CommandExitCode = intPointer(result.ExitCode)
			state.phase(dependencies, "command_exited")
			if ctx.Err() != nil {
				return &classifiedError{class: contextClassification(ctx.Err()), err: errors.New("project evaluate: interrupted while image command was running")}
			}
			if result.ExitCode != 0 {
				return failf("command_nonzero", "project evaluate: image command exited with status %d", result.ExitCode)
			}
			return nil
		default:
		}
		if dependencies.Clock.Now().After(executionDeadline) {
			cancel()
			state.process = <-resultChannel
			state.record.CommandExitCode = intPointer(state.process.ExitCode)
			return fail("command_timeout", "project evaluate: image command exceeded 180 seconds")
		}
		if err := dependencies.Clock.Sleep(ctx, 250*time.Millisecond); err != nil {
			cancel()
			state.process = <-resultChannel
			state.record.CommandExitCode = intPointer(state.process.ExitCode)
			return &classifiedError{class: contextClassification(err), err: errors.New("project evaluate: interrupted while image command was running")}
		}
	}
}

func (state *runState) sandboxCreateCommand() Command {
	args := []string{
		"sandbox", "create",
		"--name", state.prepared.sandboxName,
		"--from", state.immutableReference,
		"--policy", filepath.Join(state.workspace.Root(), "policy.json"),
		"--provider", OpenBoxProvider,
		"--no-auto-providers", "--no-tty", "--no-keep",
		"--cpu", "2", "--memory", "2Gi",
		"--label", "ai.openbox.evaluation-id=" + state.prepared.evaluationID,
	}
	for _, name := range sortedEnvironmentNames(state.prepared.environment) {
		if name == "OPENBOX_API_KEY" {
			continue
		}
		args = append(args, "--env", name+"="+state.prepared.environment[name])
	}
	args = append(args, "--")
	args = append(args, state.prepared.argv...)
	return Command{Name: "openshell", Args: args}
}

func (state *runState) captureLogs(ctx context.Context, dependencies Dependencies) error {
	if state.logTailCaptured {
		return nil
	}
	logContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := dependencies.Commands.Run(logContext, Command{Name: "openshell", Args: []string{"logs", "--source", "all", state.prepared.sandboxName}})
	state.openShellLog = result
	if err != nil {
		return fail("log_collection_failure", "project evaluate: OpenShell log collection failed")
	}
	return nil
}

func (state *runState) sandboxPhase(ctx context.Context, dependencies Dependencies) (string, bool, error) {
	result, err := dependencies.Commands.Run(ctx, Command{Name: "openshell", Args: []string{"sandbox", "get", state.prepared.sandboxName, "-o", "json"}})
	if err != nil {
		return "", false, fmt.Errorf("project evaluate: inspect sandbox: %w: %s", err, boundedDiagnostic(result.Stderr))
	}
	phase := findPhase(result.Stdout)
	if phase == "" {
		return "", true, errors.New("project evaluate: sandbox JSON has no phase")
	}
	return phase, true, nil
}

func findPhase(content []byte) string {
	var value any
	if json.Unmarshal(content, &value) != nil {
		return ""
	}
	return findPhaseValue(value)
}

func findPhaseValue(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		if phase, ok := typed["phase"].(string); ok {
			return phase
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if phase := findPhaseValue(typed[key]); phase != "" {
				return phase
			}
		}
	case []any:
		for _, child := range typed {
			if phase := findPhaseValue(child); phase != "" {
				return phase
			}
		}
	}
	return ""
}

func (state *runState) startRegistry(ctx context.Context, dependencies Dependencies) error {
	volume := state.prepared.registryName + "-data"
	writer := state.prepared.registryName + "-writer"
	if _, err := dependencies.Commands.Run(ctx, Command{Name: "docker", Args: []string{
		"volume", "create", "--label", "ai.openbox.evaluation-id=" + state.prepared.evaluationID, volume,
	}}); err != nil {
		return fail("registry_start_failure", "project evaluate: create run-owned registry volume failed")
	}
	state.registryStarted = true
	args := []string{
		"run", "--detach", "--pull=never", "--name", writer,
		"--label", "ai.openbox.evaluation-id=" + state.prepared.evaluationID,
		"--network", "host",
		"--env", "REGISTRY_HTTP_ADDR=127.0.0.1:5000",
		"--volume", volume + ":/var/lib/registry",
		RegistryImage,
	}
	if _, err := dependencies.Commands.Run(ctx, Command{Name: "docker", Args: args}); err != nil {
		return fail("registry_start_failure", "project evaluate: start pinned registry writer failed")
	}
	deadline := dependencies.Clock.Now().Add(10 * time.Second)
	for {
		if _, err := dependencies.Commands.Run(ctx, Command{Name: "docker", Args: []string{
			"exec", writer, "wget", "-qO-", "http://127.0.0.1:5000/v2/",
		}}); err == nil {
			return nil
		}
		if dependencies.Clock.Now().After(deadline) {
			return fail("registry_start_failure", "project evaluate: registry writer did not become ready")
		}
		if err := dependencies.Clock.Sleep(ctx, 100*time.Millisecond); err != nil {
			return &classifiedError{class: contextClassification(err), err: err}
		}
	}
}

func (state *runState) publishImage(ctx context.Context, dependencies Dependencies) error {
	writer := state.prepared.registryName + "-writer"
	volume := state.prepared.registryName + "-data"
	state.registryTag = "127.0.0.1:5000/ai.openbox/evaluation:" + state.prepared.evaluationID
	if _, err := dependencies.Commands.Run(ctx, Command{Name: "docker", Args: []string{"tag", state.prepared.image.ID, state.registryTag}}); err != nil {
		return fail("image_publication_failure", "project evaluate: create run-owned image tag failed")
	}
	if _, err := dependencies.Commands.Run(ctx, Command{Name: "docker", Args: []string{"push", state.registryTag}}); err != nil {
		return fail("image_publication_failure", "project evaluate: push exact image to temporary registry failed")
	}
	if _, err := dependencies.Commands.Run(ctx, Command{Name: "docker", Args: []string{"container", "rm", "--force", writer}}); err != nil {
		return fail("registry_start_failure", "project evaluate: remove registry writer failed")
	}
	if _, err := dependencies.Commands.Run(ctx, Command{Name: "docker", Args: []string{
		"run", "--detach", "--pull=never", "--name", state.prepared.registryName,
		"--label", "ai.openbox.evaluation-id=" + state.prepared.evaluationID,
		"--publish", "127.0.0.1::5000",
		"--volume", volume + ":/var/lib/registry",
		RegistryImage,
	}}); err != nil {
		return fail("registry_start_failure", "project evaluate: start loopback registry reader failed")
	}
	portResult, err := dependencies.Commands.Run(ctx, Command{Name: "docker", Args: []string{"port", state.prepared.registryName, "5000/tcp"}})
	if err != nil {
		return fail("registry_start_failure", "project evaluate: resolve temporary registry port failed")
	}
	address := strings.TrimSpace(string(portResult.Stdout))
	if !strings.HasPrefix(address, "127.0.0.1:") {
		return fail("registry_start_failure", "project evaluate: temporary registry did not bind loopback")
	}
	if _, err := strconv.Atoi(strings.TrimPrefix(address, "127.0.0.1:")); err != nil {
		return fail("registry_start_failure", "project evaluate: temporary registry returned an invalid port")
	}
	state.registryAddress = address
	deadline := dependencies.Clock.Now().Add(10 * time.Second)
	for {
		response, requestErr := get(ctx, dependencies.HTTP, "http://"+address+"/v2/")
		if requestErr == nil && response != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		if dependencies.Clock.Now().After(deadline) {
			return fail("registry_start_failure", "project evaluate: temporary registry did not become ready")
		}
		if err := dependencies.Clock.Sleep(ctx, 100*time.Millisecond); err != nil {
			return &classifiedError{class: contextClassification(err), err: err}
		}
	}
	manifestURL := "http://" + state.registryAddress + "/v2/ai.openbox/evaluation/manifests/" + url.PathEscape(state.prepared.evaluationID)
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, nil)
	request.Header.Set("accept", strings.Join([]string{
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ", "))
	response, err := dependencies.HTTP.Do(request)
	if err != nil || response == nil {
		return fail("registry_refusal", "project evaluate: Registry v2 manifest request failed")
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	digest := response.Header.Get("Docker-Content-Digest")
	if readErr != nil || len(body) > 1<<20 || response.StatusCode != http.StatusOK || !manifestDigestPattern.MatchString(digest) {
		return fail("registry_refusal", "project evaluate: Registry v2 returned an invalid manifest response")
	}
	var manifest struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if json.Unmarshal(body, &manifest) != nil || manifest.Config.Digest != state.prepared.image.ID {
		return fail("image_identity_mismatch", "project evaluate: published manifest config digest does not match the local image ID")
	}
	state.manifestDigest = digest
	state.publishedReference = state.registryAddress + "/ai.openbox/evaluation@" + digest
	state.immutableReference = state.prepared.image.ID
	state.record.Image.ManifestDigest = digest
	state.record.Image.PublishedReference = state.publishedReference
	state.record.Image.ImmutableReference = state.immutableReference
	return nil
}

func (state *runState) cleanup(dependencies Dependencies) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var cleanupErrors []error
	if state.relay != nil {
		closeContext, closeCancel := context.WithTimeout(ctx, 5*time.Second)
		if err := state.relay.Close(closeContext); err != nil {
			cleanupErrors = append(cleanupErrors, errors.New("Core relay shutdown failed"))
		}
		closeCancel()
	}
	if state.effectRelay != nil {
		closeContext, closeCancel := context.WithTimeout(ctx, 5*time.Second)
		if err := state.effectRelay.Close(closeContext); err != nil {
			cleanupErrors = append(cleanupErrors, errors.New("safe effect receipt service shutdown failed"))
		}
		closeCancel()
	}
	if state.sandboxMayExist {
		state.record.Cleanup.SandboxDeleteAttempted = true
		_, deleteErr := dependencies.Commands.Run(ctx, Command{Name: "openshell", Args: []string{"sandbox", "delete", state.prepared.sandboxName}})
		absent := state.waitSandboxAbsent(ctx, dependencies)
		state.record.Cleanup.SandboxAbsent = absent
		if deleteErr != nil && !absent {
			cleanupErrors = append(cleanupErrors, errors.New("exact sandbox delete failed"))
		}
		if !absent {
			cleanupErrors = append(cleanupErrors, errors.New("exact sandbox remains after cleanup"))
		} else {
			state.phase(dependencies, "sandbox_deleted")
		}
	} else {
		state.record.Cleanup.SandboxAbsent = state.labelAbsent(ctx, dependencies)
	}
	if state.registryTag != "" {
		_, _ = dependencies.Commands.Run(ctx, Command{Name: "docker", Args: []string{"image", "rm", state.registryTag}})
		state.record.Cleanup.RegistryTagRemoved = dockerObjectAbsent(ctx, dependencies.Commands, []string{"image", "inspect", state.registryTag})
		if !state.record.Cleanup.RegistryTagRemoved {
			cleanupErrors = append(cleanupErrors, errors.New("run-owned registry tag remains"))
		}
	} else {
		state.record.Cleanup.RegistryTagRemoved = true
	}
	if state.registryStarted {
		writer := state.prepared.registryName + "-writer"
		_, _ = dependencies.Commands.Run(ctx, Command{Name: "docker", Args: []string{"container", "rm", "--force", state.prepared.registryName}})
		_, _ = dependencies.Commands.Run(ctx, Command{Name: "docker", Args: []string{"container", "rm", "--force", writer}})
		state.record.Cleanup.RegistryContainerAbsent = dockerObjectAbsent(ctx, dependencies.Commands, []string{"container", "inspect", state.prepared.registryName}) &&
			dockerObjectAbsent(ctx, dependencies.Commands, []string{"container", "inspect", writer})
		state.record.Cleanup.RegistryContainerRemoved = state.record.Cleanup.RegistryContainerAbsent
		if !state.record.Cleanup.RegistryContainerAbsent {
			cleanupErrors = append(cleanupErrors, errors.New("run-owned registry container remains"))
		}
		volume := state.prepared.registryName + "-data"
		_, _ = dependencies.Commands.Run(ctx, Command{Name: "docker", Args: []string{"volume", "rm", volume}})
		state.record.Cleanup.RegistryVolumeAbsent = dockerObjectAbsent(ctx, dependencies.Commands, []string{"volume", "inspect", volume})
		state.record.Cleanup.RegistryVolumeRemoved = state.record.Cleanup.RegistryVolumeAbsent
		if !state.record.Cleanup.RegistryVolumeAbsent {
			cleanupErrors = append(cleanupErrors, errors.New("run-owned registry volume remains"))
		}
	} else {
		state.record.Cleanup.RegistryContainerRemoved = true
		state.record.Cleanup.RegistryContainerAbsent = true
		state.record.Cleanup.RegistryVolumeRemoved = true
		state.record.Cleanup.RegistryVolumeAbsent = true
	}
	if (state.registryStarted || state.registryTag != "") && state.record.Cleanup.RegistryTagRemoved && state.record.Cleanup.RegistryContainerAbsent && state.record.Cleanup.RegistryVolumeAbsent {
		state.phase(dependencies, "registry_removed")
	}
	state.record.Cleanup.OllamaModelUnloaded = ensureModelUnloaded(ctx, dependencies.Commands)
	if !state.record.Cleanup.OllamaModelUnloaded {
		cleanupErrors = append(cleanupErrors, errors.New("Granite model remains loaded"))
	}
	if !state.labelAbsent(ctx, dependencies) {
		cleanupErrors = append(cleanupErrors, errors.New("matching evaluation sandbox remains"))
		state.record.Cleanup.SandboxAbsent = false
	}
	return errors.Join(cleanupErrors...)
}

func (state *runState) waitSandboxAbsent(ctx context.Context, dependencies Dependencies) bool {
	deadline := dependencies.Clock.Now().Add(50 * time.Second)
	for {
		_, exists, err := state.sandboxPhase(ctx, dependencies)
		if err != nil && isNotFound(err) {
			return true
		}
		if err == nil && !exists {
			return true
		}
		if dependencies.Clock.Now().After(deadline) {
			return false
		}
		if dependencies.Clock.Sleep(ctx, 250*time.Millisecond) != nil {
			return false
		}
	}
}

func (state *runState) labelAbsent(ctx context.Context, dependencies Dependencies) bool {
	result, err := dependencies.Commands.Run(ctx, Command{Name: "openshell", Args: []string{
		"sandbox", "list", "--selector", "ai.openbox.evaluation-id=" + state.prepared.evaluationID, "-o", "json",
	}})
	return err == nil && jsonArrayEmpty(result.Stdout)
}

func dockerObjectAbsent(ctx context.Context, runner CommandRunner, args []string) bool {
	result, err := runner.Run(ctx, Command{Name: "docker", Args: args})
	return err != nil && isNotFound(errors.New(string(append(result.Stdout, result.Stderr...))))
}

func ensureModelUnloaded(ctx context.Context, runner CommandRunner) bool {
	result, err := runner.Run(ctx, Command{Name: "ollama", Args: []string{"ps"}})
	if err != nil {
		return false
	}
	if strings.Contains(string(result.Stdout), InferenceModel) {
		if _, err := runner.Run(ctx, Command{Name: "ollama", Args: []string{"stop", InferenceModel}}); err != nil {
			return false
		}
		for attempt := 0; attempt < 20; attempt++ {
			result, err = runner.Run(ctx, Command{Name: "ollama", Args: []string{"ps"}})
			if err != nil || !strings.Contains(string(result.Stdout), InferenceModel) {
				break
			}
			select {
			case <-ctx.Done():
				return false
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	return err == nil && !strings.Contains(string(result.Stdout), InferenceModel)
}

func (state *runState) finishRecord() {
	if state.relay != nil {
		receipt := state.relay.Receipt()
		state.record.Core.ValidationAttempts = receipt.ValidationAttempts
		state.record.Core.ValidationSuccesses = receipt.ValidationSuccesses
		state.record.Core.MatchingValidations = receipt.MatchingValidations
		state.record.Core.GovernanceEvents = receipt.GovernanceEvents
		state.record.Core.LastValidationStatus = receipt.LastValidationStatus
		state.record.Core.AuthorizationClass = receipt.AuthorizationClass
	}
	if state.effectRelay != nil {
		receipt := state.effectRelay.Receipt()
		state.record.Effects.SafeSinkAttempts = receipt.Attempts
		state.record.Effects.SafeSinkMatching = receipt.MatchingReceipts
	}
	state.record.Logs.ProcessStdout = digestRecord(state.process.Stdout, state.process.StdoutTruncated)
	state.record.Logs.ProcessStderr = digestRecord(state.process.Stderr, state.process.StderrTruncated)
	openShellBytes := combinedLog(state.openShellLog)
	state.record.Logs.OpenShell = digestRecord(openShellBytes, state.openShellLog.StdoutTruncated || state.openShellLog.StderrTruncated || combinedLogSize(state.openShellLog) > maxCaptureBytes)
}

func (state *runState) writeOutput() error {
	if !state.policyWritten {
		if len(state.policy) == 0 {
			state.policy, _ = buildPolicy(state.prepared.applicationRoot, state.prepared.argv[0], 0)
		}
		if err := state.workspace.WritePrivateFile("policy.json", state.policy); err != nil {
			return err
		}
		state.policyWritten = true
	}
	if err := state.workspace.WritePrivateFile("process.stdout", state.process.Stdout); err != nil {
		return err
	}
	if err := state.workspace.WritePrivateFile("process.stderr", state.process.Stderr); err != nil {
		return err
	}
	if err := state.workspace.WritePrivateFile("openshell.log", combinedLog(state.openShellLog)); err != nil {
		return err
	}
	execution, err := json.Marshal(state.record)
	if err != nil {
		return err
	}
	return state.workspace.WritePrivateFile("execution.json", execution)
}

func combinedLog(result CommandResult) []byte {
	combined := append([]byte(nil), result.Stdout...)
	if len(result.Stdout) > 0 && len(result.Stderr) > 0 && result.Stdout[len(result.Stdout)-1] != '\n' {
		combined = append(combined, '\n')
	}
	combined = append(combined, result.Stderr...)
	if int64(len(combined)) > maxCaptureBytes {
		return combined[:maxCaptureBytes]
	}
	return combined
}

func combinedLogSize(result CommandResult) int64 {
	size := int64(len(result.Stdout) + len(result.Stderr))
	if len(result.Stdout) > 0 && len(result.Stderr) > 0 && result.Stdout[len(result.Stdout)-1] != '\n' {
		size++
	}
	return size
}

func sortedEnvironmentNames(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		if name != "OPENBOX_API_KEY" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func environmentInventory(values map[string]string) []string {
	names := sortedEnvironmentNames(values)
	names = append(names, "OPENBOX_API_KEY")
	sort.Strings(names)
	return names
}

func classify(err error) string {
	var classified *classifiedError
	if errors.As(err, &classified) {
		return classified.class
	}
	return "internal_error"
}

func contextClassification(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "interrupted"
}

func safeError(err error) string {
	message := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(message) > 512 {
		message = message[:512]
	}
	return message
}

func boundedDiagnostic(content []byte) string {
	text := strings.TrimSpace(string(content))
	if len(text) > 256 {
		text = text[:256]
	}
	return text
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "not found") || strings.Contains(text, "no such") || strings.Contains(text, "does not exist")
}

func intPointer(value int) *int { return &value }
