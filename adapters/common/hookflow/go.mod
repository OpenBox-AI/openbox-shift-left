// Package hookflow is the provider-agnostic engine behind a developer-runtime
// hook run: the durable spool, the cross-process duration stash, and the
// advisory sink.
//
// It exists because the Claude Code and Codex adapters carried byte-identical
// copies of all of it. That duplication was not a style problem — this is the
// path a governance event survives on, and a fix landing in one copy and not
// the other is itself a governance gap.
module github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow

go 1.23

require github.com/openbox-ai/openbox-shift-left/client v0.0.0

replace github.com/openbox-ai/openbox-shift-left/client => ../../../client

require (
	github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig v0.0.0
	github.com/openbox-ai/openbox-shift-left/decision v0.0.0
	github.com/openbox-ai/openbox-shift-left/provider v0.0.0
)

replace github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig => ../devconfig

replace github.com/openbox-ai/openbox-shift-left/decision => ../../../decision

replace github.com/openbox-ai/openbox-shift-left/provider => ../../../provider
