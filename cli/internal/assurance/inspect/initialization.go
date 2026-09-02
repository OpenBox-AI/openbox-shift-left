package inspect

import (
	"sort"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/snapshot"
)

var knownInitializationOptions = map[string]struct{}{
	"apiUrl": {}, "apiKey": {}, "validate": {}, "onApiError": {},
	"sendActivityStartEvent": {}, "evaluateMaxRetries": {}, "governanceTimeout": {},
	"hitlEnabled": {}, "httpCapture": {}, "instrumentDatabases": {},
	"instrumentFileIo": {}, "agentDid": {}, "agentPrivateKey": {},
}

func (builder *detectionBuilder) detectOpenBoxInitialization(file snapshot.File, tokens []sourceToken, index int) {
	if tokens[index].kind != tokenIdentifier || tokens[index].value != "withOpenBox" ||
		index+1 >= len(tokens) || tokens[index+1].value != "(" {
		return
	}
	initialization := OpenBoxInitialization{
		Function: "withOpenBox",
		Evidence: sourceEvidence("source-withopenbox-initialization", file, tokens[index], ConfidenceHigh),
	}
	if index > 0 && tokens[index-1].value == "." {
		initialization.HasUnclassifiedOptions = true
	}
	arguments, ok := splitCallArguments(tokens, index+1)
	if !ok || len(arguments) != 2 {
		initialization.HasUnclassifiedOptions = true
		builder.recordInitialization(initialization)
		return
	}
	if len(arguments[0]) == 1 && arguments[0][0].kind == tokenIdentifier && arguments[0][0].value == "mastra" {
		initialization.Target = "mastra"
	}
	options, unclassified := parseInitializationOptions(arguments[1])
	initialization.Options = options
	initialization.HasUnclassifiedOptions = initialization.HasUnclassifiedOptions || unclassified
	builder.recordInitialization(initialization)
}

func splitCallArguments(tokens []sourceToken, open int) ([][]sourceToken, bool) {
	start := open + 1
	paren, brace, bracket := 0, 0, 0
	arguments := make([][]sourceToken, 0, 2)
	for cursor := start; cursor < len(tokens); cursor++ {
		switch tokens[cursor].value {
		case "(":
			paren++
		case ")":
			if paren == 0 && brace == 0 && bracket == 0 {
				arguments = append(arguments, append([]sourceToken(nil), tokens[start:cursor]...))
				if len(arguments) == 1 && len(arguments[0]) == 0 {
					return nil, true
				}
				return arguments, true
			}
			paren--
		case "{":
			brace++
		case "}":
			brace--
		case "[":
			bracket++
		case "]":
			bracket--
		case ",":
			if paren == 0 && brace == 0 && bracket == 0 {
				arguments = append(arguments, append([]sourceToken(nil), tokens[start:cursor]...))
				start = cursor + 1
			}
		}
		if paren < 0 || brace < 0 || bracket < 0 {
			return nil, false
		}
	}
	return nil, false
}

func parseInitializationOptions(tokens []sourceToken) ([]InitializationOption, bool) {
	if len(tokens) < 2 || tokens[0].value != "{" || tokens[len(tokens)-1].value != "}" {
		return nil, true
	}
	properties, ok := splitObjectProperties(tokens[1 : len(tokens)-1])
	if !ok {
		return nil, true
	}
	options := make([]InitializationOption, 0, len(properties))
	seen := make(map[string]struct{}, len(properties))
	unclassified := false
	for _, property := range properties {
		if len(property) == 0 {
			continue
		}
		if len(property) < 3 || property[1].value != ":" ||
			(property[0].kind != tokenIdentifier && (property[0].kind != tokenString || !property[0].literal)) {
			unclassified = true
			continue
		}
		name := property[0].value
		if _, known := knownInitializationOptions[name]; !known {
			unclassified = true
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			unclassified = true
			continue
		}
		seen[name] = struct{}{}
		options = append(options, classifyInitializationOption(name, property[2:]))
	}
	sort.Slice(options, func(left, right int) bool { return options[left].Name < options[right].Name })
	return options, unclassified
}

func splitObjectProperties(tokens []sourceToken) ([][]sourceToken, bool) {
	if len(tokens) == 0 {
		return nil, true
	}
	start := 0
	paren, brace, bracket := 0, 0, 0
	properties := make([][]sourceToken, 0)
	for cursor, token := range tokens {
		switch token.value {
		case "(":
			paren++
		case ")":
			paren--
		case "{":
			brace++
		case "}":
			brace--
		case "[":
			bracket++
		case "]":
			bracket--
		case ",":
			if paren == 0 && brace == 0 && bracket == 0 {
				properties = append(properties, append([]sourceToken(nil), tokens[start:cursor]...))
				start = cursor + 1
			}
		}
		if paren < 0 || brace < 0 || bracket < 0 {
			return nil, false
		}
	}
	if paren != 0 || brace != 0 || bracket != 0 {
		return nil, false
	}
	if start < len(tokens) {
		properties = append(properties, append([]sourceToken(nil), tokens[start:]...))
	}
	return properties, true
}

func classifyInitializationOption(name string, value []sourceToken) InitializationOption {
	option := InitializationOption{Name: name, Shape: InitializationBindingDynamic}
	if environment, ok := exactEnvironmentReference(value); ok {
		option.Shape = InitializationBindingEnvironment
		option.Environment = environment
		return option
	}
	if name == "apiUrl" || name == "apiKey" {
		if len(value) == 1 && value[0].kind == tokenString && value[0].literal {
			option.Shape = InitializationBindingLiteral
		}
		return option
	}
	if len(value) != 1 {
		return option
	}
	candidate := value[0]
	literal := candidate.kind == tokenString && candidate.literal ||
		candidate.kind == tokenIdentifier && (candidate.value == "true" || candidate.value == "false") ||
		candidate.kind == tokenPunctuation && (candidate.value == "0" || candidate.value == "5")
	if literal {
		option.Shape = InitializationBindingLiteral
		if safeControlLiteral(candidate.value) {
			option.Literal = candidate.value
		} else {
			option.Literal = "other"
		}
	}
	return option
}

func exactEnvironmentReference(tokens []sourceToken) (string, bool) {
	if len(tokens) == 5 && tokens[0].value == "process" && tokens[1].value == "." && tokens[2].value == "env" && tokens[3].value == "." && tokens[4].kind == tokenIdentifier && validEnvironmentName(tokens[4].value) {
		return tokens[4].value, true
	}
	if len(tokens) == 6 && tokens[0].value == "process" && tokens[1].value == "." && tokens[2].value == "env" && tokens[3].value == "[" && tokens[4].kind == tokenString && tokens[4].literal && tokens[5].value == "]" && validEnvironmentName(tokens[4].value) {
		return tokens[4].value, true
	}
	return "", false
}

func safeControlLiteral(value string) bool {
	switch strings.ToLower(value) {
	case "true", "false", "0", "5", "fail_closed", "fail_open":
		return true
	default:
		return false
	}
}
