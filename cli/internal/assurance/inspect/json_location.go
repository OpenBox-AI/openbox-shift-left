package inspect

import (
	"encoding/json"
	"fmt"
)

type jsonLocationToken struct {
	kind   byte
	value  string
	offset int
}

func lexJSONLocations(content []byte) ([]jsonLocationToken, error) {
	tokens := make([]jsonLocationToken, 0)
	for index := 0; index < len(content); {
		switch content[index] {
		case ' ', '\t', '\r', '\n':
			index++
		case '{', '}', '[', ']', ':', ',':
			tokens = append(tokens, jsonLocationToken{kind: content[index], offset: index})
			index++
		case '"':
			start := index
			index++
			escaped := false
			for index < len(content) {
				if escaped {
					escaped = false
					index++
					continue
				}
				if content[index] == '\\' {
					escaped = true
					index++
					continue
				}
				if content[index] == '"' {
					index++
					break
				}
				index++
			}
			if index > len(content) || index == 0 || content[index-1] != '"' {
				return nil, fmt.Errorf("inspect: unterminated JSON string at byte %d", start)
			}
			var value string
			if err := json.Unmarshal(content[start:index], &value); err != nil {
				return nil, fmt.Errorf("inspect: decode JSON string at byte %d: %w", start, err)
			}
			tokens = append(tokens, jsonLocationToken{kind: 's', value: value, offset: start})
		default:
			start := index
			for index < len(content) {
				switch content[index] {
				case ' ', '\t', '\r', '\n', '{', '}', '[', ']', ':', ',':
					goto primitiveDone
				default:
					index++
				}
			}
		primitiveDone:
			tokens = append(tokens, jsonLocationToken{kind: 'v', offset: start})
		}
	}
	return tokens, nil
}

func jsonObjectMember(tokens []jsonLocationToken, objectIndex int, name string) (jsonLocationToken, int, bool) {
	if objectIndex < 0 || objectIndex >= len(tokens) || tokens[objectIndex].kind != '{' {
		return jsonLocationToken{}, 0, false
	}
	depth := 1
	for index := objectIndex + 1; index+2 < len(tokens) && depth > 0; index++ {
		switch tokens[index].kind {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case 's':
			if depth == 1 && tokens[index].value == name && tokens[index+1].kind == ':' {
				return tokens[index], index + 2, true
			}
		}
	}
	return jsonLocationToken{}, 0, false
}

func jsonDependencyLocation(tokens []jsonLocationToken, section, name string) (int, bool) {
	_, sectionValue, found := jsonObjectMember(tokens, 0, section)
	if !found || sectionValue >= len(tokens) || tokens[sectionValue].kind != '{' {
		return 0, false
	}
	key, _, found := jsonObjectMember(tokens, sectionValue, name)
	return key.offset, found
}

func jsonEntrypointLocation(tokens []jsonLocationToken, field, member, value string) (int, bool) {
	_, fieldValue, found := jsonObjectMember(tokens, 0, field)
	if !found || fieldValue >= len(tokens) {
		return 0, false
	}
	if tokens[fieldValue].kind == 's' {
		return tokens[fieldValue].offset, member == "" && tokens[fieldValue].value == value
	}
	if tokens[fieldValue].kind != '{' {
		return 0, false
	}
	_, entryValue, found := jsonObjectMember(tokens, fieldValue, member)
	if !found || entryValue >= len(tokens) || tokens[entryValue].kind != 's' {
		return 0, false
	}
	return tokens[entryValue].offset, tokens[entryValue].value == value
}
