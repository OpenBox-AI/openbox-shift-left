package client

import "encoding/json"

// Verdict is the canonical governance verdict, priority-ordered
// HALT > BLOCK > REQUIRE_APPROVAL > CONSTRAIN > ALLOW (contract $defs.verdict).
//
// openbox-core serializes the response `verdict` field as LOWERCASE wire
// strings and also emits a legacy `action` field (MAPPING.md §4, verified vs
// core governance.go). Phase-1 observe (D7/INV-3) treats every verdict as
// allow and never blocks the caller — callers parse it only for finops/audit
// and Phase-2 readiness.
type Verdict string

const (
	VerdictAllow           Verdict = "ALLOW"
	VerdictConstrain       Verdict = "CONSTRAIN"
	VerdictRequireApproval Verdict = "REQUIRE_APPROVAL"
	VerdictBlock           Verdict = "BLOCK"
	VerdictHalt            Verdict = "HALT"
	// VerdictUnknown is returned when the response carries no recognized verdict
	// (e.g. a fail-open drop, or an unmapped wire string). Never treated as deny.
	VerdictUnknown Verdict = ""
)

// wireToVerdict maps core's lowercase wire strings to the canonical enum
// (MAPPING.md §4, x-wire-mapping).
var wireToVerdict = map[string]Verdict{
	"allow":            VerdictAllow,
	"constrain":        VerdictConstrain,
	"require_approval": VerdictRequireApproval,
	"block":            VerdictBlock,
	"halt":             VerdictHalt,
}

// legacyActionToVerdict maps core's legacy `action` field to the canonical enum
// (MAPPING.md §4, x-legacy-action) — a fallback when `verdict` is absent.
var legacyActionToVerdict = map[string]Verdict{
	"continue":         VerdictAllow,
	"require-approval": VerdictRequireApproval,
	"stop":             VerdictBlock,
}

// verdictResponse is the subset of core's GovernanceVerdictResponse this client
// needs to resolve a canonical verdict. Extra fields are ignored (forward
// compatibility): core may add attestation/policy detail we don't consume in
// Phase-1 observe.
type verdictResponse struct {
	Verdict string `json:"verdict"`
	Action  string `json:"action"`
}

// parseVerdict resolves a canonical Verdict from a raw /evaluate response body.
// It prefers the lowercase `verdict` field and falls back to the legacy
// `action` field; anything unrecognized yields VerdictUnknown (never deny).
func parseVerdict(body []byte) Verdict {
	var r verdictResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return VerdictUnknown
	}
	if v, ok := wireToVerdict[r.Verdict]; ok {
		return v
	}
	if v, ok := legacyActionToVerdict[r.Action]; ok {
		return v
	}
	return VerdictUnknown
}
