package main

import (
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// These moved here with their subjects when `auth` stopped owning agent
// credentials. They are not `auth` tests any more; they guard the validation
// `init`'s adopt prompt runs on a hand-typed API key.

// TestOrgKeyInTheAgentKeyFieldIsRejected the org/agent key mix-up cost hours of
// debugging once, and the split made it easier to hit rather than harder: the
// two credentials are now typed at two different prompts in two different
// commands, so pasting one into the other's field has to be rejected by name.
func TestOrgKeyInTheAgentKeyFieldIsRejected(t *testing.T) {
	body := strings.Repeat("f", 48)
	problem := apiKeyProblem("obx_key_" + body)
	if problem == "" {
		t.Fatal("an obx_key_ org key in the agent-key field must be rejected")
	}
	for _, want := range []string{"ORGANIZATION key", devconfig.EnvControlToken, "obx_"} {
		if !strings.Contains(problem, want) {
			t.Errorf("rejection should mention %q:\n%s", want, problem)
		}
	}
	if strings.Contains(problem, body) {
		t.Errorf("rejection echoed the credential body:\n%s", problem)
	}
}

// TestBlankAPIKeyIsRejected the adopt flow's confirmation is "no" by default;
// an empty answer must still be caught rather than written as an empty
// credential the next run reads as "configured".
func TestBlankAPIKeyIsRejected(t *testing.T) {
	problem := apiKeyProblem("")
	if problem == "" {
		t.Fatal("a blank API key should have been rejected")
	}
	if !strings.Contains(problem, "no API key") {
		t.Errorf("rejection does not say what is missing: %s", problem)
	}
}

// TestValidAPIKeyIsAccepted the ordinary obx_ runtime key shape.
func TestValidAPIKeyIsAccepted(t *testing.T) {
	if problem := apiKeyProblem("obx_" + strings.Repeat("a", 48)); problem != "" {
		t.Errorf("a valid runtime key was rejected: %s", problem)
	}
}

// TestAgentKeyInTheControlTokenFieldIsRejected the inverse, and the one that
// is live now: `auth` asks for the org token, and the value most people have
// nearest to hand is the agent runtime key it mints.
func TestAgentKeyInTheControlTokenFieldIsRejected(t *testing.T) {
	body := strings.Repeat("a", 48)
	problem := controlTokenProblem("obx_" + body)
	if problem == "" {
		t.Fatal("an agent runtime key pasted as the control token must be rejected")
	}
	if !strings.Contains(problem, "AGENT RUNTIME key") {
		t.Errorf("rejection should say which kind of key it got:\n%s", problem)
	}
	if strings.Contains(problem, body) {
		t.Errorf("rejection echoed the credential body:\n%s", problem)
	}
}

// TestMaskTokenShowsEnoughToRecognizeAndNotEnoughToUse whatever displays a
// pasted key back to the person who pasted it has to be recognizable to them
// and useless to anyone reading over their shoulder or their scrollback.
func TestMaskTokenShowsEnoughToRecognizeAndNotEnoughToUse(t *testing.T) {
	// Assembled rather than written out: a credential-shaped literal in this
	// repo is rewritten on disk by the local secret hook, which would leave
	// this case asserting against a placeholder.
	key := strings.Join([]string{"obx", "live", "SENSITIVEBODY", "a91f"}, "_")
	got := maskToken(key)
	if strings.Contains(got, "SENSITIVEBODY") {
		t.Errorf("mask printed the body: %q", got)
	}
	if !strings.Contains(got, "a91f") || !strings.Contains(got, "chars)") {
		t.Errorf("mask = %q, want a recognizable tail and a length", got)
	}
	// A short value has no safe prefix to show at all, so it shows none.
	short := strings.Join([]string{"obx", "short"}, "_")
	if got := maskToken(short); strings.Contains(got, short) {
		t.Errorf("a short value was echoed whole: %q", got)
	}
	if got := maskToken(""); got != "(none)" {
		t.Errorf("empty = %q, want (none)", got)
	}
}
