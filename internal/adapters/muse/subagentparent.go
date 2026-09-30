package muse

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
)

// Muse runs a subagent (its skill-reminder and verify-reminder observers, and
// any the model spawns) under a session id of its own, and no hook payload
// names the parent. The parent's journal does: a record with
// parent_session_id and child_session_id (memory_reminder_child_session_linked
// on Muse 1.4.1), written before the child's first hook fires. Claude Code
// reports a subagent's activity inside the session that spawned it, tagged with
// agent_id and agent_type; folding a Muse child into its parent's session gives
// the same shape: one OpenBox session per Muse session, its subagents' calls
// among its own, under the parent's run and therefore the parent's halt latch.
//
// The link is resolved once per child and recorded, positive or negative, so
// every later hook of the child reads one small file and all of a child's
// events land in the same place: a child whose link could not be found stays a
// session of its own, as before, rather than splitting across two.

const (
	subagentLinkDir = "subagents"
	// subagentLinkWait bounds how long the first hook of an unknown session
	// polls for its link, for a parent journal written a moment behind the
	// child's first hook.
	subagentLinkWait = 300 * time.Millisecond
	subagentLinkPoll = 50 * time.Millisecond
	// parentLogMaxAge and maxParentLogs bound the scan: a parent is a session
	// that is running right now, so its journal was written moments ago.
	parentLogMaxAge = 10 * time.Minute
	maxParentLogs   = 16
	// maxLinkHops bounds following a parent that is itself a subagent.
	maxLinkHops = 4
)

// subagentLink is one child session's resolved parent. An empty
// ParentSessionID records that no parent was found.
type subagentLink struct {
	ChildSessionID  string `json:"child_session_id"`
	ParentSessionID string `json:"parent_session_id,omitempty"`
	AgentID         string `json:"agent_id,omitempty"`
	UpdatedAt       int64  `json:"updated_at"`
}

func (l lifecycle) linkPath(childID string) string {
	return filepath.Join(l.Dir, subagentLinkDir, filepath.Base(l.statePath(childID)))
}

func (l lifecycle) loadLink(childID string) (subagentLink, bool) {
	raw, err := os.ReadFile(l.linkPath(childID))
	if err != nil {
		return subagentLink{}, false
	}
	var link subagentLink
	if json.Unmarshal(raw, &link) != nil || link.ChildSessionID != childID {
		return subagentLink{}, false
	}
	return link, true
}

func (l lifecycle) saveLink(link subagentLink) {
	link.UpdatedAt = l.now().UnixNano()
	data, err := json.Marshal(link)
	if err == nil {
		err = os.MkdirAll(filepath.Join(l.Dir, subagentLinkDir), 0o700)
	}
	if err == nil {
		err = hookflow.AtomicWriteFile(l.linkPath(link.ChildSessionID), data, 0o600)
	}
	if err != nil {
		l.logf("subagent link: not recorded: %v", err)
	}
}

// foldSubagent moves a subagent's hook event into its parent's session: the
// event's SessionID becomes the parent's, SubagentSessionID keeps the child's,
// and SubagentID names the subagent when the payload did not. A SessionStart
// is never folded (a subagent announces itself with SubagentStart), and a
// session the lifecycle already knows is only folded when a link says so, so
// a main session never pays for a scan after its first event.
func (l lifecycle) foldSubagent(hook HookName, ev *HookEvent, logRoot string) {
	if hook == HookSessionStart || ev == nil || !safeSessionID(ev.SessionID) {
		return
	}
	child := ev.SessionID
	link, have := l.loadLink(child)
	if !have {
		if _, known := l.load(child); known && hook != HookSubagentStart {
			return
		}
		unlock := l.lock(child)
		link, have = l.loadLink(child)
		if !have {
			link = l.resolveLink(child, logRoot)
			if link.AgentID == "" {
				link.AgentID = capStr(ev.SubagentID)
			}
			l.saveLink(link)
		}
		unlock()
	}
	if link.ParentSessionID == "" {
		return
	}
	ev.SubagentSessionID = child
	ev.SessionID = link.ParentSessionID
	if ev.SubagentID == "" {
		ev.SubagentID = link.AgentID
	}
}

// resolveLink finds child's parent in the recent journals, polling briefly for
// one written just behind the child's first hook, and follows a parent that is
// itself a folded subagent to the session at the top.
func (l lifecycle) resolveLink(child, logRoot string) subagentLink {
	link := subagentLink{ChildSessionID: child}
	deadline := time.Now().Add(subagentLinkWait)
	for {
		if parent, agentID, ok := findParentLink(logRoot, child, l.now()); ok {
			link.ParentSessionID, link.AgentID = parent, agentID
			break
		}
		if time.Now().Add(subagentLinkPoll).After(deadline) {
			return link
		}
		time.Sleep(subagentLinkPoll)
	}
	for hop := 0; hop < maxLinkHops; hop++ {
		up, ok := l.loadLink(link.ParentSessionID)
		if !ok || up.ParentSessionID == "" || up.ParentSessionID == child {
			break
		}
		link.ParentSessionID = up.ParentSessionID
	}
	return link
}

// findParentLink scans the main journals written in the last parentLogMaxAge,
// newest first, for the record linking child to its parent. Only an id that can
// name a session directory is accepted as a parent.
func findParentLink(root, child string, now time.Time) (parent, agentID string, ok bool) {
	if root == "" {
		return "", "", false
	}
	type candidate struct {
		path string
		mod  time.Time
	}
	var logs []candidate
	for _, day := range dateDirs(root, now) {
		entries, err := os.ReadDir(day)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || e.Name() == child || !safeSessionID(e.Name()) {
				continue
			}
			p := filepath.Join(day, e.Name(), sessionLogName)
			info, err := os.Stat(p)
			if err != nil || !info.Mode().IsRegular() || now.Sub(info.ModTime()) > parentLogMaxAge {
				continue
			}
			logs = append(logs, candidate{p, info.ModTime()})
		}
	}
	sort.Slice(logs, func(a, b int) bool { return logs[a].mod.After(logs[b].mod) })
	if len(logs) > maxParentLogs {
		logs = logs[:maxParentLogs]
	}
	for _, c := range logs {
		if parent, agentID, ok := scanForLink(c.path, child); ok {
			return parent, agentID, true
		}
	}
	return "", "", false
}

// scanForLink reads one journal line by line. A line is decoded only when it
// names the child and a parent_session_id key, so a pass costs one byte search
// per line.
func scanForLink(path, child string) (parent, agentID string, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", false
	}
	defer f.Close()
	needle := []byte(child)
	key := []byte(`"parent_session_id"`)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, needle) || !bytes.Contains(line, key) {
			continue
		}
		var v any
		if json.Unmarshal(line, &v) != nil {
			continue
		}
		if parent, agentID, ok := linkIn(v, child); ok {
			return parent, agentID, true
		}
	}
	return "", "", false
}

// linkIn finds, anywhere in a decoded record, an object whose child_session_id
// is child and whose parent_session_id is another usable session id.
func linkIn(v any, child string) (parent, agentID string, ok bool) {
	switch t := v.(type) {
	case map[string]any:
		if c, _ := t["child_session_id"].(string); c == child {
			if p, _ := t["parent_session_id"].(string); p != child && safeSessionID(p) {
				for _, k := range []string{"reminder_agent_id", "subagent_id", "agent_id"} {
					if a, _ := t[k].(string); a != "" {
						agentID = capStr(a)
						break
					}
				}
				return p, agentID, true
			}
		}
		for _, x := range t {
			if parent, agentID, ok := linkIn(x, child); ok {
				return parent, agentID, true
			}
		}
	case []any:
		for _, x := range t {
			if parent, agentID, ok := linkIn(x, child); ok {
				return parent, agentID, true
			}
		}
	}
	return "", "", false
}
