module github.com/openbox-ai/openbox-shift-left/client

go 1.27.0

require github.com/openbox-ai/openbox-shift-left/contracts/dev-event/conformance v0.0.0

require (
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3 // indirect
	golang.org/x/text v0.14.0 // indirect
)

replace github.com/openbox-ai/openbox-shift-left/contracts/dev-event/conformance => ../contracts/dev-event/conformance
