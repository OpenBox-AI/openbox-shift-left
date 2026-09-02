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

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/runfs"
)

func TestParseEnvironment(t *testing.T) {
	accepted, err := parseEnvironment([]byte("# comment\nEMPTY=\nA=one=two\nLOCAL=http://127.0.0.1:8080/path\nINFERENCE=https://inference.local/v1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if accepted["A"] != "one=two" || accepted["EMPTY"] != "" {
		t.Fatalf("values=%v", accepted)
	}

	tests := map[string]string{
		"duplicate":        "A=1\nA=2\n",
		"export":           "export A=1\n",
		"reserved":         "OPENBOX_URL=http://127.0.0.1:1\n",
		"credential":       "SERVICE_TOKEN=value\n",
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
	values, err := effectiveEnvironment(
		[]string{"PATH=/usr/bin", "A=image", "OPENAI_API_KEY=unused"},
		map[string]string{"A": "file", "B": "file"}, "ev-one", "agent-one",
	)
	if err != nil {
		t.Fatal(err)
	}
	if values["A"] != "file" || values["OPENAI_API_KEY"] != "unused" || values["OPENBOX_EVALUATION_ID"] != "ev-one" {
		t.Fatalf("values=%v", values)
	}
	wantNames := []string{"A", "B", "OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_MODEL", "OPENBOX_AGENT_ID", "OPENBOX_API_KEY", "OPENBOX_EVALUATION_ID", "PATH"}
	if got := environmentInventory(values); !reflect.DeepEqual(got, wantNames) {
		t.Fatalf("names=%v want=%v", got, wantNames)
	}
	if _, err := effectiveEnvironment([]string{"SERVICE_SECRET=x"}, nil, "ev", "agent"); err == nil {
		t.Fatal("accepted embedded credential")
	}
	if _, err := effectiveEnvironment([]string{"OPENBOX_API_KEY=embedded"}, nil, "ev", "agent"); err == nil {
		t.Fatal("accepted embedded reserved credential")
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

func TestPolicyIsCanonicalAndNarrow(t *testing.T) {
	first, err := buildPolicy("/app", "/usr/local/bin/node", 49152)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := buildPolicy("/app", "/usr/local/bin/node", 49152)
	if !bytes.Equal(first, second) {
		t.Fatal("policy bytes changed")
	}
	var policy map[string]any
	if err := json.Unmarshal(first, &policy); err != nil {
		t.Fatal(err)
	}
	text := string(first)
	for _, required := range []string{"host.openshell.internal", `"allowed_ips":["192.168.127.254/32"]`, "obx-openbox-local", "/usr/local/bin/node", "/api/v1/auth/validate", "/api/v1/governance/evaluate", "/api/v1/governance/approval"} {
		if !strings.Contains(text, required) {
			t.Fatalf("policy missing %q: %s", required, text)
		}
	}
	for _, forbidden := range []string{"inference.local", "**", `"access":"full"`} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("policy contains %q: %s", forbidden, text)
		}
	}
}

func TestSandboxCreateCommandIsAttachedAndDirect(t *testing.T) {
	workspace, err := runfs.Create(filepath.Join(t.TempDir(), "output"))
	if err != nil {
		t.Fatal(err)
	}
	state := &runState{
		prepared: &prepared{
			sandboxName: "obx-eval-one", evaluationID: "ev-one",
			argv: []string{"/usr/local/bin/node", "/app/index.mjs"},
			environment: map[string]string{
				"OPENBOX_URL":    "http://host.openshell.internal:49152",
				"OPENAI_API_KEY": "unused",
				"Z_VALUE":        "last",
			},
		},
		workspace:          workspace,
		immutableReference: "127.0.0.1:49153/ai.openbox/evaluation@sha256:" + strings.Repeat("b", 64),
	}
	command := state.sandboxCreateCommand()
	joined := strings.Join(command.Args, " ")
	for _, required := range []string{"--no-auto-providers", "--no-tty", "--no-keep", "--cpu 2", "--memory 2Gi", "-- /usr/local/bin/node /app/index.mjs"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("missing %q: %s", required, joined)
		}
	}
	for _, forbidden := range []string{"--detach", "--forward", "--upload", " sandbox exec ", "provider create", "Dockerfile"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("forbidden %q: %s", forbidden, joined)
		}
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
	if err := os.WriteFile(envFile, []byte("APP_ENV=security-test\n"), 0o600); err != nil {
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
	want := []string{".incomplete", "execution.json", "openshell.log", "policy.json", "process.stderr", "process.stdout"}
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
	if record.Image.ImmutableReference != record.Image.LocalID || !strings.HasPrefix(record.Image.PublishedReference, "127.0.0.1:") {
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
		configure      func(*lifecycleRunner, *lifecycleHTTP)
		classification string
	}{
		{name: "registry refusal", configure: func(_ *lifecycleRunner, client *lifecycleHTTP) { client.manifestStatus = http.StatusBadRequest }, classification: "registry_refusal"},
		{name: "pre-ready Error", configure: func(runner *lifecycleRunner, _ *lifecycleHTTP) { runner.phaseError = true }, classification: "sandbox_pre_ready_error"},
		{name: "command nonzero", configure: func(runner *lifecycleRunner, _ *lifecycleHTTP) { runner.commandExit = 7 }, classification: "command_nonzero"},
		{name: "cleanup overrides success", configure: func(runner *lifecycleRunner, _ *lifecycleHTTP) { runner.cleanupTagStays = true }, classification: "cleanup_failure"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parent := t.TempDir()
			envFile := filepath.Join(parent, "evaluation.env")
			if err := os.WriteFile(envFile, []byte("# empty\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(parent, "record")
			runner := newLifecycleRunner()
			client := &lifecycleHTTP{agentID: "c59e95b6-2a4e-44a7-8c43-b69bfa77667e"}
			test.configure(runner, client)
			result, err := Run(context.Background(), Input{Image: "example:local", EnvFile: envFile, OpenBoxAgent: client.agentID, Output: output}, Dependencies{
				Commands: runner, Clock: realClock{}, Random: bytes.NewReader(bytes.Repeat([]byte{0x31}, 12)),
				Listen: net.Listen, HTTP: client, GOOS: "darwin", GOARCH: "arm64",
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

type lifecycleProcess struct {
	ctx                 context.Context
	trigger             <-chan struct{}
	evaluationID, relay string
	result              CommandResult
}

type lifecycleLogProcess struct{ ctx context.Context }

func (process *lifecycleLogProcess) Wait() CommandResult {
	<-process.ctx.Done()
	return CommandResult{Stdout: []byte("sandbox live log\n"), ExitCode: -1}
}

func (process *lifecycleProcess) Wait() CommandResult {
	select {
	case <-process.trigger:
	case <-process.ctx.Done():
		return CommandResult{ExitCode: -1}
	}
	relayURL := strings.Replace(process.relay, "host.openshell.internal", "127.0.0.1", 1)
	response, _ := http.Get(relayURL + "/api/v1/auth/validate")
	if response != nil {
		response.Body.Close()
	}
	request, _ := http.NewRequest(http.MethodPost, relayURL+"/api/v1/governance/evaluate", strings.NewReader(`{"run_id":"`+process.evaluationID+`"}`))
	request.Header.Set("content-type", "application/json")
	response, _ = http.DefaultClient.Do(request)
	if response != nil {
		response.Body.Close()
	}
	return process.result
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
	case command.Name == "openshell" && args == "--version":
		return CommandResult{Stdout: []byte("openshell 0.0.111\n")}, nil
	case command.Name == "openshell" && args == "status -o json":
		return jsonResult(map[string]any{"status": "connected", "version": "0.0.111", "server": "https://localhost:17670", "authentication": map[string]string{"status": "authenticated", "provider": "mTLS transport"}}), nil
	case command.Name == "openshell" && args == "gateway info -o json":
		return jsonResult(map[string]any{"status": "healthy", "version": "0.0.111", "compute_drivers": []any{map[string]any{"name": "vm", "capabilities": map[string]string{"driver_name": "openshell-driver-vm", "driver_version": "0.0.111"}}}}), nil
	case command.Name == "openshell" && args == "provider get "+OpenBoxProvider:
		return CommandResult{Stdout: []byte("Name: obx-openbox-local\nType: openbox-local\nCredential keys: OPENBOX_API_KEY\nConfig keys: <none>\n")}, nil
	case command.Name == "openshell" && args == "provider get "+InferenceProvider:
		return CommandResult{Stdout: []byte("Name: openai-compatible-provider\nType: openai\nCredential keys: OPENAI_API_KEY\nConfig keys: OPENAI_BASE_URL\n")}, nil
	case command.Name == "openshell" && args == "inference get":
		return CommandResult{Stdout: []byte("Provider: openai-compatible-provider\nModel: granite4.1:3b\n")}, nil
	case command.Name == "openshell" && strings.HasPrefix(args, "sandbox list --selector "):
		return CommandResult{Stdout: []byte("[]")}, nil
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
	case command.Name == "openshell" && strings.HasPrefix(args, "sandbox get "):
		if runner.deleted {
			return CommandResult{Stderr: []byte("sandbox not found")}, errors.New("not found")
		}
		runner.getCount++
		if runner.phaseError {
			return CommandResult{Stdout: []byte(`{"phase":"Error"}`)}, nil
		}
		if runner.getCount == 1 {
			return CommandResult{Stdout: []byte(`{"phase":"Provisioning"}`)}, nil
		}
		runner.triggerOnce.Do(func() { close(runner.trigger) })
		return CommandResult{Stdout: []byte(`{"phase":"Ready"}`)}, nil
	case command.Name == "openshell" && strings.HasPrefix(args, "logs --source all "):
		return CommandResult{Stdout: []byte("sandbox log\n")}, nil
	case command.Name == "openshell" && strings.HasPrefix(args, "sandbox delete "):
		runner.deleted = true
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

func (runner *lifecycleRunner) Start(ctx context.Context, command Command) (Process, error) {
	runner.mu.Lock()
	runner.commands = append(runner.commands, command)
	runner.mu.Unlock()
	if command.Name == "openshell" && len(command.Args) >= 2 && command.Args[0] == "logs" && command.Args[1] == "--tail" {
		return &lifecycleLogProcess{ctx: ctx}, nil
	}
	if command.Name != "openshell" || len(command.Args) < 3 || command.Args[0] != "sandbox" || command.Args[1] != "create" {
		return nil, errors.New("unexpected start")
	}
	var relay, evaluationID string
	for index, argument := range command.Args {
		if argument == "--env" && index+1 < len(command.Args) {
			name, value, _ := strings.Cut(command.Args[index+1], "=")
			if name == "OPENBOX_URL" {
				relay = value
			}
			if name == "OPENBOX_EVALUATION_ID" {
				evaluationID = value
			}
		}
	}
	return &lifecycleProcess{ctx: ctx, trigger: runner.trigger, relay: relay, evaluationID: evaluationID, result: CommandResult{Stdout: []byte("application complete\n"), ExitCode: runner.commandExit}}, nil
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
	case request.URL.String() == coreURL+"/":
		body = "hello world"
	case request.URL.String() == backendHealthURL:
		body = `{"status":200}`
	case request.URL.String() == ollamaTagsURL:
		body = `{"models":[{"name":"granite4.1:3b","digest":"` + strings.TrimPrefix(InferenceModelDigest, "sha256:") + `"}]}`
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
