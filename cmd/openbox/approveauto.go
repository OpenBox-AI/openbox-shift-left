package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/approver"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/backend"
)

type autoFlags struct {
	host        string
	envelope    string
	decide      bool
	once        bool
	interval    time.Duration
	hostTimeout time.Duration
	maxPerHour  int
	allowSelf   bool
}

const defaultAutoInterval = time.Second

func (a *app) runApproveAuto(cl *backend.Client, orgID string, f autoFlags) int {
	cfgPath := devconfig.DefaultApproverConfigPath()
	installed, _ := devconfig.LoadApprover(cfgPath)

	hostName := firstNonEmpty(f.host, installed.Host)
	envelopePath := firstNonEmpty(f.envelope, installed.Envelope)
	interval := f.interval
	if interval <= 0 {
		interval = msOrDefault(installed.PollIntervalMS, defaultAutoInterval)
	}
	hostTimeout := f.hostTimeout
	if hostTimeout <= 0 {
		hostTimeout = msOrDefault(installed.HostTimeoutMS, 8*time.Second)
	}
	maxPerHour := f.maxPerHour
	if maxPerHour < 0 {
		maxPerHour = installed.MaxAutoPerHour
	}
	installedMayDecide := installed.OrgID != "" && !installed.Shadow
	shadow := !(f.decide || installedMayDecide)

	env, err := approver.LoadEnvelope(envelopePath)
	if err != nil {
		return a.errorf("%v", err)
	}

	var host approver.Host
	switch hostName {
	case "":
		fmt.Fprintln(a.stdout, "note: no --host configured — consultable requests will be left for a human")
	case "claude-code":
		scratch, err := os.MkdirTemp("", "openbox-approver-")
		if err != nil {
			return a.errorf("host scratch dir: %v", err)
		}
		defer os.RemoveAll(scratch)
		host = approver.ClaudeCodeHost{Model: a.env("OPENBOX_APPROVER_MODEL", "sonnet"), Timeout: hostTimeout, Dir: scratch}
	default:
		return a.errorf("unknown --host %q (supported: claude-code)", hostName)
	}

	evidence := a.env("OPENBOX_APPROVER_EVIDENCE", filepath.Join(filepath.Dir(cfgPath), "approvals-auto.jsonl"))
	selfAgent := devconfig.ResolveAgentID() // this machine's developer agent, if any

	fmt.Fprintf(a.stdout, "Autonomous approver — %s\n", modeLabel(shadow))
	fmt.Fprintf(a.stdout, "  queue     %s\n  envelope  %s\n  host      %s\n  evidence  %s\n",
		orgID, envelopePath, orNone(hostName), evidence)
	if selfAgent != "" && !f.allowSelf {
		fmt.Fprintf(a.stdout, "  refusing requests from this machine's own agent (%s) — pass --allow-same-agent to override\n", selfAgent)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := approver.Loop(ctx, cl, approver.Config{
		OrgID:          orgID,
		Envelope:       env,
		Host:           host,
		Shadow:         shadow,
		Interval:       interval,
		Once:           f.once,
		SelfAgentID:    selfAgent,
		AllowSelfAgent: f.allowSelf,
		MaxPerHour:     maxPerHour,
		EvidencePath:   evidence,
		Log:            a.stdout,
	}); err != nil {
		return a.errorf("%v", err)
	}
	return exitOK
}

func modeLabel(shadow bool) string {
	if shadow {
		return "SHADOW: recording what it would decide, deciding nothing"
	}
	return "DECIDING"
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func msOrDefault(ms int, d time.Duration) time.Duration {
	if ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return d
}
