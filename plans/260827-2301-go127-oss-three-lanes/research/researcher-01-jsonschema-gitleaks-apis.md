# Research: jsonschema/v6 + gitleaks/v8 library APIs

Sources: pkg.go.dev (godoc, generated from source — authoritative) for API shapes;
raw go.mod on GitHub tag v8.30.1 for gitleaks deps. Budget was 5 research tool
calls (used exactly 5); some sub-answers are inferred/lower-confidence, flagged
inline and in Unresolved.

## Topic 1 — santhosh-tekuri/jsonschema/v6 (target v6.0.3)
Source: https://pkg.go.dev/github.com/santhosh-tekuri/jsonschema/v6

### (a) Custom keyword / vocabulary
```go
type Vocabulary struct {
    URL        string
    Schema     *Schema      // meta-schema validating the vocab keyword's own shape
    Subschemas []SchemaPath
    Compile    func(ctx *CompilerContext, obj map[string]any) (SchemaExt, error)
}
type SchemaExt interface {
    Validate(ctx *ValidatorContext, v any)
}
func (c *Compiler) RegisterVocabulary(vocab *Vocabulary)
```
Sketch:
```go
c := jsonschema.NewCompiler()
c.RegisterVocabulary(&jsonschema.Vocabulary{
    URL:    "https://openbox.dev/vocab/x-content-gated",
    Schema: metaSchema, // *Schema describing the keyword's allowed shape
    Compile: func(ctx *jsonschema.CompilerContext, obj map[string]any) (jsonschema.SchemaExt, error) {
        v, ok := obj["x-content-gated"]
        if !ok { return nil, nil }
        return gatedExt{val: v}, nil
    },
})
type gatedExt struct{ val any }
func (g gatedExt) Validate(ctx *jsonschema.ValidatorContext, v any) {
    // report failure via ctx — exact method unconfirmed, see Unresolved #3
}
```
No separate `ExtCompiler` type surfaced (contrary to the name hypothesized in the
task) — `Vocabulary.Compile` is a plain func field; `SchemaExt` is the only
extension interface. Medium confidence, single AI-summarized source — verify
field names against actual source before coding.

### (b) In-memory compile
```go
func (c *Compiler) AddResource(url string, doc any) error  // doc = decoded JSON (map[string]any), not bytes/io.Reader
func (c *Compiler) Compile(loc string) (*Schema, error)
```
Pattern: `json.Unmarshal(schemaBytes, &doc)` -> `c.AddResource("mem://schema.json", doc)`
-> `c.Compile("mem://schema.json")`. `AddResource` pins the URL to the in-memory
doc so `Compile` never fetches externally — direct replacement for file-based
loading in `validator.go`.

### (c) Error reporting
```go
type ValidationError struct {
    SchemaURL        string
    InstanceLocation []string
    ErrorKind        ErrorKind
    Causes           []*ValidationError
}
func (e *ValidationError) Error() string
func (e *ValidationError) BasicOutput() *OutputUnit     // flat list
func (e *ValidationError) DetailedOutput() *OutputUnit  // hierarchical tree
func (e *ValidationError) FlagOutput() *FlagOutput      // bool only
```
`OutputUnit` carries `KeywordLocation`, `AbsoluteKeywordLocation`,
`InstanceLocation`. Walk `Causes` recursively (or call `DetailedOutput()`) for
path+message per leaf failure; `Error()` alone gives one human string. Covers
"readable conformance failures."

### (d) $ref / oneOf
Confirmed native: `Schema.Ref *Schema`, `Schema.OneOf []*Schema` fields exist;
library's whole purpose is full draft-2020-12 conformance. `$ref: "#/$defs/x"`
and oneOf branch trial are core spec features, not extensions — expect them to
work with zero extra code once the schema compiles.

## Topic 2 — zricethezav/gitleaks/v8 (target v8.30.1)
Sources: https://pkg.go.dev/github.com/zricethezav/gitleaks/v8/{detect,report,config};
https://raw.githubusercontent.com/zricethezav/gitleaks/v8.30.1/go.mod

### (a) Detector construction + default rules
```go
func NewDetector(cfg config.Config) *Detector
func NewDetectorContext(ctx context.Context, cfg config.Config) *Detector
func NewDetectorDefaultConfig() (*Detector, error)  // <- simplest path
```
Default ruleset lives in `config`, embedded (`//go:embed gitleaks.toml` into a
`DefaultConfig` string var — exact declaration syntax not cleanly extracted, but
the embed + `DefaultConfig` symbol are confirmed). Don't hand-parse it — call
`detect.NewDetectorDefaultConfig()` directly for a ready `*Detector`.

### (b) Finding struct — redaction feasibility
```go
type Finding struct {
    RuleID, Description          string
    StartLine, EndLine           int
    StartColumn, EndColumn       int
    Line, Match, Secret          string
    File, SymlinkFile            string
    Commit, Link                 string
    Entropy                      float64
    Author, Email, Date, Message string
    Tags                         []string
    Fingerprint                  string
    Fragment                     Fragment
}
func (f Finding) Redact(percent uint)  // <- built-in redaction helper, already exists
```
`Secret` is the exact matched substring; `StartColumn`/`EndColumn` are offsets
within `Line` (not a global multi-line offset) — for a single-line/single-
fragment scan (our case: redacting one in-memory string body) these map
directly onto the input. Combine `Line` + `StartColumn`/`EndColumn` (or
`strings.Index` on `Secret` for the simple case) to splice in a placeholder.
Bonus: `Finding.Redact(percent uint)` already exists — check it before
hand-rolling replacement logic; exact semantics (mutates in place vs returns
copy, full vs percent-masked) not confirmed this session (Unresolved #4).

### (c) Pure in-memory
```go
func (d *Detector) DetectString(content string) []report.Finding
func (d *Detector) DetectBytes(content []byte) []report.Finding
func (d *Detector) Detect(fragment Fragment) []report.Finding
```
All three are pure in-memory, no fs/git. Avoid `DetectGit`/`DetectFiles`/
`DetectSource` (fs/git-bound, irrelevant here).

### (d) Dependency tree size
go.mod @ v8.30.1: **14 direct + 37 indirect = 51 modules**, `go 1.24.11` (under
our 1.27 floor, fine). Direct: aho-corasick, sprig/v3, lipgloss, semgroup,
go-gitdiff, go-cmp, filetype, go-version, mholt/archives, zerolog, cobra,
viper, testify, x/exp. Heavy/notable: **cobra+viper** (CLI+config framework —
likely only needed by gitleaks' own `cmd` package, not `detect`/`report`;
`config` plausibly uses viper internally to parse the embedded TOML, so
viper's subtree may still get linked even without calling it directly —
unconfirmed, didn't inspect `config` package's imports), **mholt/archives**
(archive/compression scanning, probably dead weight for plain-string use),
**go-gitdiff** (git-diff parsing, same). Net: importing `detect`+`report`+
`config` likely pulls meaningfully more than the 353-line hand-rolled
detector even after Go's linker drops unreachable code — actual reachable
subset not verified (Unresolved #5).

### (e) Entropy in default rules
`config.Rule.Entropy float64` confirmed — doc: "minimum shannon entropy a
regex group must have to be considered a secret." Entropy is a **per-rule
threshold layered on a regex match** (hybrid), not a standalone "any
high-entropy substring is a secret" scan — consistent with gitleaks' known
`generic-api-key`-style rules. Not verified against actual `gitleaks.toml`
content this session (Unresolved #6).

## Assessment (for the plan)
- **jsonschema/v6**: clean fit. `AddResource`/`Compile` drop in for file
  loading; `RegisterVocabulary` is the one genuinely new piece. Error model is
  richer than a 211-line hand-rolled validator needs — start with `.Error()`,
  grow into `DetailedOutput()` if path+message granularity is needed.
- **gitleaks/v8 as library**: strong fit on the literal requirement (`Secret`
  string + column offsets + a ready `Redact` method beats hand-rolling), but
  architecturally heavier than the 353-line file it replaces — 51-module
  transitive graph vs today's presumably-stdlib-only regex+entropy code.
  Whether that weight actually lands in the compiled binary (cobra/viper/
  archives reachability from `detect`/`config`) is the one fact this research
  could not settle and should gate the "adopt as library" call.

Status: DONE_WITH_CONCERNS
Summary: Both libraries' public APIs support the intended replacement —
jsonschema/v6 via `AddResource`+`Compile` (in-memory) and `RegisterVocabulary`+
`SchemaExt` (custom `x-content-gated` keyword); gitleaks/v8 via
`DetectString`->`[]report.Finding` giving `Secret`+column offsets and a
built-in `Redact` method for in-memory secret redaction. Main open risk is
whether gitleaks pulls cobra/viper/archives weight into the actual binary.
Concerns:
- All API facts sourced from AI-summarized pkg.go.dev fetches (one page per
  fact), not raw source diffed against v6.0.3/v8.30.1 exactly — the pkg.go.dev
  URLs used were unpinned (show latest version); drift from the pinned target
  versions is possible and unchecked.
- The `ExtCompiler` name hypothesized in the task did not surface in the
  fetched docs — flagging in case there's a reason to expect it (different
  version, or an API layer this pass didn't reach).

Unresolved questions:
1. Confirm fetched jsonschema docs correspond to v6.0.3 exactly (pin `@v6.0.3`
   in the pkg.go.dev URL, or check after `go get`).
2. Confirm fetched gitleaks docs (`detect`/`report`/`config`) correspond to
   v8.30.1 exactly (same pinning issue).
3. Exact `ValidatorContext` method used inside `SchemaExt.Validate` to report a
   failure (`.Error(...)`? panic? a ctx field?) — needed to write the
   `x-content-gated` validator body.
4. Exact semantics of `Finding.Redact(percent uint)` — full replace vs
   percentage-masked, mutates in place or returns a copy.
5. Whether `cobra`/`viper`/`mholt/archives` are actually reachable (imported)
   from `detect`/`report`/`config` — decides real dependency-graph impact vs
   go.mod's superset.
6. Whether default `gitleaks.toml` rules include entropy-only (no regex)
   generic detection, or are always regex+entropy hybrid — affects
   false-positive tuning expectations vs the current hand-rolled detector.
7. `jsonschema/v6`'s own go.mod minimum Go version (not checked) — expected
   well under the 1.27 floor but unconfirmed.
