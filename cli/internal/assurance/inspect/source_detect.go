package inspect

import (
	"strings"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/snapshot"
)

var recognizedCalls = map[string]FactKind{
	"agent": FactAgent, "createagent": FactAgent,
	"createtool": FactTool, "tool": FactTool,
	"mcpclient": FactMCPServer, "mcpserver": FactMCPServer,
	"createretriever": FactRetrieval, "retriever": FactRetrieval, "vectorquery": FactRetrieval,
	"memory": FactMemory, "creatememory": FactMemory,
	"generatetext": FactModelRoute, "streamtext": FactModelRoute, "callmodel": FactModelRoute,
	"requestapproval": FactApproval, "humanintheloop": FactApproval,
	"fetch": FactNetworkBoundary, "request": FactNetworkBoundary,
	"query": FactPersistenceSink, "connect": FactPersistenceSink, "execute": FactPersistenceSink,
	"startspan": FactTelemetrySink, "gettracer": FactTelemetrySink,
	"readfile": FactFilesystemBoundary, "readfilesync": FactFilesystemBoundary,
	"writefile": FactFilesystemBoundary, "writefilesync": FactFilesystemBoundary,
	"spawn": FactProcessBoundary, "execfile": FactProcessBoundary,
	"registerapiroute": FactEntrypoint, "listen": FactEntrypoint,
}

func (builder *detectionBuilder) detectSource(file snapshot.File, language language, tokens []sourceToken) {
	for index := 0; index < len(tokens); index++ {
		if builder.err != nil {
			return
		}
		token := tokens[index]
		if token.kind == tokenOpaque {
			builder.recordUncertainty(Uncertainty{Subject: token.value, Reason: "Slash-delimited JavaScript syntax was conservatively skipped because division and regular-expression grammar are ambiguous without execution-grade parsing.", Path: file.Path, Line: token.line})
			continue
		}
		if token.kind == tokenString && token.dynamic {
			subject := "dynamic-template-expression"
			reason := "JavaScript template interpolation contains executable syntax that this passive lexer does not evaluate."
			if language == languagePython {
				subject = "dynamic-string"
				reason = "Formatted Python string content cannot be resolved passively."
			}
			builder.recordUncertainty(Uncertainty{Subject: subject, Reason: reason, Path: file.Path, Line: token.line})
			continue
		}
		if token.kind == tokenString && token.literal {
			if destination := safeDestination(token.value); destination != "" {
				builder.recordFact(Fact{Kind: FactExternalDestination, Value: destination, Evidence: sourceEvidence("source-url", file, token, ConfidenceMedium)})
			}
		}
		if language == languagePython {
			builder.detectPythonImport(file, tokens, index)
		} else {
			builder.detectJSImport(file, tokens, index)
		}
		builder.detectCall(file, tokens, index)
		builder.detectOpenBoxInitialization(file, tokens, index)
		builder.detectEnvironment(file, tokens, index)
		builder.detectCredentialIdentifier(file, tokens, index)
	}
}

func (builder *detectionBuilder) detectJSImport(file snapshot.File, tokens []sourceToken, index int) {
	token := tokens[index]
	if token.kind != tokenIdentifier {
		return
	}
	if token.value == "import" {
		if index+1 < len(tokens) && tokens[index+1].value == "(" {
			if index+3 >= len(tokens) || tokens[index+2].kind != tokenString || !tokens[index+2].literal || tokens[index+3].value != ")" {
				builder.recordUncertainty(Uncertainty{Subject: "dynamic-import", Reason: "Dynamic import target is not a complete literal and cannot be resolved passively.", Path: file.Path, Line: token.line})
				return
			}
			builder.addImportFact(file, tokens[index+2], "javascript-import")
			return
		}
		if index+1 < len(tokens) && tokens[index+1].kind == tokenString {
			builder.recordJSImportTarget(file, token, tokens[index+1])
			return
		}
		for cursor := index + 1; cursor < len(tokens) && tokens[cursor].line <= token.line+8; cursor++ {
			if tokens[cursor].kind == tokenString {
				builder.recordUncertainty(Uncertainty{Subject: "ambiguous-import-syntax", Reason: "JavaScript import declaration contains a string before a from clause.", Path: file.Path, Line: token.line})
				return
			}
			if tokens[cursor].kind == tokenIdentifier && tokens[cursor].value == "from" {
				if cursor+1 >= len(tokens) || tokens[cursor+1].kind != tokenString {
					builder.recordUncertainty(Uncertainty{Subject: "ambiguous-import-syntax", Reason: "JavaScript from clause is not followed immediately by a string target.", Path: file.Path, Line: token.line})
					return
				}
				builder.recordJSImportTarget(file, token, tokens[cursor+1])
				return
			}
			if tokens[cursor].value == ";" {
				builder.recordUncertainty(Uncertainty{Subject: "ambiguous-import-syntax", Reason: "JavaScript import declaration ended without a literal target.", Path: file.Path, Line: token.line})
				return
			}
		}
		builder.recordUncertainty(Uncertainty{Subject: "ambiguous-import-syntax", Reason: "JavaScript import declaration ended without a literal target.", Path: file.Path, Line: token.line})
	}
	if token.value == "require" && index+1 < len(tokens) && tokens[index+1].value == "(" {
		if index+3 < len(tokens) && tokens[index+2].kind == tokenString && tokens[index+2].literal && tokens[index+3].value == ")" {
			builder.addImportFact(file, tokens[index+2], "javascript-require")
		} else {
			builder.recordUncertainty(Uncertainty{Subject: "dynamic-import", Reason: "require target is not a complete literal and cannot be resolved passively.", Path: file.Path, Line: token.line})
		}
	}
}

func (builder *detectionBuilder) recordJSImportTarget(file snapshot.File, importToken, target sourceToken) {
	if target.literal {
		builder.addImportFact(file, target, "javascript-import")
		return
	}
	builder.recordUncertainty(Uncertainty{Subject: "dynamic-import", Reason: "Import target contains escapes or interpolation and cannot be resolved passively.", Path: file.Path, Line: importToken.line})
}

func (builder *detectionBuilder) detectPythonImport(file snapshot.File, tokens []sourceToken, index int) {
	token := tokens[index]
	if token.kind != tokenIdentifier || (token.value != "import" && token.value != "from") {
		return
	}
	if token.value == "from" {
		module, location, _ := pythonModule(tokens, index+1, token.line)
		if module != "" {
			builder.addImportFact(file, sourceToken{value: module, literal: true, line: location.line, column: location.column}, "python-import")
		} else {
			builder.recordUncertainty(Uncertainty{Subject: "dynamic-import", Reason: "Python import syntax could not be resolved as a same-line literal module.", Path: file.Path, Line: token.line})
		}
		return
	}
	for cursor := index - 1; cursor >= 0 && tokens[cursor].line == token.line; cursor-- {
		if tokens[cursor].value == ";" {
			break
		}
		if tokens[cursor].kind == tokenIdentifier && tokens[cursor].value == "from" {
			return
		}
	}
	for cursor := index + 1; cursor < len(tokens) && tokens[cursor].line == token.line; {
		module, location, next := pythonModule(tokens, cursor, token.line)
		if module == "" {
			builder.recordUncertainty(Uncertainty{Subject: "dynamic-import", Reason: "Python import syntax could not be resolved as a same-line literal module.", Path: file.Path, Line: token.line})
			return
		}
		builder.addImportFact(file, sourceToken{value: module, literal: true, line: location.line, column: location.column}, "python-import")
		cursor = next
		if cursor+1 < len(tokens) && tokens[cursor].line == token.line && tokens[cursor].kind == tokenIdentifier && tokens[cursor].value == "as" {
			cursor += 2
		}
		if cursor >= len(tokens) || tokens[cursor].line != token.line || tokens[cursor].value != "," {
			return
		}
		cursor++
	}
}

func pythonModule(tokens []sourceToken, start int, line int64) (string, sourceToken, int) {
	if start >= len(tokens) || tokens[start].line != line {
		return "", sourceToken{}, start
	}
	location := tokens[start]
	prefix := ""
	cursor := start
	for cursor < len(tokens) && tokens[cursor].line == line && tokens[cursor].value == "." {
		prefix += "."
		cursor++
	}
	if prefix != "" && cursor < len(tokens) && tokens[cursor].line == line && tokens[cursor].value == "import" {
		return prefix, location, cursor
	}
	if cursor >= len(tokens) || tokens[cursor].line != line || tokens[cursor].kind != tokenIdentifier {
		return "", sourceToken{}, start
	}
	parts := []string{tokens[cursor].value}
	cursor++
	for cursor+1 < len(tokens) && tokens[cursor].line == line && tokens[cursor].value == "." && tokens[cursor+1].line == line && tokens[cursor+1].kind == tokenIdentifier {
		parts = append(parts, tokens[cursor+1].value)
		cursor += 2
	}
	return prefix + strings.Join(parts, "."), location, cursor
}

func (builder *detectionBuilder) addImportFact(file snapshot.File, token sourceToken, detector string) {
	builder.recordFact(Fact{Kind: FactPackageImport, Value: token.value, Evidence: sourceEvidence(detector, file, token, ConfidenceHigh)})
	if isOpenBoxPackage(token.value) {
		builder.recordFact(Fact{Kind: FactOpenBoxSDK, Value: token.value, Evidence: sourceEvidence("source-openbox-sdk", file, token, ConfidenceHigh)})
	}
}

func (builder *detectionBuilder) detectCall(file snapshot.File, tokens []sourceToken, index int) {
	if tokens[index].kind != tokenIdentifier {
		return
	}
	if index >= 2 && tokens[index-1].value == "." && tokens[index-2].kind == tokenIdentifier {
		return
	}
	parts := []string{tokens[index].value}
	cursor := index + 1
	for cursor+1 < len(tokens) && tokens[cursor].value == "." && tokens[cursor+1].kind == tokenIdentifier {
		parts = append(parts, tokens[cursor+1].value)
		cursor += 2
	}
	if cursor >= len(tokens) || tokens[cursor].value != "(" {
		return
	}
	full := strings.Join(parts, ".")
	if full == "importlib.import_module" || full == "__import__" {
		if cursor+2 < len(tokens) && tokens[cursor+1].kind == tokenString && tokens[cursor+1].literal && tokens[cursor+2].value == ")" {
			builder.addImportFact(file, tokens[cursor+1], "python-dynamic-import-literal")
		} else {
			builder.recordUncertainty(Uncertainty{Subject: "dynamic-import", Reason: "Python runtime import target is not a complete literal and cannot be resolved passively.", Path: file.Path, Line: tokens[index].line})
		}
		return
	}
	last := strings.ToLower(parts[len(parts)-1])
	kind, recognized := recognizedCalls[last]
	if !recognized {
		if (last == "get" || last == "post" || last == "put" || last == "delete" || last == "patch") && len(parts) > 1 {
			receiver := strings.ToLower(parts[len(parts)-2])
			switch receiver {
			case "app", "router", "server", "fastify":
				kind, recognized = FactEntrypoint, true
			case "axios", "http", "https", "requests", "client":
				kind, recognized = FactNetworkBoundary, true
			default:
				builder.recordUncertainty(Uncertainty{Subject: "ambiguous-http-method", Reason: "A method-shaped HTTP call has an unknown receiver and cannot be classified as inbound or outbound passively.", Path: file.Path, Line: tokens[index].line})
			}
		}
		if full == "child_process.exec" || full == "child_process.spawn" || full == "subprocess.run" || full == "subprocess.Popen" || full == "os.system" {
			kind, recognized = FactProcessBoundary, true
		}
	}
	if recognized {
		builder.recordFact(Fact{Kind: kind, Value: full, Evidence: sourceEvidence("source-call", file, tokens[index], ConfidenceMedium)})
	}
}

func (builder *detectionBuilder) detectEnvironment(file snapshot.File, tokens []sourceToken, index int) {
	nameToken, ok, dynamic := environmentReference(tokens, index)
	if dynamic {
		builder.recordUncertainty(Uncertainty{Subject: "dynamic-environment-reference", Reason: "Environment key is computed and only its presence, not its name, is known passively.", Path: file.Path, Line: tokens[index].line})
		return
	}
	if !ok || !validEnvironmentName(nameToken.value) {
		return
	}
	builder.recordFact(Fact{Kind: FactEnvironmentReference, Value: nameToken.value, Evidence: sourceEvidence("source-environment", file, nameToken, ConfidenceHigh)})
	if likelyCredentialName(nameToken.value) {
		builder.recordFact(Fact{Kind: FactCredentialBoundary, Value: nameToken.value, Evidence: sourceEvidence("source-credential-name", file, nameToken, ConfidenceMedium)})
	}
}

func (builder *detectionBuilder) detectCredentialIdentifier(file snapshot.File, tokens []sourceToken, index int) {
	token := tokens[index]
	if token.kind != tokenIdentifier || !likelyCredentialName(token.value) {
		return
	}
	builder.recordFact(Fact{Kind: FactCredentialBoundary, Value: token.value, Evidence: sourceEvidence("source-credential-name", file, token, ConfidenceMedium)})
}

func environmentReference(tokens []sourceToken, index int) (sourceToken, bool, bool) {
	if index+4 < len(tokens) && tokens[index].value == "process" && tokens[index+1].value == "." && tokens[index+2].value == "env" {
		if tokens[index+3].value == "." && tokens[index+4].kind == tokenIdentifier {
			return tokens[index+4], true, false
		}
		if tokens[index+3].value == "[" {
			if tokens[index+4].kind == tokenString && tokens[index+4].literal {
				return tokens[index+4], true, false
			}
			return sourceToken{}, false, true
		}
	}
	if index+4 < len(tokens) && tokens[index].value == "os" && tokens[index+1].value == "." && tokens[index+2].value == "environ" && tokens[index+3].value == "[" {
		if tokens[index+4].kind == tokenString && tokens[index+4].literal {
			return tokens[index+4], true, false
		}
		return sourceToken{}, false, true
	}
	if index+4 < len(tokens) && tokens[index].value == "os" && tokens[index+1].value == "." && tokens[index+2].value == "getenv" && tokens[index+3].value == "(" {
		if tokens[index+4].kind == tokenString && tokens[index+4].literal {
			return tokens[index+4], true, false
		}
		return sourceToken{}, false, true
	}
	return sourceToken{}, false, false
}

func sourceEvidence(detector string, file snapshot.File, token sourceToken, confidence Confidence) Evidence {
	return Evidence{Detector: detector, Basis: BasisInferred, Confidence: confidence, Path: file.Path, Line: token.line, Column: token.column, Digest: file.Digest}
}

func likelyCredentialName(name string) bool {
	upper := strings.ToUpper(name)
	for _, marker := range []string{"API_KEY", "APIKEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "PRIVATE_KEY", "PRIVATEKEY", "ACCESS_KEY", "ACCESSKEY", "AUTH"} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

func validEnvironmentName(name string) bool {
	if name == "" || len(name) > 256 {
		return false
	}
	for index, character := range name {
		if !((character >= 'A' && character <= 'Z') || (index > 0 && character >= '0' && character <= '9') || (index > 0 && character == '_')) {
			return false
		}
	}
	return true
}

func validFactValue(value string) bool {
	if value == "" || utf8.RuneCountInString(value) > 4096 {
		return false
	}
	for _, character := range value {
		if character <= 0x1f || character == 0x7f {
			return false
		}
	}
	return true
}
