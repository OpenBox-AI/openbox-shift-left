package observation

import (
	"bytes"
	"embed"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// schemaFiles is the runtime copy of the public Phase 2 contract. The parity
// test binds these bytes to contracts/project-observation so verification never
// depends on the caller's checkout or current working directory.
//
//go:embed schema/*.json
var schemaFiles embed.FS

type schemaDefinition struct {
	name string
	id   string
}

var schemaDefinitions = [...]schemaDefinition{
	{name: "manifest.schema.json", id: ManifestSchema},
	{name: "run.schema.json", id: RunSchema},
	{name: "backend.schema.json", id: BackendSchema},
	{name: "openshell-record.schema.json", id: OpenShellRecordSchema},
	{name: "effects.schema.json", id: EffectsSchema},
	{name: "behavior.schema.json", id: BehaviorSchema},
	{name: "coverage.schema.json", id: CoverageSchema},
}

var (
	compiledSchemasOnce sync.Once
	compiledSchemas     map[string]*jsonschema.Schema
	compiledSchemasErr  error
)

func observationSchemas() (map[string]*jsonschema.Schema, error) {
	compiledSchemasOnce.Do(func() {
		compiler := jsonschema.NewCompiler()
		compiler.DefaultDraft(jsonschema.Draft2020)
		compiler.AssertFormat()
		compiledSchemas = make(map[string]*jsonschema.Schema, len(schemaDefinitions))
		for _, definition := range schemaDefinitions {
			content, err := schemaFiles.ReadFile("schema/" + definition.name)
			if err != nil {
				compiledSchemasErr = fmt.Errorf("observation: read embedded schema %q: %w", definition.id, err)
				return
			}
			document, err := jsonschema.UnmarshalJSON(bytes.NewReader(content))
			if err != nil {
				compiledSchemasErr = fmt.Errorf("observation: decode embedded schema %q: %w", definition.id, err)
				return
			}
			if err := compiler.AddResource(definition.name, document); err != nil {
				compiledSchemasErr = fmt.Errorf("observation: register embedded schema %q: %w", definition.id, err)
				return
			}
		}
		for _, definition := range schemaDefinitions {
			schema, err := compiler.Compile(definition.name)
			if err != nil {
				compiledSchemasErr = fmt.Errorf("observation: compile embedded schema %q: %w", definition.id, err)
				return
			}
			compiledSchemas[definition.id] = schema
		}
	})
	return compiledSchemas, compiledSchemasErr
}

func validateSchema(identifier string, content []byte) error {
	schemas, err := observationSchemas()
	if err != nil {
		return err
	}
	schema := schemas[identifier]
	if schema == nil {
		return fmt.Errorf("observation: unknown embedded schema %q", identifier)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("observation: decode %q document: %w", identifier, err)
	}
	if err := schema.Validate(document); err != nil {
		return fmt.Errorf("observation: %q schema validation failed: %w", identifier, err)
	}
	return nil
}
