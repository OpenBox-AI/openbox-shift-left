package corpusfixture

import (
	"encoding/json"
	"regexp"
)

var blockTypeRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// typeNamedField is the field a block of each known type must carry. An
// unknown type is accepted, because a provider adding a block type must not
// fail this gate.
var typeNamedField = map[string]string{
	"text":        "text",
	"thinking":    "thinking",
	"tool_use":    "input",
	"tool_result": "content",
}

// malformedBlocks reports content blocks in a recorded request body whose
// shape no provider would produce. It is deliberately a different kind of rule
// from the substitution check beside it.
func malformedBlocks(body string) []string {
	var top struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal([]byte(body), &top) != nil || len(top.Messages) == 0 {
		return nil
	}
	var out []string
	for _, m := range top.Messages {
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue
		}
		for _, blk := range blocks {
			var kind string
			if json.Unmarshal(blk["type"], &kind) != nil {
				out = append(out, "content block with no type")
				continue
			}
			if !blockTypeRe.MatchString(kind) {
				out = append(out, "content block whose type is not an identifier")
				continue
			}
			if field, known := typeNamedField[kind]; known {
				if _, ok := blk[field]; !ok {
					out = append(out, "content block of type "+kind+" with no "+field)
				}
			}
		}
	}
	return out
}
