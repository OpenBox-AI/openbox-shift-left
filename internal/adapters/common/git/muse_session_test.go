package git

import "testing"

func museResolver(env map[string]string, lookup func(string) (string, bool)) SessionResolver {
	return SessionResolver{
		Getenv:        func(k string) string { return env[k] },
		ToolUseLookup: lookup,
	}
}

func TestResolve_MuseToolUseHitResolvesToItsSession(t *testing.T) {
	var asked string
	r := museResolver(map[string]string{EnvMuseToolUseID: " call_1 "}, func(id string) (string, bool) {
		asked = id
		return "muse-parent", true
	})
	got := r.ResolveDetailed("")
	want := []ResolvedSession{{ID: "muse-parent", Tier: TierMuseToolUse, Tool: "muse"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("ResolveDetailed = %+v, want %+v", got, want)
	}
	if asked != "call_1" {
		t.Fatalf("lookup asked for %q, want the trimmed id", asked)
	}
}

func TestResolve_MuseToolUseOutranksSessionEnvButNotCodex(t *testing.T) {
	lookup := func(string) (string, bool) { return "muse-s", true }
	env := map[string]string{EnvMuseToolUseID: "call_1", EnvSession: "override"}
	if got := museResolver(env, lookup).Resolve(""); len(got) != 1 || got[0] != "muse-s" {
		t.Fatalf("Resolve = %v, want the Muse session ahead of OPENBOX_SESSION", got)
	}
	env[EnvCodexThreadID] = "th-1"
	if got := museResolver(env, lookup).Resolve(""); len(got) != 1 || got[0] != "th-1" {
		t.Fatalf("Resolve = %v, want CODEX_THREAD_ID to keep tier 0", got)
	}
}

func TestResolve_MuseToolUseMissFallsThrough(t *testing.T) {
	env := map[string]string{EnvMuseToolUseID: "call_1", EnvSession: "override"}
	r := museResolver(env, func(string) (string, bool) { return "", false })
	got := r.ResolveDetailed("")
	if len(got) != 1 || got[0].ID != "override" || got[0].Tier != TierSessionEnv {
		t.Fatalf("ResolveDetailed = %+v, want the session-env tier after an index miss", got)
	}
}

func TestResolve_NilLookupIgnoresTheMuseMarker(t *testing.T) {
	env := map[string]string{EnvMuseToolUseID: "call_1", EnvSession: "override"}
	got := museResolver(env, nil).ResolveDetailed("")
	if len(got) != 1 || got[0].ID != "override" {
		t.Fatalf("ResolveDetailed = %+v, want behaviour identical to no marker", got)
	}
}

func TestResolve_MuseToolUseNeedsAMarkerToAskTheLookup(t *testing.T) {
	r := museResolver(map[string]string{}, func(string) (string, bool) {
		t.Fatal("lookup consulted with no MUSE_TOOL_USE_ID")
		return "", false
	})
	if got := r.Resolve(""); got != nil {
		t.Fatalf("Resolve = %v, want none", got)
	}
}
