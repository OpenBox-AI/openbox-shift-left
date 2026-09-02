package inspect

import (
	"errors"
	"strings"
)

type language uint8

const (
	languageJavaScript language = iota + 1
	languageTypeScript
	languagePython
)

type tokenKind uint8

const (
	tokenIdentifier tokenKind = iota + 1
	tokenString
	tokenPunctuation
	tokenOpaque
)

type sourceToken struct {
	kind    tokenKind
	value   string
	literal bool
	dynamic bool
	line    int64
	column  int64
}

func lexSource(content []byte, language language) ([]sourceToken, error) {
	tokens := make([]sourceToken, 0)
	line, column := int64(1), int64(1)
	advance := func(character byte) {
		if character == '\n' {
			line, column = line+1, 1
		} else {
			column++
		}
	}
	for index := 0; index < len(content); {
		character := content[index]
		if isSourceSpace(character) {
			advance(character)
			index++
			continue
		}
		if language == languagePython && character == '#' {
			for index < len(content) && content[index] != '\n' {
				advance(content[index])
				index++
			}
			continue
		}
		if language != languagePython && character == '/' && index+1 < len(content) {
			switch content[index+1] {
			case '/':
				for index < len(content) && content[index] != '\n' {
					advance(content[index])
					index++
				}
				continue
			case '*':
				advance(content[index])
				advance(content[index+1])
				index += 2
				closed := false
				for index < len(content) {
					if content[index] == '*' && index+1 < len(content) && content[index+1] == '/' {
						advance(content[index])
						advance(content[index+1])
						index += 2
						closed = true
						break
					}
					advance(content[index])
					index++
				}
				if !closed {
					return nil, errors.New("unterminated block comment")
				}
				continue
			}
		}
		if character == '\'' || character == '"' || (language != languagePython && character == '`') {
			startLine, startColumn := line, column
			quote := character
			triple := language == languagePython && index+2 < len(content) && content[index+1] == quote && content[index+2] == quote
			formatted := language == languagePython && hasAdjacentPythonFormatPrefix(tokens, startLine, startColumn)
			width := 1
			if triple {
				width = 3
			}
			for count := 0; count < width; count++ {
				advance(content[index+count])
			}
			index += width
			start := index
			escaped := false
			literal := !triple && !formatted
			dynamic := formatted
			closed := false
			for index < len(content) {
				if !triple && content[index] == '\n' && quote != '`' {
					break
				}
				if escaped {
					escaped = false
					literal = false
					advance(content[index])
					index++
					continue
				}
				if content[index] == '\\' {
					escaped = true
					literal = false
					advance(content[index])
					index++
					continue
				}
				if quote == '`' && content[index] == '$' && index+1 < len(content) && content[index+1] == '{' {
					literal = false
					dynamic = true
				}
				if content[index] == quote {
					if triple {
						if index+2 >= len(content) || content[index+1] != quote || content[index+2] != quote {
							advance(content[index])
							index++
							continue
						}
						for count := 0; count < 3; count++ {
							advance(content[index+count])
						}
						index += 3
					} else {
						advance(content[index])
						index++
					}
					closed = true
					break
				}
				advance(content[index])
				index++
			}
			if !closed {
				return nil, errors.New("unterminated string literal")
			}
			if !triple || dynamic {
				end := index - 1
				value := ""
				if !triple {
					value = string(content[start:end])
				}
				tokens = append(tokens, sourceToken{kind: tokenString, value: value, literal: literal, dynamic: dynamic, line: startLine, column: startColumn})
			}
			continue
		}
		if isIdentifierStart(character) {
			start, startLine, startColumn := index, line, column
			for index < len(content) && isIdentifierContinue(content[index]) {
				advance(content[index])
				index++
			}
			tokens = append(tokens, sourceToken{kind: tokenIdentifier, value: string(content[start:index]), literal: true, line: startLine, column: startColumn})
			continue
		}
		if language != languagePython && character == '/' {
			if end, found := regexpCandidateEnd(content, index); found {
				startLine, startColumn := line, column
				clearRegexp := looksLikeRegexpStart(tokens)
				for index < end {
					advance(content[index])
					index++
				}
				for index < len(content) && isIdentifierContinue(content[index]) {
					advance(content[index])
					index++
				}
				if !clearRegexp {
					tokens = append(tokens, sourceToken{kind: tokenOpaque, value: "ambiguous-slash-expression", line: startLine, column: startColumn})
				}
				continue
			}
			if looksLikeRegexpStart(tokens) {
				return nil, errors.New("unterminated regular expression literal")
			}
		}
		tokens = append(tokens, sourceToken{kind: tokenPunctuation, value: string(character), literal: true, line: line, column: column})
		advance(character)
		index++
	}
	return tokens, nil
}

func hasAdjacentPythonFormatPrefix(tokens []sourceToken, line, column int64) bool {
	if len(tokens) == 0 {
		return false
	}
	previous := tokens[len(tokens)-1]
	if previous.kind != tokenIdentifier || previous.line != line || previous.column+int64(len(previous.value)) != column {
		return false
	}
	switch strings.ToLower(previous.value) {
	case "f", "fr", "rf":
		return true
	default:
		return false
	}
}

func regexpCandidateEnd(content []byte, start int) (int, bool) {
	escaped, class := false, false
	for index := start + 1; index < len(content) && content[index] != '\n'; index++ {
		current := content[index]
		if escaped {
			escaped = false
			continue
		}
		if current == '\\' {
			escaped = true
			continue
		}
		if current == '[' {
			class = true
		} else if current == ']' {
			class = false
		} else if current == '/' && !class {
			return index + 1, true
		}
	}
	return 0, false
}

func isSourceSpace(character byte) bool {
	return character == ' ' || character == '\t' || character == '\r' || character == '\n' || character == '\f'
}
func isIdentifierStart(character byte) bool {
	return (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || character == '_' || character == '$'
}
func isIdentifierContinue(character byte) bool {
	return isIdentifierStart(character) || (character >= '0' && character <= '9')
}

func looksLikeRegexpStart(tokens []sourceToken) bool {
	if len(tokens) == 0 {
		return true
	}
	previous := tokens[len(tokens)-1]
	if previous.kind == tokenPunctuation {
		return strings.Contains("(=:[,!&|?{;", previous.value)
	}
	return previous.kind == tokenIdentifier && (previous.value == "return" || previous.value == "case" || previous.value == "throw")
}
