//go:build darwin || linux

package sandboxclient

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Live tests against a provisioned sandbox service.
//
// Skipped unless OPENBOX_SANDBOX_LIVE=1, so a bare checkout needs no service.
// They exist for the same reason sandbox's own tests/live_openshell.rs does:
// this client is a second implementation of a wire contract, and a fake can
// only prove it is self-consistent. The drift that matters is between this
// package and the running service.
func liveConfig(t *testing.T) Config {
	t.Helper()
	if os.Getenv("OPENBOX_SANDBOX_LIVE") != "1" {
		t.Skip("set OPENBOX_SANDBOX_LIVE=1 and provision the sandbox service to run this")
	}
	config, err := LoadConfig(os.Getenv("OPENBOX_SANDBOX_AGENT_ENV"))
	if err != nil {
		t.Fatalf("load agent.env: %v", err)
	}
	return config
}

// liveCommand lets a run assert something through its exit code, which is the
// only workload signal this path carries: a main process has no exec stream,
// and the supervisor's records do not include its stdout.
func liveCommand() []string {
	if custom := os.Getenv("OPENBOX_SANDBOX_LIVE_CMD"); custom != "" {
		return strings.Split(custom, "\x1f")
	}
	return []string{"/bin/sh", "-c", "echo openbox-live-proof"}
}

// liveEnvironment lets a probe put arbitrary variables in the guest, including
// credential-shaped ones — which is the point: this wire carries them now, and
// a test that could only send settings would not exercise that.
func liveEnvironment(runID string) map[string]string {
	values := map[string]string{"OPENBOX_EVALUATION_ID": runID}
	for _, entry := range strings.Split(os.Getenv("OPENBOX_SANDBOX_LIVE_ENV"), "\x1f") {
		name, value, found := strings.Cut(entry, "=")
		if found && name != "" {
			values[name] = value
		}
	}
	return values
}

// providersFromEnv attaches named providers. Attachment is not free: every
// credential key an attached provider declares must be bound to an endpoint in
// the run's policy, and one unbound key makes OpenShell fail closed and revoke
// the whole set — so attach only what the policy binds.
func providersFromEnv() []string {
	names := os.Getenv("OPENBOX_SANDBOX_LIVE_PROVIDERS")
	if names == "" {
		return nil
	}
	return strings.Split(names, ",")
}

func TestLiveNegotiationReportsWhatTheServiceSupports(t *testing.T) {
	config := liveConfig(t)
	client, err := New(config)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	capabilities, err := client.Capabilities(10 * time.Second)
	if err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	t.Logf("endpoint=%s template=%s capabilities=%v",
		config.Endpoint, config.AssetBundle.Template, capabilities)

	supported, err := client.SupportsProjectRun(10 * time.Second)
	if err != nil {
		t.Fatalf("supports project run: %v", err)
	}
	// Not asserted either way: what the deployment advertises is the
	// deployment's decision. What IS asserted is that the two agree, because a
	// client that disagreed with its own negotiation would be the bug.
	found := false
	for _, capability := range capabilities {
		if capability == ProjectRunCapability {
			found = true
		}
	}
	if supported != found {
		t.Fatalf("SupportsProjectRun=%v but capabilities=%v", supported, capabilities)
	}
	t.Logf("project_run_v2 supported: %v", supported)
}

// TestLiveProjectRunRoundTrip drives the whole v2 lifecycle against a real
// service and a real developer image.
//
// Requires a deployment that advertises the capability; it skips rather than
// fails otherwise, because a native-provider deployment legitimately cannot
// serve it.
func TestLiveProjectRunRoundTrip(t *testing.T) {
	config := liveConfig(t)
	image := os.Getenv("OPENBOX_SANDBOX_LIVE_IMAGE")
	policyPath := os.Getenv("OPENBOX_SANDBOX_LIVE_POLICY_FILE")
	if image == "" || policyPath == "" {
		t.Skip("set OPENBOX_SANDBOX_LIVE_IMAGE and OPENBOX_SANDBOX_LIVE_POLICY_FILE")
	}
	client, err := New(config)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	supported, err := client.SupportsProjectRun(10 * time.Second)
	if err != nil {
		t.Fatalf("supports project run: %v", err)
	}
	if !supported {
		t.Skip("this deployment does not advertise project_run_v2")
	}

	policy, identity, err := policyFromFile(policyPath)
	if err != nil {
		t.Fatalf("read policy: %v", err)
	}
	runID, err := NewRunID()
	if err != nil {
		t.Fatalf("run id: %v", err)
	}
	spec := ProjectRunSpec{
		RunID:          runID,
		Template:       image,
		PolicyDocument: policy,
		ExpectedPolicy: identity,
		Environment:    liveEnvironment(runID),
		Providers:      providersFromEnv(),
		Command:        liveCommand(),
	}

	begunID, token, err := client.Begin(spec, 5*time.Minute)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Cleanup is owed from the moment begin returns, success or not.
	defer func() {
		if err := client.Delete(begunID, 60*time.Second); err != nil {
			t.Errorf("delete: %v", err)
		}
		if err := client.WaitDeleted(begunID, 60*time.Second); err != nil {
			t.Errorf("wait deleted: %v", err)
		}
	}()

	readyToken, err := client.WaitReady(begunID, token, identity, 5*time.Minute)
	if err != nil {
		t.Fatalf("wait ready: %v", err)
	}
	result, err := client.WaitCompleted(begunID, readyToken, 4*time.Minute)
	if err != nil {
		t.Fatalf("wait completed: %v", err)
	}
	t.Logf("exit=%d records=%d", result.ExitCode, len(result.Logs))
	for _, record := range result.Logs {
		t.Logf("  [%s/%s] %s", record.Source, record.Level, record.Message)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d", result.ExitCode)
	}
}
