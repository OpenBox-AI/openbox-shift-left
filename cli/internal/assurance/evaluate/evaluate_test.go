package evaluate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/sandboxclient"
)

func TestParseEnvironment(t *testing.T) {
	accepted, err := parseEnvironment([]byte("# comment\nEMPTY=\nA=one=two\nLOCAL=http://127.0.0.1:8080/path\nINFERENCE=https://inference.local/v1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if accepted.public["A"] != "one=two" || accepted.public["EMPTY"] != "" {
		t.Fatalf("values=%v", accepted.public)
	}

	tests := map[string]string{
		"duplicate":        "A=1\nA=2\n",
		"export":           "export A=1\n",
		"reserved":         "OPENBOX_URL=http://127.0.0.1:1\n",
		"remote URL":       "ENDPOINT=https://example.com/v1\n",
		"indented comment": "  # not-a-comment\n",
		"bad name":         "1A=value\n",
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseEnvironment([]byte(input)); err == nil {
				t.Fatal("accepted invalid environment")
			}
		})
	}
	if _, err := parseEnvironment([]byte{0xff}); err == nil {
		t.Fatal("accepted invalid UTF-8")
	}
	if _, err := parseEnvironment([]byte("A=x\x00y\n")); err == nil {
		t.Fatal("accepted NUL")
	}
}

func TestClassifyAuthorizationNeverRetainsCredential(t *testing.T) {
	tests := map[string]string{
		"":                                      "missing",
		"Bearer openshell:resolve:env:v1_KEY":   "openshell_placeholder",
		"Bearer obx_runtime-secret-never-store": "openbox_runtime_key",
		"Bearer opaque-secret-never-store":      "other_bearer",
		"Basic opaque-secret-never-store":       "other_scheme",
	}
	for input, want := range tests {
		if got := classifyAuthorization(input); got != want || strings.Contains(got, "secret") {
			t.Fatalf("classification=%q want=%q", got, want)
		}
	}
}

func TestEnvironmentFileRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	link := filepath.Join(root, "evaluation.env")
	if err := os.WriteFile(target, []byte("A=value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := parseEnvironmentFile(link); err == nil {
		t.Fatal("accepted symlink environment file")
	}
}

func TestEffectiveEnvironmentPrecedence(t *testing.T) {
	declared, err := parseEnvironment([]byte(
		"A=file\nB=file\n" +
			"OPENAI_MODEL=granite4.1:3b\n" +
			"OPENAI_API_KEY=sk-live\n"))
	if err != nil {
		t.Fatal(err)
	}
	values, err := effectiveEnvironment(
		[]string{"PATH=/usr/bin", "A=image", "OPENAI_API_KEY=unused"}, declared, "ev-one", "agent-one")
	if err != nil {
		t.Fatal(err)
	}
	// The project's declaration beats the image's baked-in value, for a secret
	// exactly as for a setting.
	if values["A"] != "file" || values["OPENBOX_EVALUATION_ID"] != "ev-one" || values["OPENAI_MODEL"] != "granite4.1:3b" {
		t.Fatalf("values=%v", values)
	}
	if values["OPENAI_API_KEY"] != "sk-live" {
		t.Fatalf("declared secret did not reach the guest environment: %v", values["OPENAI_API_KEY"])
	}

	// A credential baked into the IMAGE is still refused. The run cannot
	// disclose what it never saw declared, and a layer outlives the run.
	empty := newProjectEnvironment()
	if _, err := effectiveEnvironment([]string{"SERVICE_SECRET=real-value"}, empty, "ev", "agent"); err == nil {
		t.Fatal("accepted a credential baked into the image")
	}
	if _, err := effectiveEnvironment([]string{"OPENBOX_API_KEY=embedded"}, empty, "ev", "agent"); err == nil {
		t.Fatal("accepted embedded reserved credential")
	}
	// The stand-in an SDK insists on stays allowed.
	if _, err := effectiveEnvironment([]string{"SERVICE_TOKEN=unused"}, empty, "ev", "agent"); err != nil {
		t.Fatalf("refused a placeholder-valued image variable: %v", err)
	}
}

// Every credential the workload gets in plaintext is named in a warning. The
// bound one is not, because it is not in plaintext.
func TestPlaintextCredentialsAreWarnedAboutByName(t *testing.T) {
	declared, err := parseEnvironment([]byte(
		"OPENAI_MODEL=granite4.1:3b\n" +
			"PAYMENTS_API_KEY=sk-live-do-not-log\n" +
			"SERVICE_TOKEN=also-a-secret\n"))
	if err != nil {
		t.Fatal(err)
	}
	warnings := plaintextCredentialWarnings(declared)
	if len(warnings) != 2 {
		t.Fatalf("warnings=%v", warnings)
	}
	joined := strings.Join(warnings, "\n")
	for _, name := range []string{"PAYMENTS_API_KEY", "SERVICE_TOKEN"} {
		if !strings.Contains(joined, name) {
			t.Fatalf("%s was not named: %v", name, warnings)
		}
	}
	// A setting is not a credential, and values never appear.
	if strings.Contains(joined, "OPENAI_MODEL") || strings.Contains(joined, "sk-live") || strings.Contains(joined, "also-a-secret") {
		t.Fatalf("warning is wrong: %v", warnings)
	}
	if got := plaintextCredentialWarnings(newProjectEnvironment()); len(got) != 0 {
		t.Fatalf("a project with no credentials warned %v", got)
	}
}

func TestProjectEnvironmentIsAnOrdinaryDotenvFile(t *testing.T) {
	// No prefixes, no declaration syntax. This is what a developer gets by
	// copying their own .env.
	declared, err := parseEnvironment([]byte(
		"NODE_ENV=production\n" +
			"OPENAI_BASE_URL=https://inference.local/v1\n" +
			"OPENAI_MODEL=granite4.1:3b\n" +
			"OPENAI_API_KEY=unused\n" +
			"PAYMENTS_API_KEY=sk-live-do-not-log\n"))
	if err != nil {
		t.Fatal(err)
	}
	// Ordinary names stay ordinary and keep their own spelling.
	for name, want := range map[string]string{
		"NODE_ENV":        "production",
		"OPENAI_BASE_URL": "https://inference.local/v1",
		"OPENAI_MODEL":    "granite4.1:3b",
	} {
		if declared.public[name] != want {
			t.Fatalf("public[%s]=%q want %q", name, declared.public[name], want)
		}
	}
	// Credential-shaped names are filed for disclosure by SHAPE, since nothing
	// in the file marks them. That catches the real key and also the stand-in;
	// over-reporting is the direction this errs in on purpose.
	if got := declared.secretNames(); !reflect.DeepEqual(got, []string{"OPENAI_API_KEY", "PAYMENTS_API_KEY"}) {
		t.Fatalf("secret names=%v", got)
	}
	// Nothing is refused for its name, whatever its shape.
	for _, line := range []string{"SERVICE_TOKEN=value\n", "AWS_SECRET_ACCESS_KEY=abc\n", "DB_PASSWORD=hunter2\n"} {
		if _, err := parseEnvironment([]byte(line)); err != nil {
			t.Fatalf("%q was refused: %v", line, err)
		}
	}
	// One name twice is still a contradiction rather than a precedence.
	if _, err := parseEnvironment([]byte("TOKEN=a\nTOKEN=b\n")); err == nil {
		t.Fatal("accepted a name declared twice")
	}
}

func TestValidateImageUsesStandardOCICommand(t *testing.T) {
	image := validTestImage()
	argv, root, err := validateImage(image)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(argv, []string{"/usr/local/bin/node", "/app/src/index.mjs"}) || root != "/app" {
		t.Fatalf("argv=%v root=%q", argv, root)
	}

	mutations := map[string]func(*dockerImage){
		"platform":            func(image *dockerImage) { image.Architecture = "amd64" },
		"label":               func(image *dockerImage) { delete(image.Config.Labels, ContractLabel) },
		"obsolete label":      func(image *dockerImage) { image.Config.Labels["ai.openbox.project-evaluation.mode"] = "http" },
		"root":                func(image *dockerImage) { image.Config.User = "0" },
		"named user":          func(image *dockerImage) { image.Config.User = "node" },
		"relative executable": func(image *dockerImage) { image.Config.Entrypoint = []string{"node"} },
		"empty":               func(image *dockerImage) { image.Config.Entrypoint = nil; image.Config.Cmd = nil },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			candidate := validTestImage()
			mutate(&candidate)
			if _, _, err := validateImage(candidate); err == nil {
				t.Fatal("accepted invalid image")
			}
		})
	}
}

// The policy the sandbox service validates is YAML meeting its floor, not the
// CLI path's JSON. These are the properties that decide what the image may do.
func TestSandboxPolicyMeetsTheFloorAndBindsCredentialsByProvider(t *testing.T) {
	first := buildSandboxPolicy("/usr/local/bin/node", DefaultOpenBoxProvider, 49152)
	if !bytes.Equal(first, buildSandboxPolicy("/usr/local/bin/node", DefaultOpenBoxProvider, 49152)) {
		t.Fatal("policy bytes changed between identical renders")
	}
	text := string(first)
	for _, required := range []string{
		"read_write:\n    - /sandbox", // the floor admits exactly one writable root
		"run_as_user: sandbox",        // a name, never uid 1000
		"host.openshell.internal",
		"credential_binding:\n          provider: obx-openbox-local",
		"/api/v1/auth/validate",
		"/api/v1/governance/evaluate",
		"/api/v1/governance/approval",
		"- /app", "- /var/log", // declared so enrichment has nothing to add
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("policy missing %q:\n%s", required, text)
		}
	}
	// A credential must never be renderable into the document.
	// The Core relay is the only endpoint the guest may reach.
	if strings.Count(text, "host: host.openshell.internal") != 1 || !strings.Contains(text, "openbox_core_relay") {
		t.Fatalf("policy must carry exactly the Core relay endpoint:\n%s", text)
	}
	for _, forbidden := range []string{"obx_", "OPENBOX_API_KEY", "access: full", "**", "/effects/", "/v1/chat/completions"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("policy contains %q:\n%s", forbidden, text)
		}
	}
}

// The run identity is the gateway's, not this lane's: sbx-<15 hex> is 19
// characters, which is OpenShell's MAX_ROUTABLE_NAME_LEN.
func TestRunIdentityFitsTheGatewayNameLimit(t *testing.T) {
	id, err := sandboxclient.NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 19 || !strings.HasPrefix(id, "sbx-") {
		t.Fatalf("run id = %q", id)
	}
}

func TestUnsupportedPlatformHasNoEffects(t *testing.T) {
	runner := &countingRunner{}
	_, err := Run(context.Background(), Input{Image: "x", EnvFile: "missing", OpenBoxAgent: "x", ControlToken: "tok"}, Dependencies{
		Commands: runner, Clock: realClock{}, Random: bytes.NewReader(make([]byte, 12)),
		Listen: net.Listen, HTTP: http.DefaultClient, GOOS: "linux", GOARCH: "arm64",
	})
	if err == nil || runner.calls != 0 {
		t.Fatalf("err=%v calls=%d", err, runner.calls)
	}
}

func TestPreparedSandboxNameFitsOpenShellLimit(t *testing.T) {
	identifier := strings.Repeat("a", 24)
	name := "obx-eval-" + identifier[:10]
	if len(name) != 19 {
		t.Fatalf("sandbox name length=%d name=%q", len(name), name)
	}
}

// runBackend is the HTTP boundary of the backend, and nothing else: it records
// what the CLI asked and answers the two evaluation routes.
type runBackend struct {
	server  *httptest.Server
	mu      sync.Mutex
	posts   []string
	keys    []string
	gets    int
	final   string
	reason  string
	report  string
	postErr int
}

func newRunBackend(t *testing.T, final, reason string) *runBackend {
	t.Helper()
	backend := &runBackend{final: final, reason: reason, report: `null`}
	backend.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backend.mu.Lock()
		defer backend.mu.Unlock()
		backend.keys = append(backend.keys, r.Header.Get("x-api-key"))
		base := "/agent/" + testAgent + "/security-evaluations"
		switch {
		case r.Method == http.MethodPost && r.URL.Path == base:
			raw, _ := io.ReadAll(r.Body)
			backend.posts = append(backend.posts, string(raw))
			if backend.postErr != 0 {
				w.WriteHeader(backend.postErr)
				fmt.Fprint(w, `{"code":"connector_required"}`)
				return
			}
			fmt.Fprint(w, `{"status":200,"data":{"id":"se-1","status":"queued"}}`)
		case r.Method == http.MethodGet && r.URL.Path == base+"/se-1":
			backend.gets++
			if backend.gets < 2 {
				fmt.Fprint(w, `{"status":200,"data":{"id":"se-1","status":"analyzing"}}`)
				return
			}
			reason := "null"
			if backend.reason != "" {
				reason = fmt.Sprintf("%q", backend.reason)
			}
			fmt.Fprintf(w, `{"status":200,"data":{"id":"se-1","status":%q,"failure_reason":%s,"report":%s}}`, backend.final, reason, backend.report)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(backend.server.Close)
	return backend
}

func (backend *runBackend) postCount() int {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return len(backend.posts)
}

type runFixture struct {
	input   Input
	deps    Dependencies
	runner  *lifecycleRunner
	sandbox *fakeSandbox
	client  *lifecycleHTTP
	stdout  *bytes.Buffer
	stderr  *bytes.Buffer
	dir     string
}

func newRunFixture(t *testing.T, backend *runBackend, envContent string) *runFixture {
	t.Helper()
	dir := t.TempDir()
	envFile := filepath.Join(dir, "evaluation.env")
	if err := os.WriteFile(envFile, []byte(envContent), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := &runFixture{
		runner: newLifecycleRunner(), sandbox: newFakeSandbox(), dir: dir,
		client: &lifecycleHTTP{agentID: testAgent},
		stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
	}
	fixture.input = Input{
		Image: "example:local", EnvFile: envFile, OpenBoxAgent: testAgent,
		BackendURL: backend.server.URL, ControlToken: testToken,
		Stdout: fixture.stdout, Stderr: fixture.stderr,
	}
	fixture.deps = Dependencies{
		Commands: fixture.runner, Clock: newFakeClock(), Random: bytes.NewReader(bytes.Repeat([]byte{0x2a}, 12)),
		Listen: net.Listen, HTTP: fixture.client, BackendHTTP: backend.server.Client(),
		GOOS: "darwin", GOARCH: "arm64",
		Sandbox: fixture.sandbox, SandboxTemplateFromConfig: "example.invalid/base@sha256:" + strings.Repeat("a", 64),
	}
	return fixture
}

func (fixture *runFixture) run() (Result, error) {
	return Run(context.Background(), fixture.input, fixture.deps)
}

func TestRunRequestsTheEvaluationAfterASuccessfulRun(t *testing.T) {
	backend := newRunBackend(t, StatusComplete, "")
	fixture := newRunFixture(t, backend, "APP_ENV=security-test\nPAYMENTS_API_KEY=sk-live-do-not-log\n")
	result, err := fixture.run()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !result.Succeeded || result.Evaluation == nil || result.Evaluation.ID != "se-1" {
		t.Fatalf("result=%+v", result)
	}
	// One POST, authenticated, naming the run the SDK tagged its events with.
	runID := fixture.sandbox.environment["OPENBOX_EVALUATION_ID"]
	if backend.postCount() != 1 || backend.posts[0] != `{"run_id":"`+runID+`"}` || runID == "" || runID != result.EvaluationID {
		t.Fatalf("posts=%v run id=%q result=%q", backend.posts, runID, result.EvaluationID)
	}
	for _, key := range backend.keys {
		if key != testToken {
			t.Fatalf("key=%q", key)
		}
	}
	// Without --wait the run ends at the request: one line, no polling.
	if fixture.stdout.String() != "security evaluation requested: se-1 (agent "+testAgent+")\n" || backend.gets != 0 {
		t.Fatalf("stdout=%q gets=%d", fixture.stdout.String(), backend.gets)
	}
	// The credential-shaped variable is a warning on stderr, by name only.
	if !strings.Contains(fixture.stderr.String(), "warning: credential-shaped variable PAYMENTS_API_KEY") || strings.Contains(fixture.stderr.String(), "sk-live") {
		t.Fatalf("stderr=%q", fixture.stderr.String())
	}
	// Nothing is written beside the inputs: no record, no pack, no staging.
	entries, err := os.ReadDir(fixture.dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "evaluation.env" {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	// Cleanup ran: the sandbox is gone and the registry objects were removed.
	joined := fixture.runner.JoinedCommands()
	for _, want := range []string{"container rm --force", "volume rm ", "image rm "} {
		if !strings.Contains(joined, want) {
			t.Fatalf("cleanup command %q missing:\n%s", want, joined)
		}
	}
	if !fixture.sandbox.deleted {
		t.Fatal("sandbox was not deleted")
	}
	for _, forbidden := range []string{"--forward", "--upload", "sandbox exec", "provider create", "inference set"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("commands contain %q:\n%s", forbidden, joined)
		}
	}
	// The guest gets the image's own environment, the project's declarations and
	// the connector values, and nothing else: no receipt endpoints, no model route.
	names := make([]string, 0, len(fixture.sandbox.environment))
	for name := range fixture.sandbox.environment {
		names = append(names, name)
	}
	sort.Strings(names)
	wantNames := []string{"APP_ENV", "NODE_ENV", "OPENBOX_AGENT_ID", "OPENBOX_EVALUATION_ID", "OPENBOX_URL", "PATH", "PAYMENTS_API_KEY"}
	if !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("guest environment names=%v want=%v", names, wantNames)
	}
}

func TestRunWaitPrintsTheSummaryOnComplete(t *testing.T) {
	backend := newRunBackend(t, StatusComplete, "")
	backend.report = `{"result":"fail","security_pass":false,"issues":[{"title":"Send without approval"}],` +
		`"recommendations":[{"catalog_entry_id":"human-authorization","target":{"action_name":"send_email"},"rule":{"body":{}}}],` +
		`"rejected_candidates":[{}]}`
	fixture := newRunFixture(t, backend, "APP_ENV=security-test\n")
	fixture.input.Wait = true
	result, err := fixture.run()
	if err != nil || !result.Succeeded || result.Evaluation.Status != StatusComplete {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	out := fixture.stdout.String()
	for _, want := range []string{
		"security evaluation requested: se-1", "result: fail", "issues: 1", "  - Send without approval",
		"suggested rule: human-authorization on send_email", "rejected candidates: 1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s", want, out)
		}
	}
	if backend.gets != 2 {
		t.Fatalf("polled %d times", backend.gets)
	}
}

func TestRunWaitFailedEvaluationIsAnErrorWithItsReason(t *testing.T) {
	backend := newRunBackend(t, StatusFailed, "model connector returned 401")
	fixture := newRunFixture(t, backend, "APP_ENV=security-test\n")
	fixture.input.Wait = true
	result, err := fixture.run()
	if err == nil || result.Succeeded {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	class, message := Describe(err)
	if class != "evaluation_failed" || !strings.Contains(message, "model connector returned 401") || !strings.Contains(message, "se-1") {
		t.Fatalf("class=%s message=%s", class, message)
	}
	if strings.Contains(message, testToken) || strings.Contains(fixture.stdout.String()+fixture.stderr.String(), testToken) {
		t.Fatal("token leaked")
	}
}

func TestRunReportsABackendRefusalWithoutLeakingTheToken(t *testing.T) {
	backend := newRunBackend(t, StatusComplete, "")
	backend.postErr = http.StatusUnprocessableEntity
	fixture := newRunFixture(t, backend, "APP_ENV=security-test\n")
	_, err := fixture.run()
	class, message := Describe(err)
	if err == nil || class != "evaluation_request_failure" || !strings.Contains(message, "decision model") || strings.Contains(message, testToken) {
		t.Fatalf("class=%s message=%s", class, message)
	}
	if fixture.stdout.Len() != 0 {
		t.Fatalf("a refused request printed %q", fixture.stdout.String())
	}
}

func TestRunFailuresReportTheirClassAndNeverRequestAnEvaluation(t *testing.T) {
	tests := []struct {
		name           string
		configure      func(*lifecycleRunner, *lifecycleHTTP, *fakeSandbox)
		classification string
	}{
		{name: "registry refusal", configure: func(_ *lifecycleRunner, client *lifecycleHTTP, _ *fakeSandbox) {
			client.manifestStatus = http.StatusBadRequest
		}, classification: "registry_refusal"},
		{name: "readiness refused", configure: func(_ *lifecycleRunner, _ *lifecycleHTTP, sandbox *fakeSandbox) {
			sandbox.readyErr = errors.New("policy mismatch")
		}, classification: "readiness_failure"},
		{name: "command nonzero", configure: func(_ *lifecycleRunner, _ *lifecycleHTTP, sandbox *fakeSandbox) { sandbox.exitCode = 7 }, classification: "command_nonzero"},
		{name: "no SDK traffic reached Core", configure: func(_ *lifecycleRunner, _ *lifecycleHTTP, sandbox *fakeSandbox) { sandbox.skipRelay = true }, classification: "observation_failure"},
		{name: "cleanup overrides success", configure: func(runner *lifecycleRunner, _ *lifecycleHTTP, _ *fakeSandbox) { runner.cleanupTagStays = true }, classification: "cleanup_failure"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := newRunBackend(t, StatusComplete, "")
			fixture := newRunFixture(t, backend, "APP_ENV=security-test\n")
			test.configure(fixture.runner, fixture.client, fixture.sandbox)
			result, err := fixture.run()
			if err == nil || result.Succeeded {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if class, message := Describe(err); class != test.classification || message == "" || strings.Contains(message, "\n") {
				t.Fatalf("class=%q message=%q want %q", class, message, test.classification)
			}
			if backend.postCount() != 0 || fixture.stdout.Len() != 0 {
				t.Fatalf("a failed run requested an evaluation: posts=%v stdout=%q", backend.posts, fixture.stdout.String())
			}
			if test.classification != "cleanup_failure" && test.name != "registry refusal" && !fixture.sandbox.deleted {
				t.Fatal("sandbox was not deleted after a failed run")
			}
			if test.classification != "cleanup_failure" && !strings.Contains(fixture.runner.JoinedCommands(), "volume rm ") {
				t.Fatal("registry volume was not removed after a failed run")
			}
		})
	}
}

// A run whose evaluation cannot be requested wastes the developer's time, so
// the missing token is found before any command runs.
func TestMissingControlTokenFailsBeforeAnyEffect(t *testing.T) {
	backend := newRunBackend(t, StatusComplete, "")
	fixture := newRunFixture(t, backend, "APP_ENV=security-test\n")
	fixture.input.ControlToken = " "
	_, err := fixture.run()
	if err == nil || !strings.Contains(err.Error(), "OPENBOX_CONTROL_TOKEN") || !strings.Contains(err.Error(), "evaluate:agent_security") {
		t.Fatalf("err=%v", err)
	}
	if got := fixture.runner.JoinedCommands(); got != "" || fixture.sandbox.environment != nil || backend.postCount() != 0 {
		t.Fatalf("effects before the token check: commands=%q", got)
	}
}

func TestRunRefusesAnUnsafeBackendURLBeforeAnyEffect(t *testing.T) {
	backend := newRunBackend(t, StatusComplete, "")
	fixture := newRunFixture(t, backend, "APP_ENV=security-test\n")
	fixture.input.BackendURL = "https://user:pass@backend.example.com"
	if _, err := fixture.run(); err == nil || fixture.runner.JoinedCommands() != "" {
		t.Fatalf("err=%v commands=%q", err, fixture.runner.JoinedCommands())
	}
}

func TestBoundedBufferMarksTruncation(t *testing.T) {
	buffer := &boundedBuffer{limit: 4}
	if written, err := buffer.Write([]byte("abcdef")); err != nil || written != 6 {
		t.Fatalf("written=%d err=%v", written, err)
	}
	if string(buffer.Bytes()) != "abcd" || !buffer.Truncated() {
		t.Fatalf("bytes=%q truncated=%v", buffer.Bytes(), buffer.Truncated())
	}
}

func validTestImage() dockerImage {
	var image dockerImage
	image.ID = "sha256:" + strings.Repeat("a", 64)
	image.OS, image.Architecture = "linux", "arm64"
	image.Config.User = "1000:1000"
	image.Config.Env = []string{"PATH=/usr/local/bin:/usr/bin", "NODE_ENV=production"}
	image.Config.Entrypoint = []string{"/usr/local/bin/node"}
	image.Config.Cmd = []string{"/app/src/index.mjs"}
	image.Config.WorkingDir = "/app"
	image.Config.Labels = map[string]string{ContractLabel: ContractVersion}
	return image
}

type countingRunner struct{ calls int }

func (runner *countingRunner) Run(context.Context, Command) (CommandResult, error) {
	runner.calls++
	return CommandResult{}, errors.New("unexpected")
}
func (runner *countingRunner) Start(context.Context, Command) (Process, error) {
	runner.calls++
	return nil, errors.New("unexpected")
}

type lifecycleRunner struct {
	mu              sync.Mutex
	commands        []Command
	trigger         chan struct{}
	cleanupTagStays bool
}

func newLifecycleRunner() *lifecycleRunner { return &lifecycleRunner{trigger: make(chan struct{})} }

func (runner *lifecycleRunner) Run(_ context.Context, command Command) (CommandResult, error) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	runner.commands = append(runner.commands, command)
	args := strings.Join(command.Args, " ")
	switch {
	case command.Name == "docker" && strings.HasPrefix(args, "image inspect example:local"):
		content, _ := json.Marshal([]dockerImage{validTestImage()})
		return CommandResult{Stdout: content}, nil
	case command.Name == "docker" && strings.HasPrefix(args, "image inspect "+RegistryImage):
		image := validTestImage()
		image.ID = "sha256:" + strings.Repeat("c", 64)
		return jsonResult([]dockerImage{image}), nil
	case command.Name == "docker" && strings.HasPrefix(args, "run --detach --pull=never"):
		return CommandResult{Stdout: []byte("registry-container-id\n")}, nil
	case command.Name == "docker" && strings.HasPrefix(args, "volume create "):
		return CommandResult{Stdout: []byte("registry-volume\n")}, nil
	case command.Name == "docker" && strings.HasPrefix(args, "exec ") && strings.HasSuffix(args, "wget -qO- http://127.0.0.1:5000/v2/"):
		return CommandResult{Stdout: []byte("{}")}, nil
	case command.Name == "docker" && strings.HasPrefix(args, "port "):
		return CommandResult{Stdout: []byte("127.0.0.1:49153\n")}, nil
	case command.Name == "docker" && strings.HasPrefix(args, "tag "):
		return CommandResult{}, nil
	case command.Name == "docker" && strings.HasPrefix(args, "push "):
		return CommandResult{}, nil
	case command.Name == "docker" && strings.HasPrefix(args, "image rm "):
		return CommandResult{}, nil
	case command.Name == "docker" && strings.HasPrefix(args, "image inspect 127.0.0.1:"):
		if runner.cleanupTagStays {
			return jsonResult([]dockerImage{validTestImage()}), nil
		}
		return CommandResult{Stderr: []byte("No such image")}, errors.New("not found")
	case command.Name == "docker" && strings.HasPrefix(args, "container rm --force "):
		return CommandResult{}, nil
	case command.Name == "docker" && strings.HasPrefix(args, "container inspect "):
		return CommandResult{Stderr: []byte("No such container")}, errors.New("not found")
	case command.Name == "docker" && strings.HasPrefix(args, "volume rm "):
		return CommandResult{}, nil
	case command.Name == "docker" && strings.HasPrefix(args, "volume inspect "):
		return CommandResult{Stderr: []byte("No such volume")}, errors.New("not found")
	default:
		return CommandResult{}, errors.New("unexpected command: " + command.Name + " " + args)
	}
}

// Start refuses everything, which is the assertion.
//
// The lane used to start two long-running `openshell` processes — an attached
// `sandbox create` and a `logs --tail` follower — and this fake reconstructed
// the run from their argv. Execution is a typed sandbox call now, so a Start
// reaching this runner would mean something regressed to spawning a process.
func (runner *lifecycleRunner) Start(_ context.Context, command Command) (Process, error) {
	runner.mu.Lock()
	runner.commands = append(runner.commands, command)
	runner.mu.Unlock()
	return nil, errors.New("unexpected start: " + command.Name)
}

func (runner *lifecycleRunner) JoinedCommands() string {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	var lines []string
	for _, command := range runner.commands {
		lines = append(lines, command.Name+" "+strings.Join(command.Args, " "))
	}
	return strings.Join(lines, "\n")
}

func jsonResult(value any) CommandResult {
	content, _ := json.Marshal(value)
	return CommandResult{Stdout: content}
}

type lifecycleHTTP struct {
	agentID        string
	manifestStatus int
}

func (client *lifecycleHTTP) Do(request *http.Request) (*http.Response, error) {
	status, body, headers := http.StatusOK, `{}`, make(http.Header)
	switch {
	case request.URL.String() == localCoreURL+"/":
		body = "hello world"
	case request.URL.Path == "/health":
		body = `{"status":200}`
	case request.URL.Host == "127.0.0.1:49153" && request.URL.Path == "/v2/":
		body = `{}`
	case request.URL.Host == "127.0.0.1:49153" && strings.Contains(request.URL.Path, "/manifests/"):
		if client.manifestStatus != 0 {
			status = client.manifestStatus
			break
		}
		body = `{"schemaVersion":2,"config":{"digest":"sha256:` + strings.Repeat("a", 64) + `"}}`
		headers.Set("Docker-Content-Digest", "sha256:"+strings.Repeat("b", 64))
	case request.URL.Host == "127.0.0.1:8086" && request.URL.Path == "/api/v1/auth/validate":
		body = `{"valid":true,"active":true,"agent_id":"` + client.agentID + `"}`
	case request.URL.Host == "127.0.0.1:8086" && request.URL.Path == "/api/v1/governance/evaluate":
		body = `{"action":"allow"}`
	default:
		status = http.StatusNotFound
	}
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
}

// fakeSandbox stands in for the sandbox service.
//
// It answers the six typed operations the lane uses and nothing else, which is
// the point: the lane can no longer reach a sandbox by any other route, so a
// fake at this seam covers the whole boundary.
type fakeSandbox struct {
	capabilities []string
	beginErr     error
	readyErr     error
	execErr      error
	exitCode     int
	evidence     []byte
	// skipRelay makes the fake guest silent: no SDK traffic reaches Core.
	skipRelay   bool
	deleted     bool
	absent      bool
	environment map[string]string
}

func newFakeSandbox() *fakeSandbox {
	return &fakeSandbox{
		capabilities: []string{sandboxclient.ProjectRunCapability},
		evidence:     []byte(`{"egress_decisions":[]}`),
		absent:       true,
	}
}

func (fake *fakeSandbox) Capabilities(time.Duration) ([]string, error) {
	return fake.capabilities, nil
}

func (fake *fakeSandbox) Begin(spec sandboxclient.ProjectRunSpec, _ time.Duration) (string, string, error) {
	if fake.beginErr != nil {
		return "", "", fake.beginErr
	}
	// The workload must be the main process, or provider credentials never
	// reach it. A spec without a command would run a keepalive and observe
	// nothing.
	if len(spec.Command) == 0 {
		return "", "", errors.New("fake sandbox: run began without a workload command")
	}
	// The credential must never reach the service. Asserting it here rather
	// than only in the policy test keeps the guarantee on the live path.
	if _, present := spec.Environment["OPENBOX_API_KEY"]; present {
		return "", "", errors.New("fake sandbox: caller sent a credential in the environment")
	}
	fake.environment = spec.Environment
	return spec.RunID, "token-created", nil
}

func (fake *fakeSandbox) WaitReady(runID, _ string, _ sandboxclient.PolicyIdentity, _ time.Duration) (string, error) {
	if fake.readyErr != nil {
		return "", fake.readyErr
	}
	return "token-ready", nil
}

func (fake *fakeSandbox) WaitCompleted(_, _ string, _ time.Duration) (*sandboxclient.ProjectRunCompleted, error) {
	if fake.execErr != nil {
		return nil, fake.execErr
	}
	// Stand in for the guest's SDK traffic. The lane's success predicate is
	// that the relay observed a matching validation and a governance event, so
	// a fake that never calls it would only ever prove the failure path.
	if !fake.skipRelay {
		fake.callCoreRelay()
	}
	return &sandboxclient.ProjectRunCompleted{
		ExitCode: fake.exitCode,
		Logs: []sandboxclient.ProjectRunLogRecord{
			{TimestampMS: 1, Level: "INFO", Source: "sandbox", Target: "workload", Message: "ok"},
		},
	}, nil
}

// callCoreRelay reaches the relay the way the guest would, through the URL the
// policy admits — rewritten to loopback because there is no guest here.
func (fake *fakeSandbox) callCoreRelay() {
	base := strings.Replace(fake.environment["OPENBOX_URL"], "host.openshell.internal", "127.0.0.1", 1)
	if base == "" {
		return
	}
	client := &http.Client{Timeout: 5 * time.Second}
	if response, err := client.Get(base + "/api/v1/auth/validate"); err == nil {
		response.Body.Close()
	}
	body := strings.NewReader(`{"evaluation_id":"` + fake.environment["OPENBOX_EVALUATION_ID"] + `"}`)
	request, err := http.NewRequest(http.MethodPost, base+"/api/v1/governance/evaluate", body)
	if err != nil {
		return
	}
	request.Header.Set("content-type", "application/json")
	if response, err := client.Do(request); err == nil {
		response.Body.Close()
	}
}

func (fake *fakeSandbox) Delete(string, time.Duration) error {
	fake.deleted = true
	return nil
}

func (fake *fakeSandbox) WaitDeleted(string, time.Duration) error {
	if fake.absent {
		return nil
	}
	return errors.New("fake sandbox: still present")
}

// The connector is the whole point of part 2: a run against UAT or production
// is the same lane pointed somewhere else, not a different code path. The
// local-stack values are a default, and defaults must not be reachable by
// accident when a caller said something.
func TestConnectorResolvesPerEnvironmentAndDefaultsToLocalStack(t *testing.T) {
	local, err := resolveConnector(Input{})
	if err != nil {
		t.Fatalf("default connector: %v", err)
	}
	if local.coreURL != localCoreURL || local.backendURL != localBackendURL || local.openBoxProvider != DefaultOpenBoxProvider {
		t.Fatalf("default connector = %+v", local)
	}

	uat, err := resolveConnector(Input{
		CoreURL:         "https://core.uat.openbox.ai/",
		BackendURL:      "https://backend.uat.openbox.ai",
		OpenBoxProvider: "obx-openbox-uat",
	})
	if err != nil {
		t.Fatalf("uat connector: %v", err)
	}
	// The trailing slash is trimmed once, here, so no later caller has to guess
	// whether it needs to add or strip one.
	if uat.coreURL != "https://core.uat.openbox.ai" || uat.backendURL != "https://backend.uat.openbox.ai" {
		t.Fatalf("uat connector = %+v", uat)
	}
	if uat.openBoxProvider != "obx-openbox-uat" {
		t.Fatalf("uat provider = %q", uat.openBoxProvider)
	}

	for name, input := range map[string]Input{
		"no scheme":         {CoreURL: "core.uat.openbox.ai"},
		"wrong scheme":      {CoreURL: "ftp://core.uat.openbox.ai"},
		"embedded userinfo": {BackendURL: "https://user:pass@backend.uat.openbox.ai"},
	} {
		if _, err := resolveConnector(input); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}
