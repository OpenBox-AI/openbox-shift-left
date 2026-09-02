package artifact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

// ManifestReference is one validated logical-role reference from a canonical
// audit-pack manifest.
type ManifestReference struct {
	role      Role
	cid       ContentDigest
	mediaType string
	schema    *string
	bytes     int64
	retention string
}

func (reference ManifestReference) Role() Role         { return reference.role }
func (reference ManifestReference) CID() ContentDigest { return reference.cid }
func (reference ManifestReference) MediaType() string  { return reference.mediaType }
func (reference ManifestReference) Schema() *string    { return cloneStringPointer(reference.schema) }
func (reference ManifestReference) Bytes() int64       { return reference.bytes }
func (reference ManifestReference) Retention() string  { return reference.retention }

// ManifestIndex is an immutable structural index of one canonical v1 root.
// Public-schema validation remains a separate contract boundary.
type ManifestIndex struct {
	manifest   []byte
	digest     ContentDigest
	references []ManifestReference
}

func (index *ManifestIndex) Manifest() []byte {
	if index == nil {
		return nil
	}
	return append([]byte(nil), index.manifest...)
}

func (index *ManifestIndex) Digest() ContentDigest {
	if index == nil {
		return ContentDigest{}
	}
	return index.digest
}

func (index *ManifestIndex) References() []ManifestReference {
	if index == nil {
		return nil
	}
	result := append([]ManifestReference(nil), index.references...)
	for offset := range result {
		result[offset].schema = cloneStringPointer(result[offset].schema)
	}
	return result
}

func (index *ManifestIndex) VerifyObject(reference ManifestReference, content []byte) error {
	if index == nil {
		return errors.New("artifact: nil manifest index")
	}
	found := false
	for _, expected := range index.references {
		if expected.role == reference.role && expected.cid == reference.cid && expected.bytes == reference.bytes {
			found = true
			break
		}
	}
	if !found {
		return errors.New("artifact: object reference is not in the manifest")
	}
	if int64(len(content)) != reference.bytes {
		return fmt.Errorf("artifact: object %q byte length changed", reference.role)
	}
	if DigestBytes(content) != reference.cid {
		return fmt.Errorf("artifact: object %q content digest changed", reference.role)
	}
	return validateObjectEncoding(reference.role, content)
}

type readerManifest struct {
	APIVersion string                         `json:"apiVersion"`
	Kind       string                         `json:"kind"`
	RunID      string                         `json:"runId"`
	Mode       string                         `json:"mode"`
	Schemas    []readerSchemaReference        `json:"schemas"`
	Objects    map[Role]readerObjectReference `json:"objects"`
	Judgments  json.RawMessage                `json:"judgments"`
	Limits     json.RawMessage                `json:"limits"`
	Provenance readerManifestProvenance       `json:"provenance"`
	Retention  readerManifestRetention        `json:"retention"`
}

type readerObjectReference struct {
	CID            *ContentDigest  `json:"cid"`
	MediaType      string          `json:"mediaType"`
	Schema         json.RawMessage `json:"schema"`
	Bytes          *int64          `json:"bytes"`
	Retention      string          `json:"retention"`
	OmissionReason *string         `json:"omissionReason,omitempty"`
}

type readerManifestProvenance struct {
	Runner readerManifestRunner `json:"runner"`
}

type readerManifestRunner struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type readerManifestRetention struct {
	Mode                string `json:"mode"`
	RawContentPersisted *bool  `json:"rawContentPersisted"`
}

type readerSchemaReference struct {
	ID     string         `json:"id"`
	Digest *ContentDigest `json:"digest"`
}

// ParseManifestIndex validates canonical JSON plus the assembly-owned v1
// inventory, role/media/schema/retention map, and inline-judgment object bind.
func ParseManifestIndex(manifest []byte) (*ManifestIndex, error) {
	canonical, err := CanonicalizeJSON(manifest)
	if err != nil {
		return nil, fmt.Errorf("artifact: invalid manifest JSON: %w", err)
	}
	if !bytes.Equal(canonical, manifest) {
		return nil, errors.New("artifact: manifest is not canonical JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(manifest))
	decoder.DisallowUnknownFields()
	var parsed readerManifest
	if err := decoder.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("artifact: decode manifest: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, err
	}
	if parsed.APIVersion != "openbox.audit-pack/v1" || parsed.Kind != "AuditPack" {
		return nil, errors.New("artifact: manifest identity is invalid")
	}
	if err := validateManifestID(parsed.RunID); err != nil {
		return nil, err
	}
	if parsed.Mode != "baseline" && parsed.Mode != "governed" {
		return nil, errors.New("artifact: manifest mode is invalid")
	}
	schemas := make([]SchemaReference, len(parsed.Schemas))
	for index, schema := range parsed.Schemas {
		if schema.Digest == nil {
			return nil, errors.New("artifact: schema reference digest is missing")
		}
		schemas[index] = SchemaReference{ID: schema.ID, Digest: *schema.Digest}
	}
	if err := validateSchemas(schemas); err != nil {
		return nil, err
	}
	if parsed.Provenance.Runner.Name != "openbox" || parsed.Provenance.Runner.Version == "" || len(parsed.Provenance.Runner.Version) > 128 {
		return nil, errors.New("artifact: manifest runner is invalid")
	}
	if parsed.Retention.Mode != "redacted_digests" || parsed.Retention.RawContentPersisted == nil || *parsed.Retention.RawContentPersisted {
		return nil, errors.New("artifact: manifest retention is invalid")
	}
	if !isJSONObject(parsed.Limits) || !isJSONArray(parsed.Judgments) {
		return nil, errors.New("artifact: manifest judgments or limits have the wrong shape")
	}

	expectedRoles := append([]Role(nil), requiredRoles...)
	if _, optional := parsed.Objects[RolePolicyProposals]; optional {
		expectedRoles = append(expectedRoles, RolePolicyProposals)
	}
	if len(parsed.Objects) != len(expectedRoles) {
		return nil, errors.New("artifact: manifest object role inventory is incomplete or unknown")
	}
	references := make([]ManifestReference, 0, len(expectedRoles))
	for _, role := range expectedRoles {
		reference, ok := parsed.Objects[role]
		if !ok {
			return nil, fmt.Errorf("artifact: manifest is missing role %q", role)
		}
		schema, err := validateReaderReference(role, reference)
		if err != nil {
			return nil, err
		}
		references = append(references, ManifestReference{
			role: role, cid: *reference.CID, mediaType: reference.MediaType,
			schema: schema, bytes: *reference.Bytes, retention: reference.Retention,
		})
	}
	judgments := parsed.Objects[RoleJudgments]
	if judgments.Bytes == nil || judgments.CID == nil || *judgments.Bytes != int64(len(parsed.Judgments)) || *judgments.CID != DigestBytes(parsed.Judgments) {
		return nil, errors.New("artifact: inline judgments do not match the judgments object")
	}
	sort.Slice(references, func(left, right int) bool { return references[left].role < references[right].role })
	return &ManifestIndex{manifest: append([]byte(nil), manifest...), digest: DigestBytes(manifest), references: references}, nil
}

func validateReaderReference(role Role, reference readerObjectReference) (*string, error) {
	if reference.CID == nil || reference.Bytes == nil || len(reference.Schema) == 0 {
		return nil, fmt.Errorf("artifact: role %q reference is missing a required field", role)
	}
	var schema *string
	if !bytes.Equal(reference.Schema, []byte("null")) {
		var value string
		if err := json.Unmarshal(reference.Schema, &value); err != nil || value == "" || len(value) > 256 {
			return nil, fmt.Errorf("artifact: role %q schema is invalid", role)
		}
		schema = &value
	}
	if reference.MediaType != roleMediaTypes[role] {
		return nil, fmt.Errorf("artifact: role %q media type is invalid", role)
	}
	if !sameOptionalString(schema, roleSchemas[role]) {
		return nil, fmt.Errorf("artifact: role %q schema is invalid", role)
	}
	if *reference.Bytes < 0 || *reference.Bytes > maxManifestInteger {
		return nil, fmt.Errorf("artifact: role %q byte length is invalid", role)
	}
	if reference.Retention != "normalized" && reference.Retention != "redacted" && reference.Retention != "public_projection" {
		return nil, fmt.Errorf("artifact: role %q retention is not a persisted v1 payload", role)
	}
	if reference.OmissionReason != nil {
		return nil, fmt.Errorf("artifact: persisted role %q has an omission reason", role)
	}
	return schema, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("artifact: manifest has trailing JSON")
		}
		return fmt.Errorf("artifact: finish manifest decode: %w", err)
	}
	return nil
}

func isJSONObject(raw json.RawMessage) bool { return len(raw) > 1 && raw[0] == '{' }
func isJSONArray(raw json.RawMessage) bool  { return len(raw) > 1 && raw[0] == '[' }
