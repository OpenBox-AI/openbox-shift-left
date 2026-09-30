package muse

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditOfACompleteInstallHasNoProblems(t *testing.T) {
	i, path := newTestInstaller(t)
	if err := i.Install(testRef); err != nil {
		t.Fatal(err)
	}
	a, err := AuditSettings(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Present || len(a.Owned) != a.Expected || a.Expected != ExpectedHandlers() {
		t.Errorf("audit = %+v", a)
	}
	if p := a.Problems(); len(p) != 0 {
		t.Errorf("problems = %v", p)
	}
}

func TestAuditCountsWhatIsMissingOrDuplicated(t *testing.T) {
	i, path := newTestInstaller(t)
	if err := i.Install(testRef); err != nil {
		t.Fatal(err)
	}
	doc, _ := parseSettings([]byte(readFile(t, path)))
	doc.Handlers = doc.Handlers[2:] // two events lose their handler
	doc.Handlers = append(doc.Handlers, doc.Handlers[0])
	a := auditDoc(doc, "")
	if len(a.Missing) != 2 || len(a.Duplicate) != 1 {
		t.Errorf("missing %v duplicate %v", a.Missing, a.Duplicate)
	}
	if len(a.Problems()) < 2 {
		t.Errorf("problems = %v", a.Problems())
	}
}

func TestAuditFlagsAGatedHandlerWithoutASuccessor(t *testing.T) {
	i, path := newTestInstaller(t)
	if err := i.Install(testRef); err != nil {
		t.Fatal(err)
	}
	// The same install with every onFailure successor dropped.
	var doc map[string]any
	if err := json.Unmarshal([]byte(readFile(t, path)), &doc); err != nil {
		t.Fatal(err)
	}
	for _, groups := range doc["hooks"].(map[string]any) {
		for _, g := range groups.([]any) {
			for _, h := range g.(map[string]any)["hooks"].([]any) {
				delete(h.(map[string]any), "onFailure")
			}
		}
	}
	raw, _ := json.Marshal(doc)
	writeFile(t, path, string(raw))
	a, err := AuditSettings(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(a.NoSuccessor) != 4 {
		t.Errorf("NoSuccessor = %v, want the four gated events", a.NoSuccessor)
	}
}

func TestAuditFlagsAWrongSuccessor(t *testing.T) {
	doc, err := parseSettings([]byte(`{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"\"/e\" hook muse PreToolUse","onFailure":{"type":"command","command":"\"/e\" hook muse --fail-closed Stop"}}]}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if a := auditDoc(doc, ""); len(a.NoSuccessor) != 1 || a.NoSuccessor[0] != "PreToolUse" {
		t.Errorf("a successor for another event counted: %v", a.NoSuccessor)
	}
}

func TestAuditFlagsHandlersThatLackTheCurrentHome(t *testing.T) {
	i, path := newTestInstaller(t) // installed for the default home
	if err := i.Install(testRef); err != nil {
		t.Fatal(err)
	}
	a, err := AuditSettings(path, "/custom/openbox")
	if err != nil {
		t.Fatal(err)
	}
	if len(a.WrongHome) != ExpectedHandlers() {
		t.Errorf("WrongHome = %v, want every event", a.WrongHome)
	}
	if !strings.Contains(strings.Join(a.Problems(), ";"), "--home") {
		t.Errorf("problems = %v", a.Problems())
	}
}

func TestAuditOfAMissingOrUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	a, err := AuditSettings(filepath.Join(dir, "none.json"), "")
	if err != nil || a.Present || a.Expected != ExpectedHandlers() {
		t.Errorf("absent file = %+v, %v", a, err)
	}
	bad := filepath.Join(dir, "bad.json")
	writeFile(t, bad, `{"hooks":`)
	a, err = AuditSettings(bad, "")
	if err == nil || !a.Present {
		t.Errorf("unreadable file = %+v, %v", a, err)
	}
}

func TestParseInvocationShapes(t *testing.T) {
	cases := map[string]struct {
		ok  bool
		inv invocation
	}{
		`"/a b/openbox" hook muse PreToolUse`:               {true, invocation{Engine: "/a b/openbox", Event: "PreToolUse"}},
		`/bin/openbox hook muse Stop`:                       {true, invocation{Engine: "/bin/openbox", Event: "Stop"}},
		`"/e" hook muse --home "/h h" PostToolUse`:          {true, invocation{Engine: "/e", Home: "/h h", Event: "PostToolUse"}},
		`"/e" hook muse --home /h --fail-closed PreLLMCall`: {true, invocation{Engine: "/e", Home: "/h", FailClosed: true, Event: "PreLLMCall"}},
		`"/e" hook muse --fail-closed PreToolUse`:           {true, invocation{Engine: "/e", FailClosed: true, Event: "PreToolUse"}},
		`"/e" hook muse`:                                    {},
		`"/e" hook muse Bogus`:                              {},
		`"/e" hook muse PreToolUse trailing`:                {},
		`"/e" hook codex PreToolUse`:                        {},
		`"/e" hook muse --home PreToolUse`:                  {},
		`a && "/e" hook muse PreToolUse`:                    {},
		`"/e hook muse PreToolUse`:                          {},
		`"/e" hook muse --fail-closed --home /h PreToolUse`: {},
		``: {},
	}
	for in, want := range cases {
		got, ok := parseInvocation(in)
		if ok != want.ok || (ok && got != want.inv) {
			t.Errorf("parseInvocation(%q) = %+v, %v; want %+v, %v", in, got, ok, want.inv, want.ok)
		}
	}
}

func auditOf(t *testing.T, handler string) SettingsAudit {
	t.Helper()
	doc, err := parseSettings([]byte(`{"hooks":{"PreToolUse":[` + handler + `]}}`))
	if err != nil {
		t.Fatal(err)
	}
	return auditDoc(doc, "")
}

const okGated = `"type":"command","command":"\"/e\" hook muse PreToolUse","timeout":30,"onFailure":{"type":"command","command":"\"/e\" hook muse --fail-closed PreToolUse"}`

func TestAuditFlagsAnOwnedHandlerThatCannotGovern(t *testing.T) {
	cases := map[string]struct {
		handler string
		field   func(SettingsAudit) []string
	}{
		"matcher":       {`{"matcher":"Bash","hooks":[{` + okGated + `}]}`, func(a SettingsAudit) []string { return a.NotCatchAll }},
		"async":         {`{"hooks":[{` + okGated + `,"async":true}]}`, func(a SettingsAudit) []string { return a.Async }},
		"short timeout": {`{"hooks":[{"type":"command","command":"\"/e\" hook muse PreToolUse","timeout":5,"onFailure":{"type":"command","command":"\"/e\" hook muse --fail-closed PreToolUse"}}]}`, func(a SettingsAudit) []string { return a.ShortTimeout }},
	}
	for name, c := range cases {
		a := auditOf(t, c.handler)
		if got := c.field(a); len(got) != 1 || got[0] != "PreToolUse" {
			t.Errorf("%s: flagged %v", name, got)
		}
		if len(a.GatedFailures()) == 0 {
			t.Errorf("%s: not a gated failure", name)
		}
		if len(a.Problems()) < 1 {
			t.Errorf("%s: no problem", name)
		}
	}
}

func TestAuditAcceptsCatchAllSpellingsAndAbsentTimeout(t *testing.T) {
	for _, h := range []string{
		`{"matcher":"*","hooks":[{` + okGated + `}]}`,
		`{"matcher":"","hooks":[{` + okGated + `}]}`,
		`{"hooks":[{"type":"command","command":"\"/e\" hook muse PreToolUse","onFailure":{"type":"command","command":"\"/e\" hook muse --fail-closed PreToolUse"}}]}`,
		`{"hooks":[{` + okGated + `,"async":false}]}`,
	} {
		a := auditOf(t, h)
		if len(a.NotCatchAll)+len(a.Async)+len(a.ShortTimeout) != 0 || len(a.GatedFailures()) != 0 {
			t.Errorf("%s flagged: %+v", h, a)
		}
	}
}

func TestGatedFailuresIgnoreObserverHandlers(t *testing.T) {
	doc, _ := parseSettings([]byte(`{"hooks":{"PostToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"\"/e\" hook muse PostToolUse","async":true}]}]}}`))
	a := auditDoc(doc, "")
	if len(a.NotCatchAll) != 1 || len(a.Async) != 1 {
		t.Errorf("observer problems not recorded: %+v", a)
	}
	if len(a.GatedFailures()) != 0 {
		t.Errorf("an observer handler counted as a gated failure: %v", a.GatedFailures())
	}
}

func TestValidateRejectsANonBooleanAsync(t *testing.T) {
	if _, err := ValidateSettings([]byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"x","async":"yes"}]}]}}`)); err == nil {
		t.Error("async: \"yes\" accepted")
	}
}
