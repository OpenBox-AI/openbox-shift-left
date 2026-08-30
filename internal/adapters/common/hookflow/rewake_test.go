package hookflow

import (
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

func isolateMarkers(t *testing.T) {
	t.Helper()
	t.Setenv(devconfig.EnvPendingApprovalDir, t.TempDir())
}

func testKey() client.ApprovalKey {
	return client.ApprovalKey{WorkflowID: "w", RunID: "r", ActivityID: "a"}
}

func newGovernor(g *fakeGovernor) func(*log.Logger) (Governor, error) {
	return func(*log.Logger) (Governor, error) { return g, nil }
}

// TestRewakeMarkerGraceOutlastsTheEscalation the grace must outlast the worst
// case for the gate to file an approval; a full inline evaluation budget plus
// process startup.
func TestRewakeMarkerGraceOutlastsTheEscalation(t *testing.T) {
	const worstCaseEscalation = 4 * time.Second
	if rewakeMarkerGrace <= worstCaseEscalation {
		t.Fatalf("marker grace %v must exceed the %v escalation budget, or the watcher "+
			"gives up before the gate has filed", rewakeMarkerGrace, worstCaseEscalation)
	}
	if rewakeMarkerGrace < 2*worstCaseEscalation {
		t.Errorf("marker grace %v leaves under 2× margin over the %v escalation budget",
			rewakeMarkerGrace, worstCaseEscalation)
	}
}

// TestAwaitRewake_PicksUpAMarkerFiledAfterItStarted the gate files the marker
// before it starts holding, so the watcher learns within its grace rather than
// having to outwait the whole hold.
func TestAwaitRewake_PicksUpAMarkerFiledAfterItStarted(t *testing.T) {
	isolateMarkers(t)
	key := testKey()
	go func() {
		time.Sleep(300 * time.Millisecond) // the gate escalating, then filing
		RecordPendingApproval(discard(), key, "Bash")
	}()
	g := &fakeGovernor{replies: []func() (client.ApprovalStatus, error){
		decided(client.VerdictAllow, time.Now().Add(30*time.Minute)),
	}}
	if _, ok := AwaitRewake(context.Background(), discard(), key, newGovernor(g)); !ok {
		t.Error("watcher missed a marker filed while it was still in its grace period")
	}
}

// TestAwaitRewake_NoApprovalFiled the common case, and the one that has to be
// cheap: no approval was filed for this call, so the watcher gives up after
// its grace period without ever reaching the network.
func TestAwaitRewake_NoApprovalFiled(t *testing.T) {
	isolateMarkers(t)
	g := &fakeGovernor{replies: []func() (client.ApprovalStatus, error){pending(time.Now().Add(time.Hour))}}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	if _, ok := AwaitRewake(ctx, discard(), testKey(), newGovernor(g)); ok {
		t.Error("watcher woke the session for a call that filed no approval")
	}
	if g.polls != 0 {
		t.Errorf("watcher made %d polls with no marker present, want 0", g.polls)
	}
}

// TestAwaitRewake_WakesOnALateDecision the tail the hold refuses to wait for:
// the gate denied, left the marker, and a human decides minutes later.
func TestAwaitRewake_WakesOnALateDecision(t *testing.T) {
	isolateMarkers(t)
	key := testKey()
	RecordPendingApproval(discard(), key, "Bash")

	expiry := time.Now().Add(30 * time.Minute)
	g := &fakeGovernor{replies: []func() (client.ApprovalStatus, error){decided(client.VerdictAllow, expiry)}}
	msg, ok := AwaitRewake(context.Background(), discard(), key, newGovernor(g))
	if !ok {
		t.Fatal("a decision that landed after the deny must wake the session")
	}
	for _, want := range []string{"Bash", "granted", "ge-1"} {
		if !strings.Contains(msg, want) {
			t.Errorf("wake message %q missing %q", msg, want)
		}
	}
	if ClaimPendingApproval(key) {
		t.Error("the watcher must claim the marker so the outcome is announced once")
	}
}

func TestAwaitRewake_RejectionCarriesThePolicyReason(t *testing.T) {
	isolateMarkers(t)
	key := testKey()
	RecordPendingApproval(discard(), key, "mcp__github__create_issue")

	g := &fakeGovernor{replies: []func() (client.ApprovalStatus, error){
		decided(client.VerdictHalt, time.Now().Add(30*time.Minute)),
	}}
	msg, ok := AwaitRewake(context.Background(), discard(), key, newGovernor(g))
	if !ok {
		t.Fatal("a rejection must wake the session too — silence reads as still-waiting")
	}
	if !strings.Contains(msg, "refused") || !strings.Contains(msg, "approver said so") {
		t.Errorf("wake message %q should say it was refused and why", msg)
	}
}

// TestAwaitRewake_SilentWhenTheGateAlreadyHandledIt the gate answering during
// its own hold removes the marker. The watcher must then stay silent: the call
// already saw the outcome.
func TestAwaitRewake_SilentWhenTheGateAlreadyHandledIt(t *testing.T) {
	isolateMarkers(t)
	key := testKey()
	RecordPendingApproval(discard(), key, "Bash")
	if !ClaimPendingApproval(key) { // the gate's hold answered
		t.Fatal("gate could not claim its own marker")
	}

	g := &fakeGovernor{replies: []func() (client.ApprovalStatus, error){
		decided(client.VerdictAllow, time.Now().Add(30*time.Minute)),
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, ok := AwaitRewake(ctx, discard(), key, newGovernor(g)); ok {
		t.Error("watcher announced an outcome the gate had already applied to the call")
	}
}

// TestAwaitRewake_SilentWhenTheWindowClosed an expired window is not an
// outcome worth interrupting anyone for.
func TestAwaitRewake_SilentWhenTheWindowClosed(t *testing.T) {
	isolateMarkers(t)
	key := testKey()
	RecordPendingApproval(discard(), key, "Bash")

	g := &fakeGovernor{replies: []func() (client.ApprovalStatus, error){pending(time.Now().Add(-time.Minute))}}
	if _, ok := AwaitRewake(context.Background(), discard(), key, newGovernor(g)); ok {
		t.Error("a closed window must not wake the session")
	}
	if ClaimPendingApproval(key) {
		t.Error("the watcher should clean up the marker for a closed window")
	}
}

// TestPendingApprovalPath_KeyedPerCall the marker path is derived from the
// key, so the gate and the watcher; which never talk; address the same file,
// and two different calls never collide.
func TestPendingApprovalPath_KeyedPerCall(t *testing.T) {
	isolateMarkers(t)
	a := PendingApprovalPath(testKey())
	if b := PendingApprovalPath(testKey()); a != b {
		t.Errorf("same key gave different paths:\n%s\n%s", a, b)
	}
	other := client.ApprovalKey{WorkflowID: "w", RunID: "r", ActivityID: "other"}
	if PendingApprovalPath(other) == a {
		t.Error("different calls must not share a marker")
	}
}
