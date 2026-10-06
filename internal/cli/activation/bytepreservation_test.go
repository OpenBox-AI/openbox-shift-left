package activation

import (
	"os"
	"strings"
	"testing"
)

// settingsAsTheDeveloperWroteIt is a settings.json in the shape a person
// actually leaves one in: keys in the order they were added rather than
// alphabetical, two-space and four-space indentation mixed, a comment-like
// long value, and blocks this tool has never heard of. Every byte of it belongs
// to the developer.
const settingsAsTheDeveloperWroteIt = `{
  "zzz_written_last": true,
  "model": "opus",
  "permissions": {
    "allow": [
        "Bash",
        "Read"
    ],
    "deny": []
  },
  "env": {
    "CORP_TOKEN_PATH": "/etc/corp/token",
    "AAA_ALPHABETICALLY_FIRST": "kept"
  },
  "aaa_written_first": 2
}
`

// TestActivateDeactivateLeavesTheDevelopersBytesAlone.
//
// The code already refuses to rewrite this file when it cannot parse it, and
// says so in the error: the intent to leave a developer's own file alone is
// explicit. The success path then rewrites every byte of it. Go marshals a map
// in sorted key order, so a round trip through map[string]any alphabetises the
// whole document, re-indents it, and reorders `env` -- none of which the tool
// was asked to do.
//
// Activate followed by Deactivate is the case where the tool ends up with
// nothing of its own to record, so the file it leaves behind should be the file
// it found.
func TestActivateDeactivateLeavesTheDevelopersBytesAlone(t *testing.T) {
	home := t.TempDir()
	seed(t, home, settingsAsTheDeveloperWroteIt)
	path := settingsPath(home)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	activate(t, home, LaneGateway, map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:8788"})
	if _, err := Deactivate(home, path, LaneGateway, false); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("activate+deactivate rewrote the developer's file.\n--- before ---\n%s\n--- after ---\n%s\n"+
			"Every key here belongs to the developer, and the tool put back nothing of its own; the "+
			"difference is reordering and reformatting it was never asked to do", before, after)
	}
}

// TestActivateTouchesOnlyTheKeysItSets is the weaker property that must hold
// even when the tool does have something to record: the keys it did not set
// keep their order, their indentation and their values.
func TestActivateTouchesOnlyTheKeysItSets(t *testing.T) {
	home := t.TempDir()
	seed(t, home, settingsAsTheDeveloperWroteIt)
	path := settingsPath(home)

	activate(t, home, LaneGateway, map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:8788"})

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(after)

	// Key order outside env.
	iLast := strings.Index(got, "zzz_written_last")
	iFirst := strings.Index(got, "aaa_written_first")
	if iLast < 0 || iFirst < 0 {
		t.Fatalf("a top-level key the tool does not own went missing:\n%s", got)
	}
	if iLast > iFirst {
		t.Errorf("the tool alphabetised the developer's top-level keys: zzz_written_last moved after "+
			"aaa_written_first.\n%s", got)
	}

	// Key order inside env, where the tool did add a key of its own.
	iCorp := strings.Index(got, "CORP_TOKEN_PATH")
	iAAA := strings.Index(got, "AAA_ALPHABETICALLY_FIRST")
	if iCorp < 0 || iAAA < 0 {
		t.Fatalf("an env key the tool does not own went missing:\n%s", got)
	}
	if iCorp > iAAA {
		t.Errorf("the tool alphabetised the developer's env block: CORP_TOKEN_PATH moved after "+
			"AAA_ALPHABETICALLY_FIRST.\n%s", got)
	}

	// Its own key really is there, or the assertions above are vacuous.
	if !strings.Contains(got, "ANTHROPIC_BASE_URL") {
		t.Errorf("the tool's own key is absent, so this test proves nothing:\n%s", got)
	}
}

// TestInvalidJSONIsStillRefused. The guard is the whole safety property once
// the writer no longer round-trips through encoding/json: a path editor will
// happily edit a malformed document, where Unmarshal refused as a side effect.
func TestInvalidJSONIsStillRefused(t *testing.T) {
	home := t.TempDir()
	const malformed = `{"env": {"A": "1",}` // trailing comma, unclosed brace
	seed(t, home, malformed)
	path := settingsPath(home)

	_, err := Activate(home, path, LaneGateway, map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:8788"})
	if err == nil {
		t.Fatal("Activate rewrote a settings file it could not parse")
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("error does not say why it refused: %v", err)
	}
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(raw) != malformed {
		t.Errorf("the refusal still rewrote the file:\n%s", raw)
	}
}

// TestNonObjectEnvIsStillRefused: `env` holding a string is a shape this writer
// must not silently replace with an object.
func TestNonObjectEnvIsStillRefused(t *testing.T) {
	home := t.TempDir()
	const wrongShape = `{"env": "not an object"}`
	seed(t, home, wrongShape)
	path := settingsPath(home)

	if _, err := Activate(home, path, LaneGateway, map[string]string{"ANTHROPIC_BASE_URL": "x"}); err == nil {
		t.Fatal("Activate replaced a non-object env block")
	} else if !strings.Contains(err.Error(), "not a JSON object") {
		t.Errorf("error does not name the shape problem: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != wrongShape {
		t.Errorf("the refusal still rewrote the file:\n%s", raw)
	}
}
