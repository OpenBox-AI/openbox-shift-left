package conformance

import (
	"fmt"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// schemaResourceURL is the in-memory identity the schema document is compiled
// under. Any URL works — the point is that AddResource pins the document, so
// the compiler resolves it from memory and never reaches for the network.
//
// Deliberately NOT the schema's own `$id` (an https:// URL). Compiling under
// `$id` works too, but naming a fetchable URL here would make an accidental
// removal of the refusing loader look harmless: the compiler would silently
// start resolving over the network and still pass. A mem:// scheme cannot be
// fetched by anything, so that mistake fails loudly instead.
const schemaResourceURL = "mem://openbox/dev-event.schema.json"

// refusingLoader is the compiler's only loader, and it always fails.
//
// A conformance run that reaches the network can be influenced from off-host,
// and CI is where that would happen. AddResource already pins the document, so
// every load attempt is by definition something we did not intend — a `$ref` to
// a URL that is not the pinned one, or a meta-schema the library could not
// satisfy from its built-ins. Failing beats fetching.
type refusingLoader struct{}

func (refusingLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("conformance: refused to fetch %q — the schema is pinned in memory and must resolve entirely from it", url)
}

// compileSchema compiles the contract schema for structural validation.
//
// Two settings are load-bearing:
//
// AssertFormat: in draft/2020-12 `format` is an ANNOTATION by default, so
// `"format": "date-time"` would be parsed, reported as satisfied, and enforce
// nothing. The retired hand-rolled validator did assert date-time, so without
// this the swap would silently loosen the contract — testdata/invalid/
// bad_timestamp.json is the case that proves it either way.
//
// UseLoader: see refusingLoader.
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
