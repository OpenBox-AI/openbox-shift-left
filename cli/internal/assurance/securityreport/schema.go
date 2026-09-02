package securityreport

import (
	"bytes"
	"embed"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

type schemaDefinition struct {
	id       string
	filename string
}

var phaseFourSchemas = []schemaDefinition{
	{id: ManifestSchema, filename: "manifest.schema.json"},
	{id: RecommendationSchema, filename: "recommendation-catalog.schema.json"},
	{id: ReportSchema, filename: "report.schema.json"},
	{id: "ai.openbox.project-target-posture/v1", filename: "target-posture.schema.json"},
}

//go:embed schema/*.json
var schemaFiles embed.FS

var (
	schemaOnce   sync.Once
	schemaValues map[string]*jsonschema.Schema
	schemaErr    error
)

func validatePhaseFourSchema(identifier string, content []byte) error {
	schemaOnce.Do(compilePhaseFourSchemas)
	if schemaErr != nil {
		return fmt.Errorf("security report: compile public schemas: %w", schemaErr)
	}
	schema, ok := schemaValues[identifier]
	if !ok {
		return fmt.Errorf("security report: unknown public schema %q", identifier)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("security report: decode %s instance: %w", identifier, err)
	}
	if err := schema.Validate(document); err != nil {
		return fmt.Errorf("security report: %s validation failed: %w", identifier, err)
	}
	return nil
}

func compilePhaseFourSchemas() {
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	for _, definition := range phaseFourSchemas {
		content, err := schemaFiles.ReadFile("schema/" + definition.filename)
		if err != nil {
			schemaErr = err
			return
		}
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(content))
		if err != nil {
			schemaErr = err
			return
		}
		if err := compiler.AddResource(definition.filename, document); err != nil {
			schemaErr = err
			return
		}
	}
	schemaValues = make(map[string]*jsonschema.Schema, len(phaseFourSchemas))
	for _, definition := range phaseFourSchemas {
		compiled, err := compiler.Compile(definition.filename)
		if err != nil {
			schemaErr = err
			return
		}
		schemaValues[definition.id] = compiled
	}
}
