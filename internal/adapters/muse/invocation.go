package muse

import (
	"fmt"
	"path/filepath"
	"strings"
)

// invocation is one parsed `<engine> hook muse ...` command line, the only shape
// that marks a handler as OpenBox's.
type invocation struct {
	Engine string
	// Home is the --home directory baked into the command, "" when none is.
	Home string
	// FailClosed is the deny-only successor form.
	FailClosed bool
	Event      HookName
}

// splitCommand splits a command line on whitespace outside double quotes. There
// are no escapes: the installer never writes a quote into a quoted value, and a
// line it does not recognise is simply not ours.
func splitCommand(s string) ([]string, bool) {
	var (
		args    []string
		cur     strings.Builder
		inQuote bool
		started bool
	)
	for _, r := range s {
		switch {
		case r == '"':
			inQuote, started = !inQuote, true
		case !inQuote && (r == ' ' || r == '\t'):
			if started {
				args = append(args, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	if inQuote {
		return nil, false
	}
	if started {
		args = append(args, cur.String())
	}
	return args, true
}

// parseInvocation reports whether command runs this adapter's hook handler:
// `<engine> hook muse [--home <dir>] [--fail-closed] <Event>`, nothing else.
// Ownership is parsed rather than scanned, so a foreign hook that merely
// mentions `hook muse` (`my-audit && openbox hook muse PreToolUse`) is not ours
// and survives both a re-install and an uninstall.
func parseInvocation(command string) (invocation, bool) {
	args, ok := splitCommand(strings.TrimSpace(command))
	if !ok || len(args) < 4 || args[1] != "hook" || args[2] != "muse" {
		return invocation{}, false
	}
	inv := invocation{Engine: args[0]}
	rest := args[3:]
	if len(rest) >= 2 && rest[0] == "--home" {
		inv.Home, rest = rest[1], rest[2:]
	}
	if len(rest) >= 1 && rest[0] == "--fail-closed" {
		inv.FailClosed, rest = true, rest[1:]
	}
	if len(rest) != 1 {
		return invocation{}, false
	}
	ev, err := ParseHookName(rest[0])
	if err != nil {
		return invocation{}, false
	}
	inv.Event = ev
	return inv, true
}

// formatInvocation is parseInvocation's inverse. The engine and home are
// quoted, since either may hold a space.
func formatInvocation(engine, home string, failClosed bool, ev HookName) (string, error) {
	if !filepath.IsAbs(engine) {
		return "", fmt.Errorf("engine path %q is not absolute", engine)
	}
	if strings.ContainsAny(engine, "\"\n\r") {
		return "", fmt.Errorf("engine path %q holds a character a hook command cannot carry", engine)
	}
	var b strings.Builder
	b.WriteString(`"` + engine + `" hook muse`)
	if home != "" {
		if !filepath.IsAbs(home) || strings.ContainsAny(home, "\"\n\r") {
			return "", fmt.Errorf("OpenBox home %q is not usable in a hook command; it must be an absolute path without quotes or line breaks", home)
		}
		b.WriteString(` --home "` + home + `"`)
	}
	if failClosed {
		b.WriteString(" --fail-closed")
	}
	b.WriteString(" " + string(ev))
	return b.String(), nil
}
