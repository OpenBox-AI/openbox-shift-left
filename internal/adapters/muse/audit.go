package muse

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// OwnedHandler is one OpenBox handler found in a settings file.
type OwnedHandler struct {
	Event  HookName
	Engine string
	// Home is the --home baked into the command, "" when none is.
	Home string
	// HasSuccessor reports a valid deny-only onFailure successor for this event.
	HasSuccessor bool
}

// SettingsAudit is what doctor reads from a settings file: how many OpenBox
// handlers it holds against how many a complete install registers, and which
// ones are wrong.
type SettingsAudit struct {
	Path    string
	Present bool
	// Expected is the handler count of a complete install.
	Expected int
	Owned    []OwnedHandler
	// Missing names installed events with no owned handler.
	Missing []string
	// Duplicate names events registered more than once.
	Duplicate []string
	// NoSuccessor names gated events whose handler has no valid successor, so a
	// crash or a rejected answer there is an allow.
	NoSuccessor []string
	// NotCatchAll names events whose handler sits in a group with a matcher, so
	// the tools or prompts it does not name are ungoverned.
	NotCatchAll []string
	// Async names events whose handler is async: it is not waited for, so on a
	// gated event it cannot block.
	Async []string
	// ShortTimeout names gated events whose handler times out below the gated
	// ceiling, which can kill the hook mid-decision and let the call through.
	ShortTimeout []string
	// WrongHome names events whose command bakes a different --home than the
	// one this machine runs with (none at all, when it is not the default): the
	// hook would bind another store, or none.
	WrongHome []string
	// Warnings are findings that leave the file readable.
	Warnings []string
}

// Problems lists what makes the install incomplete or unsafe, empty when it is
// whole.
func (a SettingsAudit) Problems() []string {
	var out []string
	if len(a.Missing) > 0 {
		out = append(out, fmt.Sprintf("no OpenBox handler for %v", a.Missing))
	}
	if len(a.Duplicate) > 0 {
		out = append(out, fmt.Sprintf("more than one OpenBox handler for %v", a.Duplicate))
	}
	if len(a.NoSuccessor) > 0 {
		out = append(out, fmt.Sprintf("no deny-only onFailure successor on %v", a.NoSuccessor))
	}
	if len(a.NotCatchAll) > 0 {
		out = append(out, fmt.Sprintf("handler sits under a matcher, so it is not catch-all, on %v", a.NotCatchAll))
	}
	if len(a.Async) > 0 {
		out = append(out, fmt.Sprintf("handler is async and cannot block on %v", a.Async))
	}
	if len(a.ShortTimeout) > 0 {
		out = append(out, fmt.Sprintf("gated handler timeout is below %ds on %v", gatedHookTimeoutSec(), a.ShortTimeout))
	}
	if len(a.WrongHome) > 0 {
		out = append(out, fmt.Sprintf("--home does not match this machine's OpenBox home on %v", a.WrongHome))
	}
	return out
}

// GatedFailures are the problems that leave a gated call ungoverned or able to
// fail open: a gated handler with no successor, async, under a matcher or with
// too short a timeout, in the order found. A missing handler is reported apart
// (Missing) and is not repeated here.
func (a SettingsAudit) GatedFailures() []string {
	var out []string
	if len(a.NoSuccessor) > 0 {
		out = append(out, fmt.Sprintf("no deny-only onFailure successor on %v", a.NoSuccessor))
	}
	for _, list := range []struct {
		what   string
		events []string
	}{
		{"not catch-all (under a matcher)", a.NotCatchAll},
		{"async, so it cannot block", a.Async},
		{"timeout below the gated ceiling", a.ShortTimeout},
	} {
		if g := gatedOnly(list.events); len(g) > 0 {
			out = append(out, fmt.Sprintf("%s: %v", list.what, g))
		}
	}
	return out
}

func gatedOnly(events []string) []string {
	var out []string
	for _, e := range events {
		if HookName(e).Gated() {
			out = append(out, e)
		}
	}
	return out
}

// auditDoc audits a parsed settings document. wantHome is the --home every
// handler must carry: "" for the default home, which needs none.
func auditDoc(doc settingsDoc, wantHome string) SettingsAudit {
	a := SettingsAudit{Expected: ExpectedHandlers(), Warnings: doc.Warnings}
	counts := map[HookName]int{}
	homeBad := map[HookName]bool{}
	noSucc := map[HookName]bool{}
	notCatchAll := map[HookName]bool{}
	async := map[HookName]bool{}
	short := map[HookName]bool{}
	for _, h := range doc.Handlers {
		if !h.Owned || h.Invocation.FailClosed {
			continue
		}
		ev := h.Invocation.Event
		counts[ev]++
		o := OwnedHandler{Event: ev, Engine: h.Invocation.Engine, Home: h.Invocation.Home}
		if s := h.OnFailure; s != nil && s.Type == "command" {
			if inv, ok := parseInvocation(s.Command); ok && inv.FailClosed && inv.Event == ev {
				o.HasSuccessor = true
				if inv.Home != wantHome {
					homeBad[ev] = true
				}
			}
		}
		if o.Home != wantHome {
			homeBad[ev] = true
		}
		if m := h.Matcher; m != nil && *m != "" && *m != "*" {
			notCatchAll[ev] = true
		}
		if h.Async {
			async[ev] = true
		}
		if ev.Gated() && h.Timeout > 0 && h.Timeout < gatedHookTimeoutSec() {
			short[ev] = true
		}
		if ev.Gated() && !o.HasSuccessor {
			noSucc[ev] = true
		}
		a.Owned = append(a.Owned, o)
	}
	for _, ev := range installedEvents() {
		switch n := counts[ev]; {
		case n == 0:
			a.Missing = append(a.Missing, string(ev))
		case n > 1:
			a.Duplicate = append(a.Duplicate, string(ev))
		}
	}
	for ev := range homeBad {
		a.WrongHome = append(a.WrongHome, string(ev))
	}
	for ev := range noSucc {
		a.NoSuccessor = append(a.NoSuccessor, string(ev))
	}
	a.NotCatchAll, a.Async, a.ShortTimeout = keys(notCatchAll), keys(async), keys(short)
	sort.Strings(a.WrongHome)
	sort.Strings(a.NoSuccessor)
	return a
}

// AuditSettings reads the settings file at path under Muse's rules and audits
// its OpenBox handlers against wantHome. An absent file is Present=false, not
// an error; a file Muse cannot read is an error, because every handler in it is
// dropped.
func AuditSettings(path, wantHome string) (SettingsAudit, error) {
	a := SettingsAudit{Path: path, Expected: ExpectedHandlers()}
	read := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		read = resolved
	}
	raw, err := os.ReadFile(read)
	if os.IsNotExist(err) {
		return a, nil
	}
	if err != nil {
		return a, fmt.Errorf("read %s: %w", path, err)
	}
	doc, err := parseSettings(raw)
	if err != nil {
		a.Present = true
		return a, err
	}
	a = auditDoc(doc, wantHome)
	a.Path, a.Present = path, true
	return a, nil
}

func keys(m map[HookName]bool) []string {
	var out []string
	for k := range m {
		out = append(out, string(k))
	}
	sort.Strings(out)
	return out
}
