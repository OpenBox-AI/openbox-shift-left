package sidecar

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
)

func TestFileBundleSource_AbsentIsNoError(t *testing.T) {
	src := NewFileBundleSource(filepath.Join(t.TempDir(), "missing.json"))
	b, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("absent bundle file should not error (cold start fail-open): %v", err)
	}
	if b != nil {
		t.Errorf("absent file: got bundle %+v, want nil", b)
	}
}

func TestFileBundleSource_Loads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.json")
	if err := os.WriteFile(path, []byte(`{"version":"v9","rules":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := NewFileBundleSource(path).Fetch(context.Background())
	if err != nil || b == nil || b.Version != "v9" {
		t.Fatalf("Fetch = (%+v, %v), want v9 bundle", b, err)
	}
}

func TestFileBundleSource_UnchangedIsNoChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.json")
	if err := os.WriteFile(path, []byte(`{"version":"v1","rules":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	src := NewFileBundleSource(path)
	// First fetch loads.
	if b, err := src.Fetch(context.Background()); err != nil || b == nil {
		t.Fatalf("first Fetch = (%+v, %v), want a bundle", b, err)
	}
	// Second fetch with no change → (nil, nil): a genuine no-op, so loadedAt/Stale
	// are not reset every tick.
	b, err := src.Fetch(context.Background())
	if err != nil || b != nil {
		t.Fatalf("unchanged Fetch = (%+v, %v), want (nil, nil)", b, err)
	}
	// A newer mtime → reload. Force mtime forward (avoids clock granularity flake).
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if b, err := src.Fetch(context.Background()); err != nil || b == nil {
		t.Fatalf("changed Fetch = (%+v, %v), want a reloaded bundle", b, err)
	}
}

func TestFileBundleSource_MalformedErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileBundleSource(path).Fetch(context.Background()); err == nil {
		t.Error("malformed bundle should error (so sync keeps the current policy)")
	}
}

// errSource always fails — used to prove syncOnce keeps the current policy.
type errSource struct{}

func (errSource) Fetch(context.Context) (*Bundle, error) { return nil, errors.New("boom") }

func TestSyncOnce_KeepsCurrentOnError(t *testing.T) {
	srv := NewServer(ServerConfig{})
	srv.SetBundle(&Bundle{Version: "good", Rules: []Rule{
		{Match: RuleMatch{ToolName: "Bash"}, Decision: "block"},
	}})
	// A failing sync must NOT clear or change the loaded policy.
	syncOnce(context.Background(), srv, errSource{}, nil)
	resp := srv.decide(toolCall("Bash", client.ToolShell, nil))
	if resp.Evaluation.Verdict != client.VerdictBlock {
		t.Fatalf("policy changed after failed sync: verdict=%q, want BLOCK", resp.Evaluation.Verdict)
	}
}

// countSource returns a bundle once then nil (no change).
type countSource struct{ n int }

func (c *countSource) Fetch(context.Context) (*Bundle, error) {
	c.n++
	if c.n == 1 {
		return &Bundle{Version: "primed"}, nil
	}
	return nil, nil
}

func TestSyncLoop_PrimesImmediately(t *testing.T) {
	srv := NewServer(ServerConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &countSource{}
	go syncLoop(ctx, srv, src, 10*time.Millisecond, nil)
	waitFor(t, func() bool {
		srv.mu.RLock()
		defer srv.mu.RUnlock()
		return srv.version == "primed"
	}, 2*time.Second)
}
