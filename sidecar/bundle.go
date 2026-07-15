package sidecar

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// Bundle is the LOCAL policy the sidecar evaluates a tool call against, in the
// single-digit-ms band, with NO network I/O (INV-3b). It is the local
// representation of the governance policy core would evaluate on an external OPA
// server.
//
// Why a local rule bundle and not embedded OPA (design decision, E6-S5,
// cross-repo recon 2026-07-13): openbox-core does NOT embed OPA and distributes
// NO bundle today — its rego lives in Postgres (RegoCode) and is loaded into an
// external OPA server by an out-of-repo management plane. So there is no real
// bundle to pull and no test oracle to validate an embedded-OPA evaluator
// against. Per the E6-S5 stop-condition (mirroring SL-15's "build the seam, flag
// the external dependency"), Phase-1 ships:
//   - the Evaluator SEAM (evaluator.go) so a real embedded-OPA evaluator that
//     loads core's rego and queries data.org.<org>.policy_<id> drops in unchanged;
//   - BuildOPAInput (input.go) — the core-faithful rego input the OPA evaluator
//     will consume;
//   - this local rule Bundle as the default evaluator, whose DECISION CONTRACT is
//     faithful to core: rule decision strings map to verdicts via the SAME
//     mapping core uses (opa.go:257-270) and aggregate by the SAME max-severity
//     priority (governance.go HighestPriorityVerdict).
//
// [EXT-opa-bundle] — the real OPA-bundle distribution (a core endpoint that
// serves the compiled per-agent rego, or a management-plane bundle) is the
// external follow-up that lets Sync pull a live policy. Until it exists the
// sidecar loads a local bundle file and the decision is honestly local-policy.
type Bundle struct {
	// Version identifies the bundle content for staleness/telemetry (an etag,
	// content hash, or monotonic revision). Opaque.
	Version string `json:"version"`

	// PolicyID and UpdatedAt are the PIN (STORY-E6-S8, ADR-0005 §Decision-3): the
	// backend PolicyEntity.id + its updated_at, written by `openbox dev sync`. They
	// are OPAQUE staleness coordinates — the session-start check compares the
	// backend's current (id, updated_at) to this pin and warns / marks-stale on a
	// mismatch. PolicyID also stamps the resolved Evaluation for audit parity with
	// core. Neither is a secret (INV-1).
	PolicyID  string `json:"policy_id,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`

	// PolicyBuilder, when set, is the parsed backend config.policy_builder — a
	// builder-authored policy `dev sync` fetched and translated. SetBundle selects
	// the FIRST-MATCH builderEvaluator for it (NOT the max-severity bundleEvaluator,
	// which is for the legacy hand-authored Rules format only — reusing Rules would
	// change precedence, story §3). Mutually exclusive with Rules in practice.
	PolicyBuilder *PolicyBuilderConfig `json:"policy_builder,omitempty"`

	// RawRegoUnlocalized flags a backend policy authored as hand-written raw rego
	// with NO config.policy_builder: it cannot be evaluated locally without a rego
	// engine (ADR-0005 §Decision-2), so `dev sync` writes a bundle with this flag
	// set and NO rules/builder. The daemon then serves a REAL local ALLOW (honest
	// under-blocking, never over-blocking — OD9; the residual is surfaced by a
	// non-secret `dev sync` warning). It is informational/telemetry only.
	RawRegoUnlocalized bool `json:"raw_rego_unlocalized,omitempty"`

	// DefaultDecision is returned when no rule matches. It MUST be an allow-class
	// decision for a fail-open posture (OD9): an empty or "allow"/"continue" value
	// yields ALLOW. A bundle that set this to a blocking decision would make the
	// sidecar deny-by-default — rejected at load time (see validate) unless the
	// org has explicitly opted into that posture (a fail-closed *policy* is E6-S3,
	// not a bundle default).
	DefaultDecision string `json:"default_decision,omitempty"`

	// Rules are ALL evaluated; the HIGHEST-SEVERITY match wins (max-severity
	// aggregation, mirroring core's HighestPriorityVerdict — see evaluator.go).
	// Rule ORDER does NOT determine precedence (it only affects same-severity
	// ties, where the later rule's metadata wins) — a BLOCK rule wins over a
	// CONSTRAIN rule regardless of which appears first, so overlapping rules can
	// never under-block. Each rule matches on tool + attributes and yields a
	// decision.
	Rules []Rule `json:"rules"`
}

// Rule is one local policy rule. A rule matches when every set Match field
// matches the request; an unset Match field is a wildcard.
type Rule struct {
	ID string `json:"id,omitempty"`

	Match RuleMatch `json:"match"`

	// Decision is the core-style decision string this rule yields when it matches:
	// one of continue|allow|constrain|require_approval|require-approval|block|
	// stop|halt (case-insensitive). Mapped to a client.Verdict by decisionToVerdict.
	Decision string `json:"decision"`

	// Reason is a non-content, human-readable justification surfaced to the
	// developer (E6-S2 feeds it back to Claude on a deny). Never content (INV-2).
	Reason string `json:"reason,omitempty"`

	// PolicyID identifies the policy for audit/telemetry parity with core.
	PolicyID string `json:"policy_id,omitempty"`

	// Constraints mirror core's constraints list (applied by E6-S2 on CONSTRAIN).
	Constraints []map[string]any `json:"constraints,omitempty"`
}

// RuleMatch is the (all-of) match predicate. Every set field must match; unset
// fields are wildcards. Matching is metadata-only (INV-2).
type RuleMatch struct {
	EventType string `json:"event_type,omitempty"` // e.g. "ToolCall"
	ToolName  string `json:"tool_name,omitempty"`  // exact tool name, e.g. "Bash"
	ToolKind  string `json:"tool_kind,omitempty"`  // shell|file|mcp
	MCPServer string `json:"mcp_server,omitempty"`

	// AttributeContains matches when the named attribute's string value CONTAINS
	// the given substring (case-sensitive) — the common "command contains rm -rf"
	// / "file_path under /etc" shape. Metadata only.
	AttributeContains map[string]string `json:"attribute_contains,omitempty"`

	// AttributeEquals matches when the named attribute equals the value exactly.
	AttributeEquals map[string]string `json:"attribute_equals,omitempty"`
}

// LoadBundleFile reads and validates a bundle from a local JSON file. A malformed
// or deny-by-default bundle is REJECTED (returns an error) so the daemon never
// silently serves a policy that would block-by-default — the caller keeps the
// previous good bundle (or, at cold start, serves fail-open with no bundle).
func LoadBundleFile(path string) (*Bundle, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read bundle %s: %w", path, err)
	}
	return ParseBundle(raw)
}

// ParseBundle decodes + validates bundle bytes.
func ParseBundle(raw []byte) (*Bundle, error) {
	var b Bundle
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return nil, fmt.Errorf("parse bundle: %w", err)
	}
	if err := b.validate(); err != nil {
		return nil, err
	}
	return &b, nil
}

// validate enforces the fail-open-safe defaults. A bundle whose default is a
// blocking decision is rejected: deny-by-default is a fail-CLOSED policy posture
// (E6-S3), never a bundle-content default (OD9). Every rule must carry a
// recognized decision string.
func (b *Bundle) validate() error {
	if v := decisionToVerdict(b.DefaultDecision); v == client.VerdictBlock || v == client.VerdictHalt {
		return fmt.Errorf("bundle default_decision %q would block by default; "+
			"a fail-closed default is a policy posture (E6-S3), not a bundle default (OD9 fail-open)", b.DefaultDecision)
	}
	for i, r := range b.Rules {
		if strings.TrimSpace(r.Decision) == "" {
			return fmt.Errorf("bundle rule[%d] (%s): empty decision", i, r.ID)
		}
		if _, ok := recognizedDecisions[strings.ToLower(strings.TrimSpace(r.Decision))]; !ok {
			return fmt.Errorf("bundle rule[%d] (%s): unrecognized decision %q", i, r.ID, r.Decision)
		}
	}
	// Validate PolicyBuilder rule decisions too (G_SEC-INFO-3): decisionToVerdict
	// maps an unrecognized string to ALLOW (fail-open), so a typo'd decision like
	// "BLOKC" would SILENTLY DROP a BLOCK. Reject it at load/translate time, exactly
	// as the Rules[] path does, so a malformed builder policy keeps the last-good
	// bundle rather than under-blocking (the FileBundleSource/dev-sync callers both
	// go through validate).
	if b.PolicyBuilder != nil {
		for i, r := range b.PolicyBuilder.Rules {
			if strings.TrimSpace(r.Decision) == "" {
				return fmt.Errorf("policy_builder rule[%d] (%s): empty decision", i, r.ID)
			}
			if _, ok := recognizedDecisions[strings.ToLower(strings.TrimSpace(r.Decision))]; !ok {
				return fmt.Errorf("policy_builder rule[%d] (%s): unrecognized decision %q "+
					"(expected ALLOW/REQUIRE_APPROVAL/BLOCK/HALT)", i, r.ID, r.Decision)
			}
		}
	}
	return nil
}

// recognizedDecisions is the accepted set of core-style decision strings.
var recognizedDecisions = map[string]struct{}{
	"continue": {}, "allow": {},
	"constrain": {},
	"require_approval": {}, "require-approval": {},
	"block": {},
	"stop":  {}, "halt": {},
}

// decisionToVerdict maps a core-style decision string to the canonical
// client.Verdict, mirroring openbox-core's OPAClient.Evaluate switch
// (opa.go:257-270) and client's wireToVerdict/legacyAction maps:
//
//	continue|allow            -> ALLOW
//	constrain                 -> CONSTRAIN
//	require_approval|require-approval -> REQUIRE_APPROVAL
//	block                     -> BLOCK
//	stop|halt                 -> HALT
//	anything unrecognized/""  -> ALLOW (fail-open default; never deny on garbage)
func decisionToVerdict(decision string) client.Verdict {
	switch strings.ToLower(strings.TrimSpace(decision)) {
	case "continue", "allow", "":
		return client.VerdictAllow
	case "constrain":
		return client.VerdictConstrain
	case "require_approval", "require-approval":
		return client.VerdictRequireApproval
	case "block":
		return client.VerdictBlock
	case "stop", "halt":
		return client.VerdictHalt
	default:
		// Unknown decision string is treated as ALLOW, never deny — a garbled
		// policy must not block the dev loop (OD9). validate() already rejects
		// unknown decisions at load, so this is defense-in-depth.
		return client.VerdictAllow
	}
}
