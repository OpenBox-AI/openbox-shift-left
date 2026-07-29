package decision

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// DefaultBundlePath is where the in-process decider loads its local policy bundle
// when no path is configured — the same file `openbox dev sync` writes. Per-user
// under XDG on Linux / the standard config dir elsewhere.
func DefaultBundlePath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return dir + "/openbox/policy-bundle.json"
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home + "/.config/openbox/policy-bundle.json"
	}
	return "policy-bundle.json"
}

// Bundle is the local policy the decision engine evaluates a tool call
// against, in the single-digit-ms band, with no network I/O (INV-3b). It's
// the local representation of the governance policy core would evaluate on
// an external OPA server.
//
// Why a local rule bundle and not embedded OPA: openbox-core doesn't embed
// OPA and distributes no bundle today — its rego lives in Postgres and is
// loaded into an external OPA server by an out-of-repo management plane. So
// there's no real bundle to pull and no test oracle to validate an
// embedded-OPA evaluator against. This package ships:
//   - the Evaluator seam (evaluator.go) so a real embedded-OPA evaluator
//     that loads core's rego and queries data.org.<org>.policy_<id> drops in
//     unchanged;
//   - BuildOPAInput (input.go) — the core-faithful rego input the OPA
//     evaluator will consume;
//   - this local rule Bundle as the default evaluator, whose decision
//     contract is faithful to core: rule decision strings map to verdicts
//     via the same mapping core uses and aggregate by the same max-severity
//     priority (HighestPriorityVerdict).
//
// [EXT-opa-bundle] — the real OPA-bundle distribution (a core endpoint that
// serves the compiled per-agent rego, or a management-plane bundle) is the
// external follow-up that lets Sync pull a live policy. Until it exists the
// decider loads a local bundle file and the decision is honestly
// local-policy.
type Bundle struct {
	// Version identifies the bundle content for staleness/telemetry (an
	// etag, content hash, or monotonic revision). Opaque.
	Version string `json:"version"`

	// PolicyID and UpdatedAt are the pin (ADR-0005): the backend
	// PolicyEntity.id + its updated_at, written by `openbox dev sync`. They
	// are opaque staleness coordinates — the session-start check compares
	// the backend's current (id, updated_at) to this pin and warns /
	// marks-stale on a mismatch. PolicyID also stamps the resolved
	// Evaluation for audit parity with core. Neither is a secret (INV-1).
	PolicyID  string `json:"policy_id,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`

	// PolicyBuilder, when set, is the parsed backend config.policy_builder —
	// a builder-authored policy `dev sync` fetched and translated.
	// SetBundle selects the first-match builderEvaluator for it (not the
	// max-severity bundleEvaluator, which is for the legacy hand-authored
	// Rules format only — reusing Rules would change precedence). Mutually
	// exclusive with Rules in practice.
	PolicyBuilder *PolicyBuilderConfig `json:"policy_builder,omitempty"`

	// RawRegoUnlocalized flags a backend policy authored as hand-written
	// raw rego with no config.policy_builder: it cannot be evaluated
	// locally without a rego engine, so `dev sync` writes a bundle with
	// this flag set and no rules/builder. The daemon then serves a real
	// local ALLOW (honest under-blocking, never over-blocking; the residual
	// is surfaced by a non-secret `dev sync` warning). It's informational/
	// telemetry only.
	RawRegoUnlocalized bool `json:"raw_rego_unlocalized,omitempty"`

	// Signed, when present, is the backend's signature over the authoritative
	// policy (E8-S6 / ADR-0008). It is optional: an unsigned bundle loads
	// exactly as before, so a backend that does not sign yet is unaffected.
	//
	// When it IS present, the fields above become a debugging view — the policy
	// actually evaluated is re-derived from the signed bytes by VerifyIntegrity,
	// so editing PolicyBuilder or Rules in this file cannot change a decision.
	Signed *SignedPolicy `json:"signed,omitempty"`

	// DefaultDecision is returned when no rule matches. It must be an
	// allow-class decision for a fail-open posture: an empty or
	// "allow"/"continue" value yields ALLOW. A bundle that set this to a
	// blocking decision would make the decider deny-by-default — rejected
	// at load time (see validate) unless the org has explicitly opted into
	// that posture (a fail-closed policy, not a bundle default).
	DefaultDecision string `json:"default_decision,omitempty"`

	// Rules are all evaluated; the highest-severity match wins (max-severity
	// aggregation, mirroring core's HighestPriorityVerdict — see
	// evaluator.go). Rule order does not determine precedence (it only
	// affects same-severity ties, where the later rule's metadata wins) —
	// a BLOCK rule wins over a CONSTRAIN rule regardless of which appears
	// first, so overlapping rules can never under-block. Each rule matches
	// on tool + attributes and yields a decision.
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
	// developer (fed back to Claude on a deny). Never content (INV-2).
	Reason string `json:"reason,omitempty"`

	// PolicyID identifies the policy for audit/telemetry parity with core.
	PolicyID string `json:"policy_id,omitempty"`

	// Constraints mirror core's constraints list (applied on CONSTRAIN).
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

// LoadBundleFile reads and validates a bundle from a local JSON file. A
// malformed or deny-by-default bundle is rejected (returns an error) so the
// daemon never silently serves a policy that would block-by-default — the
// caller keeps the previous good bundle (or, at cold start, serves fail-open
// with no bundle).
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
// blocking decision is rejected: deny-by-default is a fail-closed policy
// posture, never a bundle-content default. Every rule must carry a
// recognized decision string.
func (b *Bundle) validate() error {
	if v := decisionToVerdict(b.DefaultDecision); v == client.VerdictBlock || v == client.VerdictHalt {
		return fmt.Errorf("bundle default_decision %q would block by default; "+
			"a fail-closed default is a policy posture, not a bundle default (fail-open)", b.DefaultDecision)
	}
	for i, r := range b.Rules {
		if strings.TrimSpace(r.Decision) == "" {
			return fmt.Errorf("bundle rule[%d] (%s): empty decision", i, r.ID)
		}
		if _, ok := recognizedDecisions[strings.ToLower(strings.TrimSpace(r.Decision))]; !ok {
			return fmt.Errorf("bundle rule[%d] (%s): unrecognized decision %q", i, r.ID, r.Decision)
		}
	}
	// Validate PolicyBuilder rule decisions too: decisionToVerdict maps an
	// unrecognized string to ALLOW (fail-open), so a typo'd decision like
	// "BLOKC" would silently drop a BLOCK. Reject it at load/translate time,
	// exactly as the Rules[] path does, so a malformed builder policy keeps
	// the last-good bundle rather than under-blocking.
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
	"constrain":        {},
	"require_approval": {}, "require-approval": {},
	"block": {},
	"stop":  {}, "halt": {},
}

// decisionToVerdict maps a core-style decision string to the canonical
// client.Verdict, mirroring openbox-core's OPAClient.Evaluate switch and
// client's wireToVerdict/legacyAction maps:
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
		// Unknown decision string is treated as ALLOW, never deny — a
		// garbled policy must not block the dev loop. validate() already
		// rejects unknown decisions at load, so this is defense-in-depth.
		return client.VerdictAllow
	}
}
