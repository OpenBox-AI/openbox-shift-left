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
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/sandboxclient"
)

var manifestDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type runState struct {
	prepared            *prepared
	workspace           *runfs.Workspace
	record              executionRecord
	policy              []byte
	policyWritten       bool
	process             CommandResult
	relay               *coreRelay
	effectRelay         *effectRelay
	registryStarted     bool
	registryAddress     string
	registryTag         string
	manifestDigest      string
	publishedReference  string
	immutableReference  string
	sandboxMayExist     bool
	sandboxRunID        string
	sandboxResult       *sandboxclient.ProjectRunCompleted
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
		BackendURL:      state.prepared.connector.backendURL,
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
		// No typed isolation evidence yet on this path. The egress decisions
		// and violation categories were attached to exec results, and the
		// workload is no longer an exec — it is the main process, which is the
		// only way it receives provider credentials. The pack records that as
		// `observed: false`, which means the provider recorded nothing, never
		// that nothing happened.
		SandboxEvidence: nil,
		Snapshot:        state.observationSnapshot,
		Backend:         state.observationResult,
		Window:          state.observationWindow,
		Effects: map[string]any{
			"safe_sink":        map[string]any{"status": statusForReceipt(effect.MatchingReceipts), "attempts": effect.Attempts, "matching_receipts": effect.MatchingReceipts, "evaluation_id": state.prepared.evaluationID, "matched_at": effect.MatchedAt.Format(time.RFC3339Nano)},
			"retrieval_poison": map[string]any{"status": "missing", "matching_receipts": 0},
			// Reported as declared, not as observed. Nothing on this path
			// receipts a model call: the retired CLI lane proved it by grepping
			// a gateway log line, and no typed receipt has replaced that. The
			// pack's own coverage forces this channel to `missing`, and these
			// fields say which route was configured, not that it was used.
			// `provider` carries the declared route. The pack forces `status` to
			// missing regardless: nothing on this path receipts a model call, so
			// these fields say what was CONFIGURED, not what was served.
			"model_route": modelRouteEffect(state.prepared),
			"core_relay":  map[string]any{"status": "observed", "matching_validations": receipt.MatchingValidations, "governance_events": receipt.GovernanceEvents},
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
	record.Sandbox.Service = state.prepared.sandboxService
	record.Sandbox.Capability = state.prepared.sandboxCapability
	record.Sandbox.Provider = state.prepared.connector.openBoxProvider
	record.Inference.Provider = state.prepared.declared.modelRoute
	record.Inference.Model = state.prepared.environment["OPENAI_MODEL"]
	record.Inference.ModelDigest = state.prepared.declared.modelDigest
	record.CoverageLimitations = []string{
		"development observation only; not a production confinement or enforcement qualification",
		"Core relay credential use is bound to the validated OCI entrypoint executable",
		"landlock best_effort may run without filesystem enforcement",
		"runtime cgroup pids.max availability is not guaranteed",
		"OpenBox evaluation agent uses provider-bound bearer authentication without SDK request signing",
		// Both of these are regressions against the retired CLI path, and both
		// are named here rather than left to show up as a bare "missing"
		// channel, because a reader cannot tell a channel that observed nothing
		// from a channel this path cannot observe at all.
		//
		// The workload runs as the sandbox's MAIN process, which is the only
		// process OpenShell gives the provider credentials to. The gRPC API has
		// no channel for a main process's stdout or stderr: GetSandboxLogs and
		// WatchSandbox both carry SandboxLogLine, which is supervisor events.
		// The old path did not read this from the API either — it came from CLI
		// attachment, which the supported path does not have.
		"workload stdout and stderr are not retained; the gateway API carries supervisor records only",
		// Typed egress decisions and violations arrive on an exec result. A main
		// process produces none, so sandbox_isolation reports zero records —
		// which is a statement about this channel, not about the workload.
		"per-process egress decisions and violations are not collected for a main-process workload",
		// The retired path proved the model route by substring-matching
		// API:INFERENCE in a gateway log line. That stream is gone and no typed
		// receipt has replaced it yet, so the route is unproven rather than
		// disproven.
		"model route is not independently receipted; the inference credential is resolved at the gateway proxy",
	}
	record.CoverageLimitations = append(record.CoverageLimitations,
		ungovernedCredentialLimitations(state.prepared.declared)...)
}

// modelRouteEffect describes the route this run was pointed at.
//
// model_digest is present only when the project declared one, and that is the
// whole point of it being optional. A local Ollama has a real content address
// for its weights and the preflight checks it. A hosted route — OpenAI,
// Anthropic, Gemini, OpenRouter — has none: the model is a service-side name
// whose weights can change behind it, so there is nothing to cite. The field
// used to be required, which left such a project no way to run except to invent
// a digest, and an invented content address in sealed evidence is worse than an
// absent one.
//
// Absent therefore means "this route publishes no digest", which is a fact
// about the route, and is why it is omitted rather than emitted empty.
func modelRouteEffect(prepared *prepared) map[string]any {
	effect := map[string]any{
		"status":   "missing",
		"provider": prepared.declared.modelRoute,
		"model":    prepared.environment["OPENAI_MODEL"],
	}
	if prepared.declared.modelDigest != "" {
		effect["model_digest"] = prepared.declared.modelDigest
	}
	return effect
}

// ungovernedCredentialLimitations discloses every credential-shaped variable
// the workload was handed in plaintext.
//
// Two ways a credential reaches the workload, and the pack must not blur them.
// A credential bound by the policy's credential_binding is held by the proxy:
// the workload gets the access and never the secret, and CANNOT use it
// off-policy. A credential in the environment is simply given to the workload,
// and the egress policy is the only thing between it and anywhere else.
//
// The second is supported on purpose — binding requires an endpoint, and many
// credentials have none this lane can name — but a pack that reported both the
// same way would claim a guarantee for half of them that only the first has.
// So each one is named here, once, by name only.
func ungovernedCredentialLimitations(declared *projectEnvironment) []string {
	names := declared.secretNames()
	limitations := make([]string, 0, len(names))
	for _, name := range names {
		// "credential-shaped", not "credential". The run classified this by
		// name, so it knows the variable was supplied in plaintext and unbound
		// — both observed — but not that the value is really a secret. Saying
		// more than that would be the pack guessing in its own evidence.
		limitations = append(limitations,
			"credential-shaped variable "+name+" was supplied to the workload in plaintext and was not bound to an endpoint")
	}
	return limitations
}

func (state *runState) phase(dependencies Dependencies, phase string) {
	state.phaseMu.Lock()
	defer state.phaseMu.Unlock()
	state.record.Phases = append(state.record.Phases, phaseEntry{Phase: phase, At: dependencies.Clock.Now()})
}

func (state *runState) execute(ctx context.Context, dependencies Dependencies) error {
	// Only a local-ollama route has anything here to load. A gateway-served
	// route is warmed, or not, by whoever runs it.
	if state.prepared.declared.modelRoute == ModelRouteLocalOllama {
		if err := loadInferenceModel(ctx, dependencies.inferenceHTTP(), state.prepared.environment["OPENAI_MODEL"]); err != nil {
			return &classifiedError{class: "model_load_failure", err: err}
		}
		state.phase(dependencies, "model_loaded")
	}

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

	relay, err := startCoreRelay(dependencies, state.prepared.connector.coreURL, state.prepared.input.OpenBoxAgent, state.prepared.evaluationID)
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
	}
	if commandErr := state.runThroughSandbox(ctx, dependencies); commandErr != nil {
		return commandErr
	}
	receipt := relay.Receipt()
	if receipt.MatchingValidations < 1 || receipt.GovernanceEvents < 1 {
		return fail("observation_failure", "project evaluate: required matching SDK validation and evaluation governance event were not observed")
	}
	return nil
}

func loadInferenceModel(ctx context.Context, client HTTPDoer, model string) error {
	body := strings.NewReader(`{"model":"` + model + `","keep_alive":"5m"}`)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, ollamaGenerateURL, body)
	if err != nil {
		return fmt.Errorf("project evaluate: construct Ollama model load request: %w", err)
	}
	request.Header.Set("content-type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("project evaluate: load local model %s", model)
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(content) > 64<<10 || response.StatusCode != http.StatusOK {
		return fmt.Errorf("project evaluate: load local model %s", model)
	}
	var result struct {
		Model      string `json:"model"`
		Done       bool   `json:"done"`
		DoneReason string `json:"done_reason"`
	}
	if json.Unmarshal(content, &result) != nil || result.Model != model || !result.Done || result.DoneReason != "load" {
		return errors.New("project evaluate: Ollama returned an invalid model load receipt")
	}
	return nil
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
	state.registryTag = pushRegistryHost + "/ai.openbox/evaluation:" + state.prepared.evaluationID
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
	// The reference handed to the sandbox is the PUSH name plus the digest, not
	// the reader's address and not the bare image ID.
	//
	// The bare ID is what the CLI path used, and the sandbox service refuses it
	// outright — a digest without a repository is not an immutable reference.
	// The reader's address does not work either: the gateway resolves an image
	// from the local container engine before falling back to a registry pull,
	// and Docker only records a repo digest under the name it was pushed to.
	// Handing it the push name means the engine resolves it locally and no
	// registry pull is attempted at all — which matters because that fallback
	// is HTTPS-only and this registry is plain HTTP on loopback.
	state.immutableReference = pushRegistryHost + "/ai.openbox/evaluation@" + digest
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
		if err := state.deleteSandbox(dependencies); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		} else {
			state.phase(dependencies, "sandbox_deleted")
		}
	} else {
		// Nothing was begun, so nothing is owed. Absence is true by
		// construction rather than by a probe that could lie.
		state.record.Cleanup.SandboxAbsent = true
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
	// Only a local-ollama route leaves a model loaded on this host to unload.
	// A gateway-served route reports the unload as done because there was
	// nothing here to do, not because a remote model was touched.
	localModel := ""
	if state.prepared.declared.modelRoute == ModelRouteLocalOllama {
		localModel = state.prepared.environment["OPENAI_MODEL"]
	}
	state.record.Cleanup.OllamaModelUnloaded = ensureModelUnloaded(ctx, dependencies.Commands, localModel)
	if !state.record.Cleanup.OllamaModelUnloaded {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("model %s remains loaded", localModel))
	}
	// The label sweep is gone with the CLI. The service owns the run's
	// identity and answers absence directly, so a second probe by selector
	// would only be this lane guessing at state it no longer holds.
	return errors.Join(cleanupErrors...)
}

func dockerObjectAbsent(ctx context.Context, runner CommandRunner, args []string) bool {
	result, err := runner.Run(ctx, Command{Name: "docker", Args: args})
	return err != nil && isNotFound(errors.New(string(append(result.Stdout, result.Stderr...))))
}

func ensureModelUnloaded(ctx context.Context, runner CommandRunner, model string) bool {
	if model == "" {
		return true
	}
	result, err := runner.Run(ctx, Command{Name: "ollama", Args: []string{"ps"}})
	if err != nil {
		return false
	}
	if strings.Contains(string(result.Stdout), model) {
		if _, err := runner.Run(ctx, Command{Name: "ollama", Args: []string{"stop", model}}); err != nil {
			return false
		}
		for attempt := 0; attempt < 20; attempt++ {
			result, err = runner.Run(ctx, Command{Name: "ollama", Args: []string{"ps"}})
			if err != nil || !strings.Contains(string(result.Stdout), model) {
				break
			}
			select {
			case <-ctx.Done():
				return false
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	return err == nil && !strings.Contains(string(result.Stdout), model)
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
	state.record.Logs.WorkloadRecords = digestRecord(state.sandboxLogs(), false)
}

func (state *runState) writeOutput() error {
	if !state.policyWritten {
		if len(state.policy) == 0 {
			state.policy = buildSandboxPolicy(state.prepared.argv[0], state.prepared.connector.openBoxProvider, 0)
		}
		if err := state.workspace.WritePrivateFile("policy.yaml", state.policy); err != nil {
			return err
		}
		state.policyWritten = true
	}
	// The retained bytes come from the same place as their digests in the
	// record. They used to come from the attached command's CommandResult,
	// which nothing populates now — so the record claimed a 473-byte stderr
	// while the file beside it was empty. A digest that does not describe the
	// artifact next to it is worse than no artifact.
	if err := state.workspace.WritePrivateFile("workload-records.json", state.sandboxLogs()); err != nil {
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

// environmentInventory lists every variable name the guest receives.
//
// Names, never values. OPENBOX_API_KEY is appended because it never appears in
// the map — the gateway injects it — but the guest does receive it, and a
// record that listed only what this process assembled would understate what
// the workload could reach.
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
