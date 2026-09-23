// Command didcheck prints git-action's own DID derivation for one agent id.
// It exists only so attribution_test.go can cross-check DIDForAgent against
// AttributionDIDFor out of process: git-action already imports devconfig
// (advisory.go), so a direct import the other way would be a cycle. Living
// under testdata/, it is excluded from `go build ./...`/`go vet ./...` by the
// Go toolchain's own convention and only ever runs via `go run` from the test.
package main

import (
	"fmt"
	"os"

	gitaction "github.com/openbox-ai/openbox-shift-left/internal/actions/openbox-git-action"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: didcheck <agent-id>")
		os.Exit(2)
	}
	did, err := gitaction.DIDForAgent(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(did)
}
