package evaluate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var manifestDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type runState struct {
	prepared           *prepared
	relay              *coreRelay
	registryStarted    bool
	registryAddress    string
	registryTag        string
	manifestDigest     string
	publishedReference string
	immutableReference string
	sandboxMayExist    bool
	sandboxRunID       string
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

// Run performs preflight without mutations, runs the image once through the
// sandbox, cleans up on every outcome, and only then asks the backend to
// evaluate the run. Nothing is written to disk: the evidence is in Core and the
// result lives at the backend.
func Run(ctx context.Context, input Input, dependencies Dependencies) (Result, error) {
	prepared, err := prepare(ctx, input, dependencies)
	if err != nil {
		return Result{}, err
	}
	stdout, stderr := writerOrDiscard(input.Stdout), writerOrDiscard(input.Stderr)
	for _, warning := range plaintextCredentialWarnings(prepared.declared) {
		fmt.Fprintf(stderr, "warning: %s\n", warning)
	}
	state := &runState{prepared: prepared}
	result := Result{EvaluationID: prepared.evaluationID}

	overall, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	runErr := state.execute(overall, dependencies)
	if cleanupErr := state.cleanup(dependencies); cleanupErr != nil {
		runErr = errors.Join(runErr, &classifiedError{class: "cleanup_failure", err: cleanupErr})
	}
	if runErr != nil {
		return result, runErr
	}
	return requestAndReport(ctx, input, dependencies, prepared, result, stdout)
}

func writerOrDiscard(writer io.Writer) io.Writer {
	if writer == nil {
		return io.Discard
	}
	return writer
}

// plaintextCredentialWarnings names every credential-shaped variable the
// workload was handed in plaintext.
//
// Two ways a credential reaches the workload, and they must not be blurred. A
// credential bound by the policy's credential_binding is held by the proxy: the
// workload gets the access and never the secret, and CANNOT use it off-policy.
// A credential in the environment is simply given to the workload, and the
// egress policy is the only thing between it and anywhere else.
//
// The second is supported on purpose, because binding requires an endpoint and
// many credentials have none this lane can name. Each one is named here, once,
// by name only, never by value.
func plaintextCredentialWarnings(declared *projectEnvironment) []string {
	names := declared.secretNames()
	warnings := make([]string, 0, len(names))
	for _, name := range names {
		// "credential-shaped", not "credential": the run classified this by name,
		// so it knows the variable was supplied in plaintext and unbound, but not
		// that the value is really a secret.
		warnings = append(warnings,
			"credential-shaped variable "+name+" was supplied to the workload in plaintext and was not bound to an endpoint")
	}
	return warnings
}

func (state *runState) execute(ctx context.Context, dependencies Dependencies) error {
	publication, cancel := context.WithTimeout(ctx, 120*time.Second)
	err := state.startRegistry(publication, dependencies)
	if err == nil {
		err = state.publishImage(publication, dependencies)
	}
	cancel()
	if err != nil {
		return err
	}

	relay, err := startCoreRelay(dependencies, state.prepared.connector.coreURL, state.prepared.input.OpenBoxAgent, state.prepared.evaluationID)
	if err != nil {
		return &classifiedError{class: "core_relay_failure", err: err}
	}
	state.relay = relay
	state.prepared.environment["OPENBOX_URL"] = fmt.Sprintf("http://host.openshell.internal:%d", relay.Port())
	if commandErr := state.runThroughSandbox(ctx, dependencies); commandErr != nil {
		return commandErr
	}
	receipt := relay.Receipt()
	if receipt.MatchingValidations < 1 || receipt.GovernanceEvents < 1 {
		return fail("observation_failure", "project evaluate: required matching SDK validation and evaluation governance event were not observed")
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
	if state.sandboxMayExist {
		if err := state.deleteSandbox(dependencies); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	if state.registryTag != "" {
		_, _ = dependencies.Commands.Run(ctx, Command{Name: "docker", Args: []string{"image", "rm", state.registryTag}})
		if !dockerObjectAbsent(ctx, dependencies.Commands, []string{"image", "inspect", state.registryTag}) {
			cleanupErrors = append(cleanupErrors, errors.New("run-owned registry tag remains"))
		}
	}
	if state.registryStarted {
		writer := state.prepared.registryName + "-writer"
		_, _ = dependencies.Commands.Run(ctx, Command{Name: "docker", Args: []string{"container", "rm", "--force", state.prepared.registryName}})
		_, _ = dependencies.Commands.Run(ctx, Command{Name: "docker", Args: []string{"container", "rm", "--force", writer}})
		if !dockerObjectAbsent(ctx, dependencies.Commands, []string{"container", "inspect", state.prepared.registryName}) ||
			!dockerObjectAbsent(ctx, dependencies.Commands, []string{"container", "inspect", writer}) {
			cleanupErrors = append(cleanupErrors, errors.New("run-owned registry container remains"))
		}
		volume := state.prepared.registryName + "-data"
		_, _ = dependencies.Commands.Run(ctx, Command{Name: "docker", Args: []string{"volume", "rm", volume}})
		if !dockerObjectAbsent(ctx, dependencies.Commands, []string{"volume", "inspect", volume}) {
			cleanupErrors = append(cleanupErrors, errors.New("run-owned registry volume remains"))
		}
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

// Describe returns the error class and a single-line, bounded message for a
// failed run, for the one place that reports it.
func Describe(err error) (class, message string) {
	return classify(err), safeError(err)
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

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "not found") || strings.Contains(text, "no such") || strings.Contains(text, "does not exist")
}
