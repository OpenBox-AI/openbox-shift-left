package artifact

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

const maxManifestInteger = int64(9007199254740991)

// Role is one closed logical object role in an openbox.audit-pack/v1 manifest.
type Role string

const (
	RoleProjectSnapshot Role = "project-snapshot"
	RoleProjectModel    Role = "project-model"
	RoleRunProfile      Role = "run-profile"
	RoleSDKCoverage     Role = "sdk-coverage"
	RoleSandboxPosture  Role = "sandbox-posture"
	RoleScenarios       Role = "scenarios"
	RoleSDKEvents       Role = "sdk-events"
	RoleFixtureEvents   Role = "fixture-events"
	RoleEffectEvents    Role = "effect-events"
	RoleJudgments       Role = "judgments"
	RoleCleanupReceipt  Role = "cleanup-receipt"
	RoleReportJSON      Role = "report-json"
	RoleReportMarkdown  Role = "report-markdown"
	RoleReportSARIF     Role = "report-sarif"
	RolePolicyProposals Role = "policy-proposals"
)

var requiredRoles = []Role{
	RoleProjectSnapshot,
	RoleProjectModel,
	RoleRunProfile,
	RoleSDKCoverage,
	RoleSandboxPosture,
	RoleScenarios,
	RoleSDKEvents,
	RoleFixtureEvents,
	RoleEffectEvents,
	RoleJudgments,
	RoleCleanupReceipt,
	RoleReportJSON,
	RoleReportMarkdown,
	RoleReportSARIF,
}

var roleSchemas = map[Role]*string{
	RoleProjectSnapshot: nil,
	RoleProjectModel:    stringPointer("openbox.project-model/v1"),
	RoleRunProfile:      stringPointer("openbox.project-run-profile/v1"),
	RoleSDKCoverage:     stringPointer("openbox.sdk-coverage/v1"),
	RoleSandboxPosture:  stringPointer("openbox.sandbox-posture/v1"),
	RoleScenarios:       stringPointer("openbox.security-test/v1"),
	RoleSDKEvents:       nil,
	RoleFixtureEvents:   nil,
	RoleEffectEvents:    nil,
	RoleJudgments:       nil,
	RoleCleanupReceipt:  nil,
	RoleReportJSON:      nil,
	RoleReportMarkdown:  nil,
	RoleReportSARIF:     nil,
	RolePolicyProposals: stringPointer("openbox.policy-proposal/v1"),
}

var roleMediaTypes = map[Role]string{
	RoleProjectSnapshot: "application/vnd.openbox.project-snapshot",
	RoleProjectModel:    "application/json",
	RoleRunProfile:      "application/json",
	RoleSDKCoverage:     "application/json",
	RoleSandboxPosture:  "application/json",
	RoleScenarios:       "application/x-ndjson",
	RoleSDKEvents:       "application/x-ndjson",
	RoleFixtureEvents:   "application/x-ndjson",
	RoleEffectEvents:    "application/x-ndjson",
	RoleJudgments:       "application/json",
	RoleCleanupReceipt:  "application/json",
	RoleReportJSON:      "application/json",
	RoleReportMarkdown:  "text/markdown",
	RoleReportSARIF:     "application/sarif+json",
	RolePolicyProposals: "application/x-ndjson",
}

var schemaIDs = [...]string{
	"openbox.project-model/v1",
	"openbox.project-run-profile/v1",
	"openbox.sdk-coverage/v1",
	"openbox.sandbox-posture/v1",
	"openbox.security-test/v1",
	"openbox.audit-pack/v1",
	"openbox.policy-proposal/v1",
}

// SchemaReference binds one public schema ID to the digest of its exact bytes.
type SchemaReference struct {
	ID     string        `json:"id"`
	Digest ContentDigest `json:"digest"`
}

// Object is one immutable content-addressed pack object. Bytes returns a copy;
// absolute paths, timestamps, process IDs, and other volatile run data must not
// be embedded in normalized objects.
type Object struct {
	role      Role
	mediaType string
	schema    *string
	retention string
	bytes     []byte
	digest    ContentDigest
}

// NewCanonicalObject creates an object from RFC 8785 canonical JSON.
func NewCanonicalObject(
	role Role,
	mediaType string,
	schema *string,
	retention string,
	value any,
) (Object, error) {
	expectedMediaType, known := roleMediaTypes[role]
	if !known {
		return Object{}, fmt.Errorf("artifact: unknown audit-pack role %q", role)
	}
	if expectedMediaType == "application/x-ndjson" || expectedMediaType == "text/markdown" {
		return Object{}, fmt.Errorf("artifact: role %q requires exact non-JSON-object bytes", role)
	}
	content, err := CanonicalJSON(value)
	if err != nil {
		return Object{}, err
	}
	return newObject(role, mediaType, schema, retention, content)
}

// NewExactObject creates an object whose identity covers the exact supplied
// bytes. Use this for canonical JSONL and non-JSON projections.
func NewExactObject(
	role Role,
	mediaType string,
	schema *string,
	retention string,
	content []byte,
) (Object, error) {
	return newObject(role, mediaType, schema, retention, content)
}

func newObject(
	role Role,
	mediaType string,
	schema *string,
	retention string,
	content []byte,
) (Object, error) {
	expectedSchema, known := roleSchemas[role]
	if !known {
		return Object{}, fmt.Errorf("artifact: unknown audit-pack role %q", role)
	}
	if !sameOptionalString(schema, expectedSchema) {
		return Object{}, fmt.Errorf("artifact: role %q has schema %s, want %s", role, optionalString(schema), optionalString(expectedSchema))
	}
	if mediaType != roleMediaTypes[role] {
		return Object{}, fmt.Errorf("artifact: role %q has media type %q, want %q", role, mediaType, roleMediaTypes[role])
	}
	switch retention {
	case "normalized", "redacted", "public_projection":
	case "digest_only", "omitted":
		return Object{}, errors.New("artifact: payload-bearing objects cannot use digest_only or omitted retention")
	default:
		return Object{}, fmt.Errorf("artifact: invalid retention %q", retention)
	}
	if int64(len(content)) > maxManifestInteger {
		return Object{}, errors.New("artifact: object exceeds the v1 integer bound")
	}
	if err := validateObjectEncoding(role, content); err != nil {
		return Object{}, err
	}
	copyOfContent := append([]byte(nil), content...)
	return Object{
		role:      role,
		mediaType: mediaType,
		schema:    cloneStringPointer(schema),
		retention: retention,
		bytes:     copyOfContent,
		digest:    DigestBytes(copyOfContent),
	}, nil
}

func validateObjectEncoding(role Role, content []byte) error {
	switch roleMediaTypes[role] {
	case "application/x-ndjson":
		return validateCanonicalJSONLines(content)
	case "text/markdown":
		if !utf8.Valid(content) {
			return fmt.Errorf("artifact: role %q is not valid UTF-8", role)
		}
		return nil
	default:
		canonical, err := CanonicalizeJSON(content)
		if err != nil {
			return fmt.Errorf("artifact: role %q is not valid JSON: %w", role, err)
		}
		if !bytes.Equal(canonical, content) {
			return fmt.Errorf("artifact: role %q JSON bytes are not canonical", role)
		}
		return nil
	}
}

func validateCanonicalJSONLines(content []byte) error {
	if len(content) == 0 {
		return nil
	}
	if content[len(content)-1] != '\n' {
		return errors.New("artifact: JSONL content must end with LF")
	}
	lines := bytes.Split(content[:len(content)-1], []byte{'\n'})
	for index, line := range lines {
		if len(line) == 0 {
			return fmt.Errorf("artifact: JSONL record %d is empty", index+1)
		}
		canonical, err := CanonicalizeJSON(line)
		if err != nil {
			return fmt.Errorf("artifact: JSONL record %d is invalid: %w", index+1, err)
		}
		if !bytes.Equal(canonical, line) {
			return fmt.Errorf("artifact: JSONL record %d is not canonical", index+1)
		}
	}
	return nil
}

// Role returns the object's logical manifest role.
func (object Object) Role() Role { return object.role }

// Digest returns the content ID of the exact object bytes.
func (object Object) Digest() ContentDigest { return object.digest }

// Bytes returns a defensive copy of the exact object bytes.
func (object Object) Bytes() []byte { return append([]byte(nil), object.bytes...) }

// ManifestInput supplies the volatile root fields and normalized pack objects.
// RunID and RunnerVersion affect only manifest identity, never object identity.
type ManifestInput struct {
	RunID         string
	Mode          string
	Schemas       []SchemaReference
	Objects       ManifestObjects
	Judgments     any
	Limits        any
	RunnerVersion string
	Runtime       RuntimeEnvelope
}

// RuntimeEnvelope is volatile in-memory provenance. It is deliberately not
// part of manifest.json or any normalized object, so host paths and collection
// time cannot perturb content identity or leak into a retained audit pack.
type RuntimeEnvelope struct {
	CapturedAt   string
	SnapshotRoot string
}

// ManifestObjects names every non-judgment object required by v1. The
// judgments object is deliberately absent: AssemblePack derives it from the
// authoritative inline Judgments value so the two representations cannot
// disagree.
type ManifestObjects struct {
	ProjectSnapshot Object
	ProjectModel    Object
	RunProfile      Object
	SDKCoverage     Object
	SandboxPosture  Object
	Scenarios       Object
	SDKEvents       Object
	FixtureEvents   Object
	EffectEvents    Object
	CleanupReceipt  Object
	ReportJSON      Object
	ReportMarkdown  Object
	ReportSARIF     Object
	PolicyProposals *Object
}

// Pack is a fully assembled canonical manifest plus its immutable objects.
// Schema validation is a separate contract boundary; AssemblePack enforces the
// role, schema, content-ID, and inline-judgment correlations that assembly owns.
type Pack struct {
	manifest []byte
	digest   ContentDigest
	objects  []Object
	runtime  RuntimeEnvelope
}

// AssemblePack builds canonical openbox.audit-pack/v1 manifest bytes.
func AssemblePack(input ManifestInput) (*Pack, error) {
	if err := validateManifestID(input.RunID); err != nil {
		return nil, err
	}
	if input.Mode != "baseline" && input.Mode != "governed" {
		return nil, fmt.Errorf("artifact: invalid audit-pack mode %q", input.Mode)
	}
	if input.RunnerVersion == "" || len(input.RunnerVersion) > 128 || !utf8.ValidString(input.RunnerVersion) {
		return nil, errors.New("artifact: runner version is invalid")
	}
	if input.Runtime.CapturedAt == "" || !utf8.ValidString(input.Runtime.CapturedAt) ||
		input.Runtime.SnapshotRoot == "" || !utf8.ValidString(input.Runtime.SnapshotRoot) {
		return nil, errors.New("artifact: runtime envelope is invalid")
	}
	if err := validateSchemas(input.Schemas); err != nil {
		return nil, err
	}

	judgments, err := CanonicalJSON(input.Judgments)
	if err != nil {
		return nil, fmt.Errorf("artifact: canonicalize judgments: %w", err)
	}
	judgmentObject, err := newObject(RoleJudgments, "application/json", nil, "normalized", judgments)
	if err != nil {
		return nil, fmt.Errorf("artifact: assemble judgments object: %w", err)
	}
	stored := []Object{
		input.Objects.ProjectSnapshot,
		input.Objects.ProjectModel,
		input.Objects.RunProfile,
		input.Objects.SDKCoverage,
		input.Objects.SandboxPosture,
		input.Objects.Scenarios,
		input.Objects.SDKEvents,
		input.Objects.FixtureEvents,
		input.Objects.EffectEvents,
		judgmentObject,
		input.Objects.CleanupReceipt,
		input.Objects.ReportJSON,
		input.Objects.ReportMarkdown,
		input.Objects.ReportSARIF,
	}
	if input.Objects.PolicyProposals != nil {
		stored = append(stored, input.Objects.PolicyProposals.clone())
	}
	objects := make(map[Role]objectReference, len(stored))
	expectedRoles := requiredRoles
	if input.Objects.PolicyProposals != nil {
		expectedRoles = append(append([]Role(nil), requiredRoles...), RolePolicyProposals)
	}
	for index, object := range stored {
		expected := expectedRoles[index]
		if object.role != expected {
			return nil, fmt.Errorf("artifact: manifest field for %q contains role %q", expected, object.role)
		}
		if object.digest != DigestBytes(object.bytes) {
			return nil, fmt.Errorf("artifact: object %q content digest changed", object.role)
		}
		objects[object.role] = object.reference()
	}

	manifest := auditManifest{
		APIVersion: "openbox.audit-pack/v1",
		Kind:       "AuditPack",
		RunID:      input.RunID,
		Mode:       input.Mode,
		Schemas:    append([]SchemaReference(nil), input.Schemas...),
		Objects:    objects,
		Judgments:  input.Judgments,
		Limits:     input.Limits,
		Provenance: manifestProvenance{Runner: manifestRunner{Name: "openbox", Version: input.RunnerVersion}},
		Retention:  manifestRetention{Mode: "redacted_digests", RawContentPersisted: false},
	}
	canonical, digest, err := DigestCanonicalJSON(manifest)
	if err != nil {
		return nil, fmt.Errorf("artifact: canonicalize audit-pack manifest: %w", err)
	}
	sort.Slice(stored, func(left, right int) bool { return stored[left].role < stored[right].role })
	return &Pack{manifest: canonical, digest: digest, objects: stored, runtime: input.Runtime}, nil
}

// Manifest returns a defensive copy of the canonical manifest bytes.
func (pack *Pack) Manifest() []byte {
	if pack == nil {
		return nil
	}
	return append([]byte(nil), pack.manifest...)
}

// Digest returns sha256(JCS(manifest.json)).
func (pack *Pack) Digest() ContentDigest {
	if pack == nil {
		return ContentDigest{}
	}
	return pack.digest
}

// Objects returns deep copies sorted by logical role.
func (pack *Pack) Objects() []Object {
	if pack == nil {
		return nil
	}
	result := make([]Object, len(pack.objects))
	for index, object := range pack.objects {
		result[index] = object.clone()
	}
	return result
}

// RuntimeEnvelope returns volatile provenance kept outside retained pack bytes.
func (pack *Pack) RuntimeEnvelope() RuntimeEnvelope {
	if pack == nil {
		return RuntimeEnvelope{}
	}
	return pack.runtime
}

type objectReference struct {
	CID       ContentDigest `json:"cid"`
	MediaType string        `json:"mediaType"`
	Schema    *string       `json:"schema"`
	Bytes     int64         `json:"bytes"`
	Retention string        `json:"retention"`
}

type auditManifest struct {
	APIVersion string                   `json:"apiVersion"`
	Kind       string                   `json:"kind"`
	RunID      string                   `json:"runId"`
	Mode       string                   `json:"mode"`
	Schemas    []SchemaReference        `json:"schemas"`
	Objects    map[Role]objectReference `json:"objects"`
	Judgments  any                      `json:"judgments"`
	Limits     any                      `json:"limits"`
	Provenance manifestProvenance       `json:"provenance"`
	Retention  manifestRetention        `json:"retention"`
}

type manifestProvenance struct {
	Runner manifestRunner `json:"runner"`
}

type manifestRunner struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type manifestRetention struct {
	Mode                string `json:"mode"`
	RawContentPersisted bool   `json:"rawContentPersisted"`
}

func (object Object) reference() objectReference {
	return objectReference{
		CID:       object.digest,
		MediaType: object.mediaType,
		Schema:    cloneStringPointer(object.schema),
		Bytes:     int64(len(object.bytes)),
		Retention: object.retention,
	}
}

func (object Object) clone() Object {
	object.schema = cloneStringPointer(object.schema)
	object.bytes = append([]byte(nil), object.bytes...)
	return object
}

func validateSchemas(schemas []SchemaReference) error {
	if len(schemas) != len(schemaIDs) {
		return fmt.Errorf("artifact: schema inventory has %d entries, want %d", len(schemas), len(schemaIDs))
	}
	for index, expected := range schemaIDs {
		if schemas[index].ID != expected {
			return fmt.Errorf("artifact: schema inventory entry %d is %q, want %q", index, schemas[index].ID, expected)
		}
	}
	return nil
}

func validateManifestID(identifier string) error {
	if identifier == "" || len(identifier) > 256 || !utf8.ValidString(identifier) {
		return errors.New("artifact: run ID is invalid")
	}
	for index, character := range identifier {
		valid := 'A' <= character && character <= 'Z' ||
			'a' <= character && character <= 'z' ||
			'0' <= character && character <= '9' ||
			(index > 0 && strings.ContainsRune("._:-", character))
		if !valid {
			return fmt.Errorf("artifact: run ID %q is invalid", identifier)
		}
	}
	return nil
}

func sameOptionalString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func optionalString(value *string) string {
	if value == nil {
		return "null"
	}
	return fmt.Sprintf("%q", *value)
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	copyOfValue := *value
	return &copyOfValue
}

func stringPointer(value string) *string { return &value }
