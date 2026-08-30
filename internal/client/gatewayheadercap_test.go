package client

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// Gatewayheadercap_test.go bounds the one content-bearing field on a gateway
// span that reached the wire uncapped.

// TestGatewayHeadersAreCappedOnTheWire asserts on the built span rather than
// on the struct, because the struct is not what gets signed.
func TestGatewayHeadersAreCappedOnTheWire(t *testing.T) {
	huge := strings.Repeat("A", 512<<10) // 512 KiB in one header value
	req := map[string]string{"User-Agent": huge}
	resp := map[string]string{"X-Provider-Note": huge}
	for i := 0; i < maxHeaderCount*3; i++ {
		resp[fmt.Sprintf("X-Pad-%03d", i)] = "v"
	}

	span := observedSpan(DevEvent{
		SessionID:        "sess-1",
		GatewayRequestID: "gwreq-1",
		Span: &Span{
			HTTPMethod: "POST", HTTPURL: "https://api.anthropic.com/v1/messages",
			HTTPStatus: 200, RequestHeaders: req, ResponseHeaders: resp,
		},
	})
	if span == nil {
		t.Fatal("no gateway span was built")
	}

	if got := len(span.RequestHeaders["User-Agent"]); got > maxHeaderValueBytes+32 {
		t.Errorf("request header value is %d bytes, want <= %d (+ truncation marker)",
			got, maxHeaderValueBytes)
	}
	if got := len(span.ResponseHeaders); got > maxHeaderCount {
		t.Errorf("response header count is %d, want <= %d", got, maxHeaderCount)
	}
	if !strings.Contains(span.RequestHeaders["User-Agent"], "truncated") {
		t.Error("a truncated header value is not marked as truncated")
	}
}

// TestGatewayHeaderCapIsDeterministic is the reason keys are sorted.
func TestGatewayHeaderCapIsDeterministic(t *testing.T) {
	in := map[string]string{}
	for i := 0; i < maxHeaderCount*4; i++ {
		in[fmt.Sprintf("X-H-%04d", i)] = "v"
	}
	first := capHeaders(in)
	for attempt := 0; attempt < 40; attempt++ {
		got := capHeaders(in)
		if len(got) != len(first) {
			t.Fatalf("attempt %d kept %d headers, first kept %d", attempt, len(got), len(first))
		}
		for k := range first {
			if _, ok := got[k]; !ok {
				t.Fatalf("attempt %d dropped %q, which the first attempt kept; the cap is not deterministic", attempt, k)
			}
		}
	}
}

// TestGatewayHeaderCapKeepsRunesWhole pins that a multi-byte value is not cut
// mid-character, the same property capBody has.
func TestGatewayHeaderCapKeepsRunesWhole(t *testing.T) {
	got := capHeaders(map[string]string{"X-Note": strings.Repeat("日", maxHeaderValueBytes+64)})
	v := got["X-Note"]
	if !utf8.ValidString(v) {
		t.Error("capped header value is not valid UTF-8; the cut landed mid-rune")
	}
	if strings.ContainsRune(strings.TrimSuffix(v, "…[truncated]"), utf8.RuneError) {
		t.Error("capped header value contains U+FFFD; a partial rune survived")
	}
}

// TestGatewayHeaderCapLeavesOrdinaryHeadersAlone is the no-op half: real
// traffic must pass through byte-identical, or the stored evidence stops
// matching the exchange.
func TestGatewayHeaderCapLeavesOrdinaryHeadersAlone(t *testing.T) {
	in := map[string]string{
		"Authorization":    "[redacted]",
		"Anthropic-Beta":   "prompt-caching-2024-07-31,computer-use-2024-10-22",
		"Content-Type":     "application/json",
		"User-Agent":       "claude-cli/2.1.229 (external, cli)",
		"X-Stainless-Lang": "js",
	}
	got := capHeaders(in)
	if len(got) != len(in) {
		t.Fatalf("kept %d of %d ordinary headers", len(got), len(in))
	}
	for k, v := range in {
		if got[k] != v {
			t.Errorf("header %q changed: %q -> %q", k, v, got[k])
		}
	}
	if capHeaders(nil) != nil {
		t.Error("an empty header map must stay nil, so omitempty drops the key")
	}
}

// TestGatewayHeaderCapIsAByteBound is the gap the first version of this bound
// could not hold. Both directions are asserted, because the byte assertion
// alone would also pass an implementation that mangled characters: the result
// must be within budget AND still be valid UTF-8.
func TestGatewayHeaderCapIsAByteBound(t *testing.T) {
	for name, value := range map[string]string{
		"CJK over the byte bound but under it in runes": strings.Repeat("日", 2000),
		"astral plane, 4 bytes per rune":                strings.Repeat("𝄞", 2000),
		// It stays so the fix cannot regress the case that already worked.
		"ASCII over the bound": strings.Repeat("a", maxHeaderValueBytes+64),
	} {
		t.Run(name, func(t *testing.T) {
			got := capHeaders(map[string]string{"X-Big": value})["X-Big"]

			body := strings.TrimSuffix(got, "…[truncated]")
			if len(body) > maxHeaderValueBytes {
				t.Errorf("value kept %d bytes, over the %d-byte bound; the cap did not hold",
					len(body), maxHeaderValueBytes)
			}
			if !strings.HasSuffix(got, "…[truncated]") {
				t.Error("an over-bound value was not marked as truncated; a reader cannot tell a short header from a shortened one")
			}
			if !utf8.ValidString(got) {
				t.Error("the cut produced invalid UTF-8; json.Marshal would rewrite it to U+FFFD")
			}
		})
	}
}
