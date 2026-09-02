package report

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dlclark/regexp2"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const regexpMatchTimeout = 25 * time.Millisecond

//go:embed schema/*.json
var schemaFiles embed.FS

type schemaDefinition struct {
	id       string
	filename string
	role     artifact.Role
}

var schemaDefinitions = [...]schemaDefinition{
	{id: "openbox.project-model/v1", filename: "project-model-v1.schema.json", role: artifact.RoleProjectModel},
	{id: "openbox.project-run-profile/v1", filename: "project-run-profile-v1.schema.json", role: artifact.RoleRunProfile},
	{id: "openbox.sdk-coverage/v1", filename: "sdk-coverage-v1.schema.json", role: artifact.RoleSDKCoverage},
	{id: "openbox.sandbox-posture/v1", filename: "sandbox-posture-v1.schema.json", role: artifact.RoleSandboxPosture},
	{id: "openbox.security-test/v1", filename: "security-test-v1.schema.json", role: artifact.RoleScenarios},
	{id: "openbox.audit-pack/v1", filename: "audit-pack-v1.schema.json"},
	{id: "openbox.policy-proposal/v1", filename: "policy-proposal-v1.schema.json", role: artifact.RolePolicyProposals},
}

type contractSet struct {
	digests map[string]artifact.ContentDigest
	schemas map[string]*jsonschema.Schema
}

var (
	contractsOnce  sync.Once
	contractsValue *contractSet
	contractsError error
)

// SchemaReferences returns the exact compiled v1 schema inventory in manifest
// order. The returned slice is independent of the package-owned bundle.
func SchemaReferences() ([]artifact.SchemaReference, error) {
	contracts, err := compiledContracts()
	if err != nil {
		return nil, err
	}
	result := make([]artifact.SchemaReference, len(schemaDefinitions))
	for index, definition := range schemaDefinitions {
		result[index] = artifact.SchemaReference{ID: definition.id, Digest: contracts.digests[definition.id]}
	}
	return result, nil
}

func compiledContracts() (*contractSet, error) {
	contractsOnce.Do(func() {
		contractsValue, contractsError = compileContracts()
	})
	return contractsValue, contractsError
}

func compileContracts() (*contractSet, error) {
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(jsonschema.SchemeURLLoader{})
	compiler.UseRegexpEngine(compileECMARegexp)
	result := &contractSet{
		digests: make(map[string]artifact.ContentDigest, len(schemaDefinitions)),
		schemas: make(map[string]*jsonschema.Schema, len(schemaDefinitions)),
	}
	for _, definition := range schemaDefinitions {
		content, err := schemaFiles.ReadFile("schema/" + definition.filename)
		if err != nil {
			return nil, fmt.Errorf("report: read compiled schema %q: %w", definition.id, err)
		}
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(content))
		if err != nil {
			return nil, fmt.Errorf("report: decode compiled schema %q: %w", definition.id, err)
		}
		if err := compiler.AddResource(definition.filename, document); err != nil {
			return nil, fmt.Errorf("report: register compiled schema %q: %w", definition.id, err)
		}
		result.digests[definition.id] = artifact.DigestBytes(content)
	}
	for _, definition := range schemaDefinitions {
		schema, err := compiler.Compile(definition.filename)
		if err != nil {
			return nil, fmt.Errorf("report: compile schema %q: %w", definition.id, err)
		}
		result.schemas[definition.id] = schema
	}
	return result, nil
}

type boundedRegexp struct{ expression *regexp2.Regexp }

func compileECMARegexp(pattern string) (jsonschema.Regexp, error) {
	expression, err := regexp2.Compile(pattern, regexp2.ECMAScript)
	if err != nil {
		return nil, err
	}
	expression.MatchTimeout = regexpMatchTimeout
	return boundedRegexp{expression: expression}, nil
}

func (expression boundedRegexp) String() string { return expression.expression.String() }

func (expression boundedRegexp) MatchString(value string) bool {
	matched, err := expression.expression.MatchString(value)
	return err == nil && matched
}

func (contracts *contractSet) validate(identifier string, content []byte) error {
	if contracts == nil {
		return errors.New("report: compiled contracts are unavailable")
	}
	schema := contracts.schemas[identifier]
	if schema == nil {
		return fmt.Errorf("report: unknown schema %q", identifier)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("report: decode %q document: %w", identifier, err)
	}
	if err := schema.Validate(document); err != nil {
		return fmt.Errorf("report: %q validation failed: %w", identifier, err)
	}
	return nil
}
