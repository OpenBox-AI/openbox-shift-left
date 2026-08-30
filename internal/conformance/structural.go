package conformance

import (
	"fmt"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// schemaResourceURL any URL works; the point is that AddResource pins the
// document, so the compiler resolves it from memory and never reaches for the
// network.
const schemaResourceURL = "mem://openbox/dev-event.schema.json"

type refusingLoader struct{}

func (refusingLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("conformance: refused to fetch %q — the schema is pinned in memory and must resolve entirely from it", url)
}

// compileSchema two settings are load-bearing:
func compileSchema(doc map[string]any) (*jsonschema.Schema, error) {
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	c.UseLoader(refusingLoader{})
	if err := c.AddResource(schemaResourceURL, doc); err != nil {
		return nil, fmt.Errorf("add schema resource: %w", err)
	}
	sch, err := c.Compile(schemaResourceURL)
	if err != nil {
		return nil, fmt.Errorf("compile schema: %w", err)
	}
	return sch, nil
}
