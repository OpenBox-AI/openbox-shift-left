package conformance

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// ErrContentDisabled is returned when an event carries content (a populated
// x-content-gated field) while content-capture is disabled (INV-2).
var ErrContentDisabled = errors.New("event carries content while content-capture is disabled (INV-2)")

// ValidateDevEvent validates a raw normalized developer-runtime event against
// the dev-event contract.
func ValidateDevEvent(raw []byte, contentCaptureEnabled bool) error {
	sch, schema, err := contractSchema()
	if err != nil {
		return err
	}

	var inst any
	if err := json.Unmarshal(raw, &inst); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}

	var errs []string
	if err := sch.Validate(inst); err != nil {
		errs = append(errs, err.Error())
	}

	if missingMCPServer(inst) {
		errs = append(errs, "$.tool: mcp_server is required when kind=mcp")
	}

	if len(errs) > 0 {
		return fmt.Errorf("event is not conformant:\n  - %s", strings.Join(errs, "\n  - "))
	}

	v := &validator{root: schema}
	if !contentCaptureEnabled && v.hasGatedContent(schema, inst) {
		return ErrContentDisabled
	}

	return nil
}

// contractSchema parses and compiles the contract once per process: the
// document is a committed file and every use of it below is read-only.
// Exported LoadSchema stays uncached, so a caller that wants its own copy of
// the document still gets one.
var compiledContract = sync.OnceValue(func() (c compiledSchema) {
	c.doc, c.err = LoadSchema()
	if c.err != nil {
		return c
	}
	c.sch, c.err = compileSchema(c.doc)
	return c
})

type compiledSchema struct {
	sch *jsonschema.Schema
	doc map[string]any
	err error
}

func contractSchema() (*jsonschema.Schema, map[string]any, error) {
	c := compiledContract()
	return c.sch, c.doc, c.err
}

// missingMCPServer the schema cannot express "required when kind=mcp".
func missingMCPServer(inst any) bool {
	obj, ok := inst.(map[string]any)
	if !ok {
		return false
	}
	tool, ok := obj["tool"].(map[string]any)
	if !ok || tool["kind"] != "mcp" {
		return false
	}
	s, ok := tool["mcp_server"].(string)
	return !ok || s == ""
}
