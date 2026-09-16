package evaluate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/runfs"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/sandboxclient"
)

// The demo project's model, as a TEST fixture. It is not a lane constant any
// more: which model serves the gateway's inference.local route is declared per
// project in `.env.sandbox`, so pinning one here would re-create the coupling
// that made every evaluable project the same project.
const (
	testModel       = "granite4.1:3b"
	testModelDigest = "sha256:6fd349357287c7ffc9e38189a93b48ea175d24fc566b38f09cfc564fb7f303eb"
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

func TestEffectiveEnvironmentPrecedenceAndInventory(t *testing.T) {
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
	wantNames := []string{"A", "B", "OPENAI_API_KEY", "OPENAI_MODEL", "OPENBOX_AGENT_ID", "OPENBOX_API_KEY", "OPENBOX_EVALUATION_ID", "PATH"}
	if got := environmentInventory(values); !reflect.DeepEqual(got, wantNames) {
		t.Fatalf("names=%v want=%v", got, wantNames)
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

// Every credential the workload gets in plaintext is named in the pack. The
// bound one is not, because it is not in plaintext.
func TestUngovernedCredentialsAreDisclosedByName(t *testing.T) {
	declared, err := parseEnvironment([]byte(
		"OPENAI_MODEL=granite4.1:3b\n" +
			"PAYMENTS_API_KEY=sk-live-do-not-log\n" +
			"SERVICE_TOKEN=also-a-secret\n"))
	if err != nil {
		t.Fatal(err)
	}
	limitations := ungovernedCredentialLimitations(declared)
	if len(limitations) != 2 {
		t.Fatalf("limitations=%v", limitations)
	}
	joined := strings.Join(limitations, "\n")
	// Declared, and undeclared-but-credential-shaped, are both disclosed.
	for _, name := range []string{"PAYMENTS_API_KEY", "SERVICE_TOKEN"} {
		if !strings.Contains(joined, name) {
			t.Fatalf("%s was not disclosed: %v", name, limitations)
		}
	}
	// A setting is not a credential, and values never appear.
	if strings.Contains(joined, "OPENAI_MODEL") || strings.Contains(joined, "sk-live") || strings.Contains(joined, "also-a-secret") {
		t.Fatalf("disclosure is wrong: %v", limitations)
	}
	if got := ungovernedCredentialLimitations(newProjectEnvironment()); len(got) != 0 {
		t.Fatalf("a project with no credentials disclosed %v", got)
	}
}

func TestProjectEnvironmentIsAnOrdinaryDotenvFile(t *testing.T) {
	// No prefixes, no declaration syntax. This is what a developer gets by
	// copying their own .env and adding the two runner directives.
	declared, err := parseEnvironment([]byte(
		"NODE_ENV=production\n" +
			"OPENAI_BASE_URL=https://inference.local/v1\n" +
			"OPENAI_MODEL=granite4.1:3b\n" +
			"OPENAI_API_KEY=unused\n" +
			"PAYMENTS_API_KEY=sk-live-do-not-log\n" +
			modelRouteSetting + "=" + ModelRouteGateway + "\n"))
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
	// A runner directive is consumed, not passed to the guest.
	if declared.modelRoute != ModelRouteGateway {
		t.Fatalf("model route=%q", declared.modelRoute)
	}
	if declared.public[modelRouteSetting] != "" || declared.secret[modelRouteSetting] != "" {
		t.Fatal("a runner directive leaked into the guest environment")
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
	for name, input := range map[string]string{
		"unknown route": modelRouteSetting + "=somewhere\n",
		"bad digest":    modelDigestSetting + "=not-a-digest\n",
	} {
		if _, err := parseEnvironment([]byte(input)); err == nil {
			t.Fatalf("%s was accepted", name)
		}
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
	first := buildSandboxPolicy("/usr/local/bin/node", DefaultOpenBoxProvider, 49152, 49153)
	if !bytes.Equal(first, buildSandboxPolicy("/usr/local/bin/node", DefaultOpenBoxProvider, 49152, 49153)) {
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
	for _, forbidden := range []string{"obx_", "OPENBOX_API_KEY", "access: full", "**"} {
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
	root := filepath.Join(t.TempDir(), "must-not-exist")
	runner := &countingRunner{}
	_, err := Run(context.Background(), Input{Image: "x", EnvFile: "missing", OpenBoxAgent: "x", Output: root}, Dependencies{
		Commands: runner, Clock: realClock{}, Random: bytes.NewReader(make([]byte, 12)),
		Listen: net.Listen, HTTP: http.DefaultClient, GOOS: "linux", GOARCH: "arm64",
	})
	if err == nil || runner.calls != 0 {
		t.Fatalf("err=%v calls=%d", err, runner.calls)
	}
	if _, statErr := os.Lstat(root); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("output exists: %v", statErr)
	}
}

func TestPreparedSandboxNameFitsOpenShellLimit(t *testing.T) {
	identifier := strings.Repeat("a", 24)
	name := "obx-eval-" + identifier[:10]
	if len(name) != 19 {
		t.Fatalf("sandbox name length=%d name=%q", len(name), name)
	}
}

func TestExistingOutputFailsBeforeReadsOrCommands(t *testing.T) {
	root := filepath.Join(t.TempDir(), "existing")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &countingRunner{}
	_, err := Run(context.Background(), Input{Image: "example:local", EnvFile: "missing", OpenBoxAgent: "c59e95b6-2a4e-44a7-8c43-b69bfa77667e", Output: root}, Dependencies{
		Commands: runner, Clock: realClock{}, Random: bytes.NewReader(make([]byte, 12)),
		Listen: net.Listen, HTTP: http.DefaultClient, GOOS: "darwin", GOARCH: "arm64",
	})
	if err == nil || !strings.Contains(err.Error(), "already exists") || runner.calls != 0 {
		t.Fatalf("err=%v calls=%d", err, runner.calls)
	}
}

func TestRunSuccessRetainsIncompleteExecutionRecord(t *testing.T) {
	parent := t.TempDir()
	envFile := filepath.Join(parent, "evaluation.env")
	if err := os.WriteFile(envFile, []byte("APP_ENV=security-test\n"+"OPENAI_MODEL=granite4.1:3b\n"+modelDigestSetting+"="+testModelDigest+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(parent, "record")
	runner := newLifecycleRunner()
	client := &lifecycleHTTP{agentID: "c59e95b6-2a4e-44a7-8c43-b69bfa77667e"}
	result, err := Run(context.Background(), Input{
		Image: "example:local", EnvFile: envFile,
		OpenBoxAgent: client.agentID, Output: output,
	}, Dependencies{
		Commands: runner, Clock: realClock{}, Random: bytes.NewReader(bytes.Repeat([]byte{0x2a}, 12)),
		Listen: net.Listen, HTTP: client, GOOS: "darwin", GOARCH: "arm64",
		Sandbox: newFakeSandbox(), SandboxTemplateFromConfig: "example.invalid/base@sha256:" + strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	resolvedParent, _ := filepath.EvalSymlinks(filepath.Dir(output))
	wantOutput := filepath.Join(resolvedParent, filepath.Base(output))
	if !result.Succeeded || result.Output != wantOutput {
		t.Fatalf("result=%+v", result)
	}
	entries, err := os.ReadDir(output)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
		info, _ := entry.Info()
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode=%o", entry.Name(), info.Mode().Perm())
		}
	}
	// No process.stdout/stderr: the workload is the sandbox's main process, and
	// a main process has no exec stream. Its output is the supervisor's records.
	want := []string{".incomplete", "execution.json", "policy.yaml", "workload-records.json"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("entries=%v want=%v", names, want)
	}
	content, err := os.ReadFile(filepath.Join(output, "execution.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(content, []byte("provider-real-secret")) {
		t.Fatal("record leaked secret")
	}
	var record executionRecord
	if err := json.Unmarshal(content, &record); err != nil {
		t.Fatal(err)
	}
	if record.ExitClassification != "success" || record.Core.MatchingValidations != 1 || record.Core.GovernanceEvents != 1 || !record.Cleanup.SandboxAbsent {
		t.Fatalf("record=%+v", record)
	}
	// Both references carry the same digest, and neither is the bare image ID
	// the CLI path ran. They differ only in host: the reference handed to the
	// sandbox uses the push address, because that is the name the local
	// container engine can resolve, while the published one names the reader.
	digest := record.Image.ManifestDigest
	if record.Image.ImmutableReference != pushRegistryHost+"/ai.openbox/evaluation@"+digest ||
		!strings.HasPrefix(record.Image.PublishedReference, "127.0.0.1:") ||
		!strings.HasSuffix(record.Image.PublishedReference, "@"+digest) ||
		record.Image.ImmutableReference == record.Image.LocalID {
		t.Fatalf("image identity=%+v", record.Image)
	}
	if state, err := runfs.Inspect(output); err != nil || state != runfs.StateIncomplete {
		t.Fatalf("state=%s err=%v", state, err)
	}
	if _, err := runfs.VerifyPack(output); err == nil {
		t.Fatal("execution staging directory verified as an audit pack")
	}
	joined := runner.JoinedCommands()
	for _, forbidden := range []string{"--forward", "--upload", "sandbox exec", "provider create", "inference set"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("commands contain %q:\n%s", forbidden, joined)
		}
	}
	for _, line := range strings.Split(joined, "\n") {
		if strings.HasPrefix(line, "openshell sandbox create ") && strings.Contains(line, "--detach") {
			t.Fatalf("sandbox create detached:\n%s", line)
		}
	}
}

func TestLifecycleFailuresRetainTruthfulRecordAndCleanup(t *testing.T) {
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
		{name: "cleanup overrides success", configure: func(runner *lifecycleRunner, _ *lifecycleHTTP, _ *fakeSandbox) { runner.cleanupTagStays = true }, classification: "cleanup_failure"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parent := t.TempDir()
			envFile := filepath.Join(parent, "evaluation.env")
			if err := os.WriteFile(envFile, []byte("OPENAI_MODEL=granite4.1:3b\n"+modelDigestSetting+"="+testModelDigest+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(parent, "record")
			runner := newLifecycleRunner()
			client := &lifecycleHTTP{agentID: "c59e95b6-2a4e-44a7-8c43-b69bfa77667e"}
			sandbox := newFakeSandbox()
			test.configure(runner, client, sandbox)
			result, err := Run(context.Background(), Input{Image: "example:local", EnvFile: envFile, OpenBoxAgent: client.agentID, Output: output}, Dependencies{
				Commands: runner, Clock: realClock{}, Random: bytes.NewReader(bytes.Repeat([]byte{0x31}, 12)),
				Listen: net.Listen, HTTP: client, GOOS: "darwin", GOARCH: "arm64",
				Sandbox: sandbox, SandboxTemplateFromConfig: "example.invalid/base@sha256:" + strings.Repeat("a", 64),
			})
			if err == nil || result.Succeeded {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			content, readErr := os.ReadFile(filepath.Join(output, "execution.json"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			var record executionRecord
			if json.Unmarshal(content, &record) != nil || record.ExitClassification != test.classification {
				t.Fatalf("classification=%q record=%s", record.ExitClassification, content)
			}
			if test.classification != "cleanup_failure" && (!record.Cleanup.RegistryContainerAbsent || !record.Cleanup.RegistryVolumeAbsent || !record.Cleanup.SandboxAbsent) {
				t.Fatalf("cleanup=%+v", record.Cleanup)
			}
		})
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
	getCount        int
	deleted         bool
	trigger         chan struct{}
	triggerOnce     sync.Once
	phaseError      bool
	commandExit     int
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
	case command.Name == "ollama" && args == "ps":
		return CommandResult{Stdout: []byte("NAME ID SIZE PROCESSOR UNTIL\n")}, nil
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
	case request.URL.String() == localBackendURL+"/health":
		body = `{"status":200}`
	case request.URL.String() == ollamaTagsURL:
		body = `{"models":[{"name":"granite4.1:3b","digest":"` + strings.TrimPrefix(testModelDigest, "sha256:") + `"}]}`
	case request.URL.String() == ollamaGenerateURL && request.Method == http.MethodPost:
		body = `{"model":"granite4.1:3b","done":true,"done_reason":"load"}`
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
	deleted      bool
	absent       bool
	environment  map[string]string
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
	fake.callCoreRelay()
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
