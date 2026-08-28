module github.com/openbox-ai/openbox-shift-left/actions/openbox-git-action

go 1.27.0

require (
	github.com/openbox-ai/openbox-shift-left/adapters/common/git v0.0.0
	github.com/openbox-ai/openbox-shift-left/client v0.0.0
)

replace github.com/openbox-ai/openbox-shift-left/client => ../../client

replace github.com/openbox-ai/openbox-shift-left/adapters/common/git => ../../adapters/common/git
