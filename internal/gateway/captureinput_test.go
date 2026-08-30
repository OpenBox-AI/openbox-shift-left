package gateway

import (
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Captureinput_test.go holds the redactor's input bound; a different bound
// from the wire cap, guarding CPU rather than bytes on the wire.

// TestCaptureInputBoundExceedsTheWireCap is the vacuity control, and it exists
// so a later "unify the constants" commit cannot kill the wire cap silently.
func TestCaptureInputBoundExceedsTheWireCap(t *testing.T) {
	if maxCaptureInputBytes <= captureBodyRunes {
		t.Fatalf("maxCaptureInputBytes (%d) must exceed captureBodyRunes (%d): "+
			"an input bound at or below the wire cap makes the wire cap vacuous",
			maxCaptureInputBytes, captureBodyRunes)
	}
	if maxCaptureSinkBytes != maxCaptureInputBytes {
		t.Errorf("maxCaptureSinkBytes (%d) != maxCaptureInputBytes (%d): the two "+
			"bounds are the same idea and drifting them apart leaves one direction unbounded",
			maxCaptureSinkBytes, maxCaptureInputBytes)
	}
}

// TestCaptureBodyBoundsRedactorInput is the cost control. The assertion is a
// latency one on purpose: the defect was not a wrong value, it was ~11.4s of
// synchronous CPU in front of the forward for a 64 MiB body.
func TestCaptureBodyBoundsRedactorInput(t *testing.T) {
	unit := `{"role":"user","content":"refactor the handler and keep the tests green"},`
	body := strings.Repeat(unit, (64<<20)/len(unit))

	start := time.Now()
	got := captureBody(body)
	elapsed := time.Since(start)

	const budget = 2 * time.Second
	if elapsed > budget {
		t.Errorf("captureBody over a %d-byte body took %s, over the %s budget; "+
			"the redactor input bound is not being applied",
			len(body), elapsed.Round(time.Millisecond), budget)
	}
	if n := utf8.RuneCountInString(got); n > captureBodyRunes {
		t.Errorf("captured %d runes, over the %d-rune wire cap", n, captureBodyRunes)
	}
}

// TestCaptureBodyTrimsToARuneBoundary pins the UTF-8 half. A mid-rune byte cut
// would leave a tail json.Marshal silently rewrites to U+fffd, so the stored
// evidence would end in a character the exchange never contained.
func TestCaptureBodyTrimsToARuneBoundary(t *testing.T) {
	body := strings.Repeat("日", (maxCaptureInputBytes/3)+16)
	got := captureBody(body)
	if !utf8.ValidString(got) {
		t.Error("captured body is not valid UTF-8; the input trim cut mid-rune")
	}
	if got == "" {
		t.Fatal("captured body is empty; the trim removed everything")
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Error("captured body contains U+FFFD; a partial rune reached the output")
	}
}

// TestCaptureBodyRepairsABodyAlreadyCutAtTheBound is the case the trim above
// could not reach, and it is the one that actually happens.
//   - The cut must land MID-rune.
//   - Redaction must shrink the body under the wire cap.
func TestCaptureBodyRepairsABodyAlreadyCutAtTheBound(t *testing.T) {
	val := strings.Repeat("aG9sZHRoaXNpc2Fsb25nUmFuZG9tVmFsdWU5ODc2", 10) // 400 chars, high entropy
	chunk := "api_key=" + val + "\n"                                      // 409 bytes, redactable
	body := strings.Repeat(chunk, 400) + "\n" + strings.Repeat("\U0001F600", 30000)

	preCut := capturableBody([]byte(body), http.Header{})
	if len(preCut) != maxCaptureInputBytes {
		t.Fatalf("precondition: capturableBody returned %d bytes, want exactly %d", len(preCut), maxCaptureInputBytes)
	}
	if utf8.ValidString(preCut) {
		t.Fatal("precondition: the pre-cut body is valid UTF-8, so this test cannot detect the repair")
	}

	got := captureBody(preCut)
	if n := utf8.RuneCountInString(got); n >= captureBodyRunes {
		t.Fatalf("precondition: result is %d runes, at or over the %d-rune cap; capRunes would mask the defect", n, captureBodyRunes)
	}
	if !strings.Contains(got, "OPENBOX_REDACTED") {
		t.Fatal("precondition: nothing was redacted, so the body never shrank under the cap")
	}
	if !utf8.ValidString(got) {
		t.Error("captured body is not valid UTF-8; the partial rune from the producer's cut survived to the wire")
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Error("captured body contains U+FFFD; a partial rune reached the output")
	}
}

// TestCaptureBodyStillRedactsWithinTheBound is the security half: bounding the
// input must not weaken redaction of what is kept.
//   - The "secret" was `${OPENBOX_REDACTED_AWS_KEY}`; the redactor's OWN
//     output placeholder, which no pattern matches.
//   - It was placed past the 65,536-rune wire cap, so capRunes removed it
//     whatever the redactor did.
func TestCaptureBodyStillRedactsWithinTheBound(t *testing.T) {
	const awsKey = "AKIAIOSFODNN7EXAMPLE"
	const assigned = "hunter2hunter2"
	body := strings.Repeat("x", 1024) + " " + awsKey +
		` "password":"` + assigned + `" ` + strings.Repeat("y", 64<<10)

	got := captureBody(body)
	if strings.Contains(got, awsKey) {
		t.Error("an AWS key inside the bounded window survived redaction")
	}
	if strings.Contains(got, assigned) {
		t.Error("an assigned password inside the bounded window survived redaction")
	}
	if !strings.Contains(got, "OPENBOX_REDACTED") {
		t.Errorf("no redaction placeholder; the body was truncated, not scanned:\n%.200s", got)
	}
	if !strings.Contains(got, strings.Repeat("x", 1024)) {
		t.Error("the non-secret prefix did not survive; this proves nothing about redaction")
	}

	h := http.Header{"Authorization": []string{"Bearer " + awsKey}}
	rc := CaptureRequest("POST", "https://api.anthropic.com/v1/messages", h, body)
	if rc.Fingerprint == "" {
		t.Error("no credential fingerprint; it must be taken before header redaction")
	}
	if strings.Contains(rc.Body, awsKey) {
		t.Error("the secret reached the captured request body")
	}
	if v := rc.Headers["Authorization"]; v != redactedHeaderValue {
		t.Errorf("Authorization = %q, want %q", v, redactedHeaderValue)
	}
}
