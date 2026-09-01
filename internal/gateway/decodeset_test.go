package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// decodeSetExceptions are the encodings the corpus client ADVERTISES and this
// relay deliberately does not decode. Naming them here is the point: an
// encoding is either decoded or it is an entry in this map with a reason, and
// there is no third state where nobody noticed.
//
// Both were advertised on every recorded request and returned by an origin
// ZERO times in 83,190 responses. A decoder for an unobserved codec buys a
// dependency and no evidence; if either ever shows up, the guard below fails on
// the corpus that proves it and the entry gets deleted rather than debated.
var decodeSetExceptions = map[string]string{
	"zstd":    "advertised by the client, returned by no origin in 83,190 recorded responses",
	"deflate": "advertised by the client, returned by no origin in 83,190 recorded responses",
}

// TestTheDecodeSetCoversEveryEncodingTheCorpusAdvertises is the guard whose
// absence let the defect ship. `decode.go` asserted that gzip "was on 118 of
// 118 captured responses" and that br was "unobserved" -- but the measurement it
// cited never recorded an encoding VALUE, so the premise was unfalsifiable, and
// it was wrong: br was 89.5% of responses and stored a marker for every one.
//
// This converts that premise into a checked claim against the one artifact in
// the tree that records what a real client asks for. It fails on a tree with
// brotli removed, which is the property that matters -- the old comment could
// not fail at all.
func TestTheDecodeSetCoversEveryEncodingTheCorpusAdvertises(t *testing.T) {
	dir := filepath.Join(repoRootForCorpus(t), "internal", "transport", "testdata", "corpus")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the recorded corpus at %s: %v", dir, err)
	}

	advertised := map[string][]string{} // token -> fixtures that advertised it
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		var fixture struct {
			Request struct {
				Headers map[string]string `json:"headers"`
			} `json:"request"`
		}
		if err := json.Unmarshal(raw, &fixture); err != nil {
			t.Fatalf("parsing %s: %v", e.Name(), err)
		}
		for name, value := range fixture.Request.Headers {
			if !strings.EqualFold(name, "Accept-Encoding") {
				continue
			}
			for _, token := range strings.Split(value, ",") {
				token = strings.ToLower(strings.TrimSpace(token))
				// `identity` is not a codec, and a `q=0` preference is not a
				// request to encode.
				if token == "" || token == "identity" || strings.Contains(token, ";") {
					continue
				}
				advertised[token] = append(advertised[token], e.Name())
			}
		}
	}

	if len(advertised) == 0 {
		t.Fatal("no Accept-Encoding token found in the corpus; this guard would pass vacuously, " +
			"which is exactly the failure mode it exists to replace")
	}

	for token, fixtures := range advertised {
		if isDecodable(token) {
			if _, excepted := decodeSetExceptions[token]; excepted {
				t.Errorf("%q is both decoded and listed as an exception; one of the two is stale", token)
			}
			continue
		}
		if _, excepted := decodeSetExceptions[token]; !excepted {
			t.Errorf("the corpus client advertises Content-Encoding %q (in %v) and this relay neither "+
				"decodes it nor names it in decodeSetExceptions. Add the codec, or add the exception "+
				"with the measurement that justifies it -- silence here is how br shipped as a marker "+
				"for 89.5%% of response bodies.", token, fixtures)
		}
	}
}

// repoRootForCorpus walks up to the one go.mod. Hard-coding ../.. would break
// silently if this file moved, and a silent skip is the failure mode this whole
// test is a reaction to.
func repoRootForCorpus(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s; cannot locate the recorded corpus", dir)
		}
		dir = parent
	}
}
