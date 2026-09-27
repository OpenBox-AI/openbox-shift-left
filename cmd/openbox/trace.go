// Command openbox trace prints the local, full-fidelity process trace
// (internal/trace) for one session or run, or reconciles it against core's
// own agent logs (tracecore.go). It never mutates the trace: rotation and
// pruning stay the exclusive job of the daemons' own Sweep tickers.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// traceExitClean/traceExitFindings/traceExitError are the exit codes
// `--against-core` (tracecore.go) reports: 0 nothing to flag, 1 flagged
// something, 2 the reconciliation itself could not run (bad args, network,
// unreadable trace). The plain timeline/--list forms only ever use
// traceExitClean/exitError (usage mistakes), never traceExitFindings -- a
// timeline has nothing to "flag" on its own.
const (
	traceExitClean    = exitOK
	traceExitFindings = 1
	traceExitError    = 2
)

func (a *app) runTrace(args []string) int {
	fs := a.newFlagSet("openbox trace")
	var (
		dirFlag     string
		jsonOut     bool
		since       string
		bodies      bool
		list        bool
		againstCore bool
		agentID     string
		backendFlag string
		apiKeyFile  string
	)
	fs.StringVar(&dirFlag, "dir", "", "trace directory (default: $OPENBOX_TRACE_DIR, else <config dir>/trace)")
	fs.BoolVar(&jsonOut, "json", false, "print each record as one JSON line instead of a formatted line")
	fs.StringVar(&since, "since", "", "only records newer than this far back (e.g. 24h)")
	fs.BoolVar(&bodies, "bodies", false, "include each record's detail (bodies), elided by default")
	fs.BoolVar(&list, "list", false, "list sessions seen in the trace, instead of one session/run's timeline")
	fs.BoolVar(&againstCore, "against-core", false, "reconcile the local trace against core's agent logs")
	fs.StringVar(&agentID, "agent", "", "agent id to query core for (required with --against-core)")
	fs.StringVar(&backendFlag, "backend", "", "openbox-backend base URL (default: devconfig's resolver, else https://openbox-api.node.lat)")
	fs.StringVar(&apiKeyFile, "api-key-file", "", "file holding the core API key (else $OPENBOX_API_KEY)")
	fs.Usage = func() {
		fmt.Fprint(a.stderr, `usage:
  openbox trace <session-or-run-id> [--dir D] [--json] [--since 24h] [--bodies]
  openbox trace --list [--dir D] [--since 24h] [--json]
  openbox trace <session-or-run-id> --against-core --agent <id> [--dir D]
                [--backend URL] [--api-key-file F]
`)
	}
	if code, ok := parseFlags(fs, reorderFlagsFirst(fs, args)); !ok {
		return code
	}

	dir := dirFlag
	if dir == "" {
		dir = resolveTraceDir(a.getenv)
	}

	var sinceCut time.Time
	if since != "" {
		d, err := time.ParseDuration(since)
		if err != nil {
			return a.errorf("invalid --since %q: %v", since, err)
		}
		sinceCut = time.Now().Add(-d)
	}

	if list {
		return a.traceList(dir, sinceCut, jsonOut)
	}

	rest := fs.Args()
	if len(rest) < 1 {
		fs.Usage()
		return exitError
	}
	target := rest[0]

	if againstCore {
		if agentID == "" {
			return a.errorf("--against-core requires --agent <id>")
		}
		return a.traceAgainstCore(dir, target, agentID, backendFlag, apiKeyFile)
	}

	return a.traceTimeline(dir, target, sinceCut, jsonOut, bodies)
}

// reorderFlagsFirst moves every recognized flag (and, for a non-bool flag,
// the value token immediately after it) ahead of everything else, so
// `openbox trace <target> --dir D` parses the same as
// `openbox trace --dir D <target>`. Go's flag package stops scanning at the
// first non-flag token, which would otherwise make the positional
// session-or-run-id argument (documented first in the usage) swallow
// every flag that follows it into fs.Args() unparsed. A token this scan does
// not recognize as a defined flag is left as positional -- including a
// bare "--", which flag.Parse itself still treats as the args terminator.
func reorderFlagsFirst(fs *flag.FlagSet, args []string) []string {
	isBool := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) {
		if bv, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bv.IsBoolFlag() {
			isBool[f.Name] = true
		}
	})

	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name = name[:eq] // "--flag=value" is self-contained; nothing more to consume.
		} else if !isBool[name] && i+1 < len(args) {
			flags = append(flags, a, args[i+1])
			i++
			continue
		}
		flags = append(flags, a)
	}
	return append(flags, positional...)
}

// traceTimeline prints every record matching target (by session or run id),
// oldest first (trace.Read's own order).
func (a *app) traceTimeline(dir, target string, since time.Time, jsonOut, bodies bool) int {
	recs, skipped, err := trace.Read(dir, func(r trace.Record) bool {
		if r.SessionID != target && r.RunID != target {
			return false
		}
		if !since.IsZero() && r.TS.Before(since) {
			return false
		}
		return true
	})
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(a.stdout, "no trace directory at %s\n", dir)
			return exitOK
		}
		return a.errorf("reading trace: %v", err)
	}
	if skipped > 0 {
		fmt.Fprintf(a.stderr, "warning: skipped %d corrupt trace line(s)\n", skipped)
	}
	for _, r := range recs {
		printTraceRecord(a.stdout, r, jsonOut, bodies)
	}
	if len(recs) == 0 {
		fmt.Fprintf(a.stdout, "no trace records match %q under %s\n", target, dir)
	}
	return exitOK
}

// traceList summarizes every session_id seen in the trace: first/last
// timestamp and record count, sorted by session id.
func (a *app) traceList(dir string, since time.Time, jsonOut bool) int {
	recs, skipped, err := trace.Read(dir, func(r trace.Record) bool {
		if r.SessionID == "" {
			return false
		}
		if !since.IsZero() && r.TS.Before(since) {
			return false
		}
		return true
	})
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(a.stdout, "no trace directory at %s\n", dir)
			return exitOK
		}
		return a.errorf("reading trace: %v", err)
	}
	if skipped > 0 {
		fmt.Fprintf(a.stderr, "warning: skipped %d corrupt trace line(s)\n", skipped)
	}

	type summary struct {
		first, last time.Time
		count       int
	}
	byID := map[string]*summary{}
	for _, r := range recs {
		s, ok := byID[r.SessionID]
		if !ok {
			s = &summary{first: r.TS, last: r.TS}
			byID[r.SessionID] = s
		}
		if r.TS.Before(s.first) {
			s.first = r.TS
		}
		if r.TS.After(s.last) {
			s.last = r.TS
		}
		s.count++
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		s := byID[id]
		if jsonOut {
			b, _ := json.Marshal(struct {
				SessionID string    `json:"session_id"`
				First     time.Time `json:"first"`
				Last      time.Time `json:"last"`
				Count     int       `json:"count"`
			}{id, s.first, s.last, s.count})
			fmt.Fprintln(a.stdout, string(b))
			continue
		}
		fmt.Fprintf(a.stdout, "%s  first=%s  last=%s  count=%d\n",
			id, s.first.Format(time.RFC3339), s.last.Format(time.RFC3339), s.count)
	}
	if len(ids) == 0 {
		fmt.Fprintf(a.stdout, "no sessions found under %s\n", dir)
	}
	return exitOK
}

// printTraceRecord writes one record either as a single JSON line or as a
// compact formatted line; Detail (bodies) is elided from both forms unless
// bodies is set.
func printTraceRecord(w io.Writer, r trace.Record, jsonOut, bodies bool) {
	if !bodies {
		r.Detail = nil
	}
	if jsonOut {
		b, err := json.Marshal(r)
		if err != nil {
			fmt.Fprintf(w, "{\"marshal_error\":%q}\n", err.Error())
			return
		}
		fmt.Fprintln(w, string(b))
		return
	}

	var line strings.Builder
	fmt.Fprintf(&line, "%s %-7d %-16s %-20s %-9s",
		r.TS.Format(time.RFC3339Nano), r.PID, r.Proc, r.Stage, orDashTrace(r.Outcome))
	for _, id := range []struct {
		label, value string
	}{
		{"session", r.SessionID},
		{"run", r.RunID},
		{"activity", r.ActivityID},
		{"event", r.EventID},
	} {
		if id.value != "" {
			fmt.Fprintf(&line, " %s=%s", id.label, id.value)
		}
	}
	if r.ErrClass != "" || r.Err != "" {
		fmt.Fprintf(&line, " err=%s:%s", r.ErrClass, r.Err)
	}
	if r.DurMS > 0 {
		fmt.Fprintf(&line, " dur_ms=%.1f", r.DurMS)
	}
	if r.Attempt > 0 {
		fmt.Fprintf(&line, " attempt=%d", r.Attempt)
	}
	if bodies && len(r.Detail) > 0 {
		b, _ := json.Marshal(r.Detail)
		fmt.Fprintf(&line, " detail=%s", string(b))
	}
	fmt.Fprintln(w, line.String())
}

func orDashTrace(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
