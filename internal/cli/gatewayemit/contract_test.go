package gatewayemit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
)

// schemaRelPath is the dev-event contract, relative to this source file.
//
// Read with stdlib rather than through the conformance package on purpose. That
// package is not a dependency of this module, and adding it would put a
// jsonschema tree into cli's go.mod for one test — a real dependency edge, and
// this repo's rule is that adding one is a decision. Reading two declared values
// out of the document needs neither.
const schemaRelPath = "../../../api/dev-event.schema.json"

// The contract declares a bound on gateway_request_id (maxLength + a printable-
// ASCII pattern); this package enforces the same rule imperatively in
// usableRequestID, with its own constant. Two statements of one rule, in two
// languages, in two modules — so this asserts they agree, and that the values
// THIS producer actually mints satisfy both.
//
// It exists because they could otherwise drift silently in either direction.
// Widen maxRequestIDLen alone and the producer starts emitting ids its own
// contract rejects; loosen the schema alone and the contract starts accepting a
// shape no producer emits, while the comments claiming they agree quietly become
// false. Nothing validates events at runtime — the conformance validator has no
// non-test caller anywhere — so a divergence surfaces at ingest or not at all.
//
// The equivalence is narrower than it looks: JSON Schema's maxLength counts CODE
// POINTS and printableASCII counts BYTES. They agree only because the pattern
// admits no rune wider than one byte. A field taking one without the other does
// not inherit that.
func TestGatewayIDBoundMatchesTheContract(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve this test's own path")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), schemaRelPath))
	if err != nil {
		t.Fatalf("read contract: %v", err)
	}
	var doc struct {
		Properties map[string]struct {
			MaxLength *int   `json:"maxLength"`
			Pattern   string `json:"pattern"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse contract: %v", err)
	}

	// All three producer ids are upstream-controlled text reaching a stored key
	// verbatim, so all three carry the bound. Checking every one keeps a future
	// lane from being added without it — the asymmetry this test was written to
	// retire.
	for _, field := range []string{"gateway_request_id", "otel_request_id", "proxy_request_id"} {
		p, declared := doc.Properties[field]
		if !declared {
			t.Errorf("%s is not declared in the contract", field)
			continue
		}
		if p.MaxLength == nil {
			t.Errorf("%s declares no maxLength; its bound rests on the producer alone", field)
			continue
		}
		if *p.MaxLength != maxRequestIDLen {
			t.Errorf("%s: contract maxLength is %d but this producer bounds at %d — "+
				"one of the two is now emitting or accepting what the other refuses",
				field, *p.MaxLength, maxRequestIDLen)
		}
		if p.Pattern == "" {
			t.Errorf("%s declares no pattern; a control character would pass the contract", field)
			continue
		}
		re, err := regexp.Compile(p.Pattern)
		if err != nil {
			t.Errorf("%s: contract pattern %q does not compile: %v", field, p.Pattern, err)
			continue
		}
		// The two rules must agree on every value, not merely coexist. These are
		// the boundaries where a disagreement would live.
		for _, v := range []string{
			"req_011CSxKq9mNp",                      // ordinary upstream id
			GatewayIDPrefix + "1a2b3c4d5e6f",        // a minted fallback
			strings.Repeat("x", maxRequestIDLen),    // exactly at the bound
			strings.Repeat("x", maxRequestIDLen+1),  // one past it
			"req 123", "req\n123", "req\x01123", "", // charset rejections
			"req_ü123", // where bytes and code points differ
		} {
			byProducer := usableRequestID(v)
			byContract := re.MatchString(v) && len([]rune(v)) <= *p.MaxLength
			if byProducer != byContract {
				t.Errorf("%s: producer accepts=%v but contract accepts=%v for %q — the two bounds disagree",
					field, byProducer, byContract, v)
			}
		}
	}
}

// And the ids this producer actually mints must clear that bound. A rule the
// producer's own output violates would be caught at ingest, or nowhere.
func TestEmittedGatewayIDsAreUsable(t *testing.T) {
	e := &Emitter{Now: func() time.Time { return time.Unix(1756000000, 0).UTC() }}
	for _, tc := range []struct {
		name string
		id   string
	}{
		{"upstream id accepted verbatim", e.requestID(gateway.Captured{
			ResponseHeaders: map[string]string{upstreamRequestHeader: "req_011CSxKq9mNpQrStUvWxYz"},
		})},
		{"minted fallback", e.requestID(gateway.Captured{})},
	} {
		if !usableRequestID(tc.id) {
			t.Errorf("%s: this producer minted %q, which its own bound refuses", tc.name, tc.id)
		}
	}
}
