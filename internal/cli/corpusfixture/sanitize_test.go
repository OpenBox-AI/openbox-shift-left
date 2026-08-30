package corpusfixture

import (
	"encoding/json"
	"strings"
	"testing"
)

// Sanitize_test.go; the sanitizer's own test, written before the sanitizer.
// Sanitize must remove every real identity, and it must preserve every field a
// consumer parses; because a fixture that is clean and inert proves nothing,
// and would look exactly like a fixture that works.

const unsanitized = `{
  "resourceLogs": [{
    "resource": {"attributes": [
      {"key": "service.name", "value": {"stringValue": "claude-code-desktop"}},
      {"key": "process.owner", "value": {"stringValue": "realdev"}}
    ]},
    "scopeLogs": [{
      "logRecords": [{
        "timeUnixNano": "1787945811534000000",
        "body": {"stringValue": "claude_code.api_request"},
        "attributes": [
          {"key": "event.name", "value": {"stringValue": "api_request"}},
          {"key": "session.id", "value": {"stringValue": "07c7412f-10e5-4da2-a05c-6949da9ae927"}},
          {"key": "request_id", "value": {"stringValue": "req_011CVrealrequestid00"}},
          {"key": "user.email", "value": {"stringValue": "real.person@example-corp.com"}},
          {"key": "user.id", "value": {"stringValue": "b55705345a5d6d700ee59956e6da1596f422a98d"}},
          {"key": "organization.id", "value": {"stringValue": "6f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8"}},
          {"key": "model", "value": {"stringValue": "claude-fable-5"}},
          {"key": "input_tokens", "value": {"intValue": "2"}},
          {"key": "duration_ms", "value": {"stringValue": "1843"}},
          {"key": "cwd", "value": {"stringValue": "/Users/realdev/Code/secret-project"}}
        ]
      }]
    }]
  }]
}`

func attrOf(t *testing.T, raw []byte, key string) string {
	t.Helper()
	var doc struct {
		ResourceLogs []struct {
			Resource struct {
				Attributes []otlpAttr `json:"attributes"`
			} `json:"resource"`
			ScopeLogs []struct {
				LogRecords []struct {
					Attributes []otlpAttr `json:"attributes"`
				} `json:"logRecords"`
			} `json:"scopeLogs"`
		} `json:"resourceLogs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal sanitized doc: %v", err)
	}
	for _, rl := range doc.ResourceLogs {
		for _, a := range rl.Resource.Attributes {
			if a.Key == key {
				return a.Value.StringValue
			}
		}
		for _, sl := range rl.ScopeLogs {
			for _, lr := range sl.LogRecords {
				for _, a := range lr.Attributes {
					if a.Key == key {
						return a.Value.StringValue
					}
				}
			}
		}
	}
	return ""
}

type otlpAttr struct {
	Key   string `json:"key"`
	Value struct {
		StringValue string `json:"stringValue"`
		IntValue    string `json:"intValue"`
	} `json:"value"`
}

func TestSanitizeRemovesEveryRealIdentity(t *testing.T) {
	out, err := Sanitize([]byte(unsanitized))
	if err != nil {
		t.Fatalf("Sanitize: %v", err)
	}
	for _, real := range []string{
		"realdev",
		"07c7412f-10e5-4da2-a05c-6949da9ae927",
		"req_011CVrealrequestid00",
		"real.person@example-corp.com",
		"b55705345a5d6d700ee59956e6da1596f422a98d",
		"6f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8",
		"/Users/realdev",
		"secret-project",
	} {
		if strings.Contains(string(out), real) {
			t.Errorf("real identity %q survived sanitization", real)
		}
	}
}

// TestSanitizePreservesWhatTheMapperParses is the other direction, and it is
// the one a sanitizer quietly fails.
func TestSanitizePreservesWhatTheMapperParses(t *testing.T) {
	out, err := Sanitize([]byte(unsanitized))
	if err != nil {
		t.Fatalf("Sanitize: %v", err)
	}

	if got := attrOf(t, out, "model"); got != "claude-fable-5" {
		t.Errorf("model must survive verbatim: got %q", got)
	}
	if got := attrOf(t, out, "duration_ms"); got != "1843" {
		t.Errorf("duration_ms must survive verbatim: got %q", got)
	}
	if got := attrOf(t, out, "event.name"); got != "api_request" {
		t.Errorf("event.name must survive verbatim: got %q", got)
	}

	for _, key := range []string{"session.id", "request_id"} {
		got := attrOf(t, out, key)
		if got == "" {
			t.Fatalf("%s was erased; the mapper drops a record without it", key)
		}
		if !identifierSafe(got) {
			t.Errorf("%s placeholder %q is not charset-safe; the mapper would drop the record", key, got)
		}
	}
}

// identifierSafe mirrors the mapper's own charset rule.
func identifierSafe(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return false
		case r == '-', r == '_', r == '.':
			return false
		}
		return true
	}) < 0
}

func TestSanitizeIsIdempotent(t *testing.T) {
	once, err := Sanitize([]byte(unsanitized))
	if err != nil {
		t.Fatalf("Sanitize: %v", err)
	}
	twice, err := Sanitize(once)
	if err != nil {
		t.Fatalf("Sanitize (second pass): %v", err)
	}
	if string(once) != string(twice) {
		t.Error("Sanitize is not idempotent; a regenerated fixture would churn its bytes")
	}
}

// TestScanCatchesWhatSanitizeMissed is the assertion half. Scan is what runs
// against the committed fixtures forever; Sanitize runs once, by hand, against
// a corpus that is not in this repository. So Scan must be red on unsanitized
// input on its own, without having been told what Sanitize did.
func TestScanCatchesWhatSanitizeMissed(t *testing.T) {
	v := Scan([]byte(unsanitized))
	if len(v) == 0 {
		t.Fatal("Scan found nothing in a document full of real identities")
	}

	out, err := Sanitize([]byte(unsanitized))
	if err != nil {
		t.Fatalf("Sanitize: %v", err)
	}
	if v := Scan(out); len(v) != 0 {
		t.Errorf("Scan still reports violations after sanitization: %v", v)
	}
}

// TestScanCatchesEachSentinelClassAlone stops the previous test from passing
// for one reason while every other class is unchecked.
func TestScanCatchesEachSentinelClassAlone(t *testing.T) {
	for name, doc := range map[string]string{
		"email":        `{"a":"someone@example.com"}`,
		"home path":    `{"a":"/Users/someone/Code"}`,
		"bearer token": `{"authorization":"Bearer abc123def456ghi789"}`,
		"api key":      `{"a":"sk-` + "ant-" + strings.Repeat("0123456789", 4) + `"}`,
		"identity key": `{"user.account_uuid":"6f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8"}`,
		"header":       `{"headers":{"x-organization-uuid":"6f1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8"}}`,
		"otlp attr":    `{"attributes":[{"key":"user.email","value":{"stringValue":"a@b.com"}}]}`,
	} {
		if v := Scan([]byte(doc)); len(v) == 0 {
			t.Errorf("%s: Scan found nothing in %s", name, doc)
		}
	}
}
