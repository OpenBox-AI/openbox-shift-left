package artifact

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

type invalidTextMarshaler struct{}

func (invalidTextMarshaler) MarshalText() ([]byte, error) {
	return []byte{0xff}, nil
}

type opaqueJSONMarshaler struct{}

func (opaqueJSONMarshaler) MarshalJSON() ([]byte, error) {
	return []byte(`"opaque"`), nil
}

func TestCanonicalizeJSONRFC8785SerializationSample(t *testing.T) {
	raw := []byte(`{"numbers":[333333333.33333329,1E30,4.50,2e-3,0.000000000000000000000000001],"string":"€$\u000f\nA'B\"\\\\\"/","literals":[null,true,false]}`)
	want := `{"literals":[null,true,false],"numbers":[333333333.3333333,1e+30,4.5,0.002,1e-27],"string":"€$\u000f\nA'B\"\\\\\"/"}`

	got, err := CanonicalizeJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("canonical JSON = %s\nwant           = %s", got, want)
	}
}

func TestCanonicalizeJSONSortsNamesByUTF16CodeUnits(t *testing.T) {
	raw := []byte(`{"€":"Euro Sign","\r":"Carriage Return","דּ":"Hebrew Letter Dalet With Dagesh","1":"One","😀":"Emoji: Grinning Face","\u0080":"Control","ö":"Latin Small Letter O With Diaeresis"}`)
	want := `{"\r":"Carriage Return","1":"One","":"Control","ö":"Latin Small Letter O With Diaeresis","€":"Euro Sign","😀":"Emoji: Grinning Face","דּ":"Hebrew Letter Dalet With Dagesh"}`

	got, err := CanonicalizeJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("canonical JSON = %s\nwant           = %s", got, want)
	}
}

func TestCanonicalizeJSONNumberBoundaries(t *testing.T) {
	tests := map[string]string{
		`-0`:    `0`,
		`1e-7`:  `1e-7`,
		`1e-6`:  `0.000001`,
		`1e20`:  `100000000000000000000`,
		`1e21`:  `1e+21`,
		`-1e21`: `-1e+21`,
	}
	for raw, want := range tests {
		t.Run(raw, func(t *testing.T) {
			got, err := CanonicalizeJSON([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != want {
				t.Fatalf("canonical number = %s, want %s", got, want)
			}
		})
	}
}

func TestCanonicalizeJSONRFC8785NumberEdges(t *testing.T) {
	tests := map[string]string{
		`5e-324`:                    `5e-324`,
		`-5e-324`:                   `-5e-324`,
		`1.7976931348623157e308`:    `1.7976931348623157e+308`,
		`-1.7976931348623157e308`:   `-1.7976931348623157e+308`,
		`9007199254740992`:          `9007199254740992`,
		`-9007199254740992`:         `-9007199254740992`,
		`295147905179352830000`:     `295147905179352830000`,
		`9.999999999999997e22`:      `9.999999999999997e+22`,
		`1e23`:                      `1e+23`,
		`1.0000000000000001e23`:     `1.0000000000000001e+23`,
		`999999999999999700000`:     `999999999999999700000`,
		`999999999999999900000`:     `999999999999999900000`,
		`0.000001`:                  `0.000001`,
		`9.999999999999997e-7`:      `9.999999999999997e-7`,
		`0.0000010000000000000002`:  `0.0000010000000000000002`,
		`333333333.3333332`:         `333333333.3333332`,
		`333333333.33333325`:        `333333333.33333325`,
		`333333333.3333333`:         `333333333.3333333`,
		`333333333.3333334`:         `333333333.3333334`,
		`333333333.33333343`:        `333333333.33333343`,
		`-0.0000033333333333333333`: `-0.0000033333333333333333`,
		`1424953923781206.25`:       `1424953923781206.2`,
	}
	for raw, want := range tests {
		t.Run(raw, func(t *testing.T) {
			got, err := CanonicalizeJSON([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != want {
				t.Fatalf("canonical number = %s, want %s", got, want)
			}
		})
	}
}

func TestCanonicalJSONStableAcrossMapInsertionOrder(t *testing.T) {
	left := make(map[string]any)
	left["z"] = []any{true, nil, "<&>"}
	left["a"] = 1
	right := make(map[string]any)
	right["a"] = 1
	right["z"] = []any{true, nil, "<&>"}

	leftBytes, leftDigest, err := DigestCanonicalJSON(left)
	if err != nil {
		t.Fatal(err)
	}
	rightBytes, rightDigest, err := DigestCanonicalJSON(right)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":1,"z":[true,null,"<&>"]}`
	if string(leftBytes) != want || string(rightBytes) != want {
		t.Fatalf("canonical bytes differ: left=%s right=%s", leftBytes, rightBytes)
	}
	if leftDigest != rightDigest {
		t.Fatalf("digests differ: left=%s right=%s", leftDigest, rightDigest)
	}
}

func TestCanonicalizeJSONRejectsInvalidIJSON(t *testing.T) {
	tests := map[string][]byte{
		"duplicate name":           []byte(`{"a":1,"a":2}`),
		"escaped duplicate name":   []byte(`{"a":1,"\u0061":2}`),
		"surrogate duplicate name": []byte(`{"😀":1,"\ud83d\ude00":2}`),
		"invalid UTF-8":            {'"', 0xff, '"'},
		"high surrogate":           []byte(`"\ud800"`),
		"low surrogate":            []byte(`"\udc00"`),
		"bad surrogate pair":       []byte(`"\ud800\u0061"`),
		"non-finite overflow":      []byte(`1e400`),
		"trailing data":            []byte(`{} {}`),
		"leading zero":             []byte(`01`),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := CanonicalizeJSON(raw); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestCanonicalJSONRejectsUnsafeGoValues(t *testing.T) {
	tests := map[string]any{
		"positive infinity":      math.Inf(1),
		"not a number":           math.NaN(),
		"invalid UTF-8 string":   string([]byte{0xff}),
		"invalid text marshaler": invalidTextMarshaler{},
		"opaque JSON marshaler":  opaqueJSONMarshaler{},
	}
	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := CanonicalJSON(value); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestCanonicalJSONLeavesIntegerBoundsToSchemaValidation(t *testing.T) {
	for _, value := range []any{int64(1 << 53), json.Number("9007199254740992")} {
		got, err := CanonicalJSON(value)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "9007199254740992" {
			t.Fatalf("canonical number = %s", got)
		}
	}
}

func TestCanonicalizeJSONRejectsExcessiveDepth(t *testing.T) {
	raw := strings.Repeat("[", maxCanonicalDepth+1) + strings.Repeat("]", maxCanonicalDepth+1)
	if _, err := CanonicalizeJSON([]byte(raw)); err == nil {
		t.Fatal("expected excessive nesting rejection")
	}
	allowed := strings.Repeat("[", maxCanonicalDepth) + strings.Repeat("]", maxCanonicalDepth)
	if _, err := CanonicalizeJSON([]byte(allowed)); err != nil {
		t.Fatalf("maximum nesting rejected: %v", err)
	}
}

func TestCanonicalJSONRejectsCycles(t *testing.T) {
	type node struct {
		Next *node `json:"next"`
	}
	pointerCycle := &node{}
	pointerCycle.Next = pointerCycle
	mapCycle := map[string]any{}
	mapCycle["self"] = mapCycle
	sliceCycle := []any{nil}
	sliceCycle[0] = sliceCycle

	for name, value := range map[string]any{
		"pointer": pointerCycle,
		"map":     mapCycle,
		"slice":   sliceCycle,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := CanonicalJSON(value); err == nil {
				t.Fatal("expected cycle rejection")
			}
		})
	}
}

func TestContentDigestSyntaxAndJSONRoundTrip(t *testing.T) {
	digest := DigestBytes([]byte("abc"))
	const want = "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if digest.String() != want {
		t.Fatalf("digest = %s, want %s", digest, want)
	}
	parsed, err := ParseContentDigest(want)
	if err != nil {
		t.Fatal(err)
	}
	if parsed != digest {
		t.Fatalf("parsed digest = %s, want %s", parsed, digest)
	}

	type envelope struct {
		Digest ContentDigest `json:"digest"`
	}
	encoded, err := json.Marshal(envelope{Digest: digest})
	if err != nil {
		t.Fatal(err)
	}
	var decoded envelope
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Digest != digest {
		t.Fatalf("round-trip digest = %s, want %s", decoded.Digest, digest)
	}
	canonical, err := CanonicalJSON(envelope{Digest: digest})
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != `{"digest":"`+want+`"}` {
		t.Fatalf("canonical digest envelope = %s", canonical)
	}

	invalid := []string{
		"sha256:BA7816BF8F01CFEA414140DE5DAE2223B00361A396177A9CB410FF61F20015AD",
		"sha256:abc",
		"sha512:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
	}
	for _, value := range invalid {
		if _, err := ParseContentDigest(value); err == nil {
			t.Fatalf("expected %q to be rejected", value)
		}
	}

	var nilDigest *ContentDigest
	if err := nilDigest.UnmarshalText([]byte(want)); err == nil {
		t.Fatal("expected nil receiver rejection")
	}
}

func FuzzCanonicalizeJSONIdempotent(f *testing.F) {
	for _, seed := range []string{
		`null`,
		`{"b":2,"a":1}`,
		`[1e30,0.000001,"€",true]`,
		`{"emoji":"\ud83d\ude00"}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		canonical, err := CanonicalizeJSON(raw)
		if err != nil {
			return
		}
		if !json.Valid(canonical) {
			t.Fatalf("canonical output is not valid JSON: %q", canonical)
		}
		repeated, err := CanonicalizeJSON(canonical)
		if err != nil {
			t.Fatal(err)
		}
		if string(repeated) != string(canonical) {
			t.Fatalf("canonicalization is not idempotent: first=%q second=%q", canonical, repeated)
		}
	})
}
