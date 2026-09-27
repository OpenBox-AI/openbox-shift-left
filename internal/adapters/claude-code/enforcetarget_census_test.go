package claudecode

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// censusMCPProbe stands in for every mcp__* tool name: classifyTool routes
// all of them identically off the "mcp__" prefix rather than a per-name
// builtinTools entry, so one probe name exercises the whole class.
const censusMCPProbe = "mcp__probe__lookup"

// toolCensusClass is the literal, hand-authored decision for what an
// enforce-path evaluation body carries for one tool class.
type toolCensusClass int

const (
	// censusVerbatim: the extract (a shell command, an MCP call's raw
	// tool_input) reaches /evaluate unredacted. Deliberate and documented
	// (docs/data-and-privacy.md, §What an enforced call sends): a policy
	// deciding whether a command is
	// dangerous has to see the command that will actually run.
	censusVerbatim toolCensusClass = iota
	// censusRebuilt: a file-write body is spliced back in via
	// hookflow.RedactToolInput once the local detector has already redacted
	// it; every other field survives byte-for-byte.
	censusRebuilt
	// censusRedacted: everything else -- a subagent prompt, a search query, a
	// file read's arguments, a glob/grep pattern -- must be scanned and
	// redacted like any other content body ("redacted then capped", the
	// subagent-prompt row of docs/data-and-privacy.md's capture table).
	censusRedacted
)

func censusLabel(c toolCensusClass) string {
	switch c {
	case censusRebuilt:
		return "rebuilt class"
	case censusRedacted:
		return "redacted class"
	default:
		return "verbatim class"
	}
}

// toolCensusTable is a literal table: every builtinTools entry, plus the one
// mcp__ probe standing in for the whole MCP class, gets an explicit redaction
// decision here. It is deliberately NOT derived from classifyTool or
// builtinTools -- a census read out of the same map it checks asserts nothing.
// An entry with no row fails the test outright ("classify me") instead of
// silently inheriting a classification nobody chose.
var toolCensusTable = map[string]toolCensusClass{
	"Bash":         censusVerbatim,
	"BashOutput":   censusVerbatim,
	"KillShell":    censusVerbatim,
	censusMCPProbe: censusVerbatim,

	"Write":        censusRebuilt,
	"Edit":         censusRebuilt,
	"MultiEdit":    censusRebuilt,
	"NotebookEdit": censusRebuilt,

	"Agent":        censusRedacted,
	"ToolSearch":   censusRedacted,
	"Read":         censusRedacted,
	"NotebookRead": censusRedacted,
	"Glob":         censusRedacted,
	"Grep":         censusRedacted,
}

// censusFixture builds one class's representative tool_input, with
// testSentinel planted in the field the current code actually extracts
// content from, plus the redacted-FileText argument a real caller supplies
// for a rebuilt (file-write) class -- the local detector always runs before
// DevEvent is ever called, so a nil redacted here would only prove the
// "nothing to redact" case, not the mechanism. A simplification, not a wire
// fixture: MultiEdit/NotebookEdit's real Claude Code shape nests
// old_string/new_string per edit rather than at the top level, but
// hookflow.RedactToolInput and HookEvent.fileText() only ever read a
// top-level "content"/"new_string" key (outputcontract.go's
// contentFieldKeys), so a flat shape is what today's code actually keys off.
// Likewise BashOutput/KillShell take bash_id/shell_id on the real wire, not
// command; classifyTool still routes them through the shared
// {ToolShell,"internal"} commandOf path, so a "command" fixture is what
// exercises that shared path -- a separate gap this census does not cover.
func censusFixture(t *testing.T, name string) (json.RawMessage, *client.Content) {
	t.Helper()
	switch name {
	case "Bash", "BashOutput", "KillShell":
		return json.RawMessage(`{"command":"` + testSentinel + `"}`), nil
	case censusMCPProbe:
		return json.RawMessage(`{"query":"` + testSentinel + `"}`), nil
	case "Write":
		return json.RawMessage(`{"file_path":"/tmp/a","content":"` + testSentinel + `"}`),
			&client.Content{FileText: "[REDACTED]"}
	case "Edit", "MultiEdit", "NotebookEdit":
		return json.RawMessage(`{"file_path":"/tmp/a","old_string":"x","new_string":"` + testSentinel + `"}`),
			&client.Content{FileText: "[REDACTED]"}
	case "Agent":
		return json.RawMessage(`{"description":"d","subagent_type":"code-reviewer","prompt":"` + testSentinel + `"}`), nil
	case "ToolSearch":
		return json.RawMessage(`{"query":"` + testSentinel + `","max_results":5}`), nil
	case "Read":
		return json.RawMessage(`{"file_path":"` + testSentinel + `"}`), nil
	case "NotebookRead":
		return json.RawMessage(`{"notebook_path":"` + testSentinel + `"}`), nil
	case "Glob", "Grep":
		return json.RawMessage(`{"pattern":"` + testSentinel + `"}`), nil
	default:
		t.Fatalf("censusFixture: no representative tool_input wired for %q; add one alongside its toolCensusTable row", name)
		return nil, nil
	}
}

// TestEnforceCopyRedactionByClass is the classification seam: for every tool
// class enforceTarget.DevEvent can see, does the content it attaches for the
// synchronous /evaluate call match the class's assigned bucket? It
// constructs enforceTarget{mapper: m} directly, so it is blind to real
// wiring (the conformance case covers that); what it catches is a class
// entering builtinTools, or losing its assignment, without an explicit
// redaction decision -- this would have gone red the day Agent/ToolSearch
// were added.
func TestEnforceCopyRedactionByClass(t *testing.T) {
	names := make([]string, 0, len(builtinTools)+1)
	for name := range builtinTools {
		names = append(names, name)
	}
	names = append(names, censusMCPProbe)

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			class, ok := toolCensusTable[name]
			if !ok {
				t.Fatalf("classify me: %q has no row in toolCensusTable; every builtinTools "+
					"entry (or the mcp__ probe) must get an explicit verbatim/rebuilt/redacted "+
					"decision before it can reach this test", name)
			}

			input, redacted := censusFixture(t, name)
			target := enforceTarget{
				id:     Identity{DeveloperDID: testDID},
				mapper: testMapper(),
				ev:     &HookEvent{SessionID: "s1", ToolName: name, ToolInput: input},
			}
			ev, ok := target.DevEvent(redacted)
			if !ok {
				t.Fatal("DevEvent ok=false")
			}
			var got string
			if ev.Content != nil {
				got = ev.Content.ToolInput
			}

			switch class {
			case censusVerbatim:
				if !strings.Contains(got, testSentinel) {
					t.Errorf("verbatim class %q: sentinel absent from the /evaluate content "+
						"(it should reach it unredacted, docs/data-and-privacy.md, §What an "+
						"enforced call sends); got %q", name, got)
				}
			case censusRebuilt, censusRedacted:
				if strings.Contains(got, testSentinel) {
					t.Errorf("%s %q: the raw sentinel reached /evaluate content unredacted; got %q",
						censusLabel(class), name, got)
				}
				if !strings.Contains(got, "[REDACTED]") {
					t.Errorf("%s %q: no redaction placeholder attached; got %q", censusLabel(class), name, got)
				}
			}
		})
	}
}

// TestEnforceCopyBoundsEveryArm pins MaxRedactBody on the redacted default,
// not just on the three override arms. The bound cannot be delegated to
// RedactText: Mapper.redact is the identity function when RedactContent is
// nil, which is exactly how hookrun wires the mapper with secret_detection
// off, and RedactText itself returns early on a nil redactor without
// truncating. So with no redactor the default arm is the only place the first
// of the two egress bounds can be applied at all.
func TestEnforceCopyBoundsEveryArm(t *testing.T) {
	oversized := strings.Repeat("z", hookflow.MaxRedactBody+4096)

	for _, tc := range []struct {
		name  string
		tool  string
		input json.RawMessage
	}{
		{"redacted default", "Agent", json.RawMessage(`{"prompt":"` + oversized + `"}`)},
		{"verbatim shell", "Bash", json.RawMessage(`{"command":"` + oversized + `"}`)},
		{"verbatim mcp", censusMCPProbe, json.RawMessage(`{"query":"` + oversized + `"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testMapper()
			m.RedactContent = nil // secret_detection off
			tgt := enforceTarget{
				id:     Identity{DeveloperDID: testDID},
				mapper: m,
				ev:     &HookEvent{SessionID: "s1", ToolName: tc.tool, ToolInput: tc.input},
			}
			ev, ok := tgt.DevEvent(nil)
			if !ok || ev.Content == nil {
				t.Fatalf("DevEvent ok=%v content=%v", ok, ev.Content)
			}
			if got := len(ev.Content.ToolInput); got > hookflow.MaxRedactBody {
				t.Errorf("enforce copy unbounded: got %d bytes, want <= %d", got, hookflow.MaxRedactBody)
			}
		})
	}
}

// TestEnforceCopyVerbatimArmIsAnAllowlist pins that the verbatim shell
// carve-out tests membership in builtinTools, not just the tool kind.
// classifyTool's default is shell/"internal", so a kind-only test puts every
// name the provider adds next -- and every typo -- on the verbatim path by
// fallthrough, unseen by the census above, which iterates builtinTools.
func TestEnforceCopyVerbatimArmIsAnAllowlist(t *testing.T) {
	input := json.RawMessage(`{"command":"` + testSentinel + `"}`)
	tgt := enforceTarget{
		id:     Identity{DeveloperDID: testDID},
		mapper: testMapper(),
		ev:     &HookEvent{SessionID: "s1", ToolName: "SomeFutureShellTool", ToolInput: input},
	}
	ev, ok := tgt.DevEvent(nil)
	if !ok {
		t.Fatal("DevEvent ok=false")
	}
	var got string
	if ev.Content != nil {
		got = ev.Content.ToolInput
	}
	if strings.Contains(got, testSentinel) {
		t.Errorf("a tool name absent from builtinTools reached /evaluate verbatim; got %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("unrecognized tool name: no redaction placeholder attached; got %q", got)
	}
}
