package inspect

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/snapshot"
)

const (
	maxSourceFileBytes  = int64(512 << 10)
	maxSourceTotalBytes = int64(16 << 20)
	maxSourceFileCount  = 2048
	maxSourceTokens     = 200000
	maxDetectedFacts    = 10000
	maxUncertainties    = 1000
	maxInitializations  = 64
)

type FactKind string

const (
	FactPackageDependency    FactKind = "package_dependency"
	FactPackageImport        FactKind = "package_import"
	FactEntrypoint           FactKind = "entrypoint"
	FactAgent                FactKind = "agent"
	FactModelRoute           FactKind = "model_route"
	FactTool                 FactKind = "tool"
	FactMCPServer            FactKind = "mcp_server"
	FactRetrieval            FactKind = "retrieval"
	FactMemory               FactKind = "memory"
	FactEnvironmentReference FactKind = "environment_reference"
	FactCredentialBoundary   FactKind = "credential_boundary"
	FactApproval             FactKind = "approval"
	FactFilesystemBoundary   FactKind = "filesystem_boundary"
	FactProcessBoundary      FactKind = "process_boundary"
	FactNetworkBoundary      FactKind = "network_boundary"
	FactTelemetrySink        FactKind = "telemetry_sink"
	FactPersistenceSink      FactKind = "persistence_sink"
	FactExternalDestination  FactKind = "external_destination"
	FactOpenBoxSDK           FactKind = "openbox_sdk"
)

type Basis string

const (
	BasisDeclared Basis = "declared"
	BasisInferred Basis = "inferred"
)

type Confidence string

const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
)

type Evidence struct {
	Detector   string
	Basis      Basis
	Confidence Confidence
	Path       string
	Line       int64
	Column     int64
	Digest     artifact.ContentDigest
}

type Fact struct {
	Kind  FactKind
	Value string
	// DeclaredValueDigest binds a package dependency to its declared value
	// without retaining a version range, URL credential, or local path.
	DeclaredValueDigest artifact.ContentDigest
	Evidence            Evidence
}

type Uncertainty struct {
	Subject string
	Reason  string
	Path    string
	Line    int64
}

type InitializationBindingShape string

const (
	InitializationBindingEnvironment InitializationBindingShape = "environment"
	InitializationBindingLiteral     InitializationBindingShape = "literal"
	InitializationBindingDynamic     InitializationBindingShape = "dynamic"
)

// InitializationOption is one non-secret classification from a literal
// withOpenBox options object. Coordinate literal values are never retained;
// non-matching control literals use a fixed "other" marker.
type InitializationOption struct {
	Name        string
	Shape       InitializationBindingShape
	Environment string
	Literal     string
}

// OpenBoxInitialization is bounded lexical evidence, not proof that the call
// executes. Options are sorted and carry no credential or coordinate value.
type OpenBoxInitialization struct {
	Function               string
	Target                 string
	Options                []InitializationOption
	HasUnclassifiedOptions bool
	Evidence               Evidence
}

type Detection struct {
	facts           []Fact
	uncertainties   []Uncertainty
	initializations []OpenBoxInitialization
}

func (detection Detection) Facts() []Fact { return append([]Fact(nil), detection.facts...) }
func (detection Detection) Uncertainties() []Uncertainty {
	return append([]Uncertainty(nil), detection.uncertainties...)
}
func (detection Detection) Initializations() []OpenBoxInitialization {
	return cloneInitializations(detection.initializations)
}

// Detect performs bounded lexical discovery over exact Phase 01 snapshot
// bytes. It does not import modules, execute code, evaluate configuration, or
// claim that a lexically present surface is reachable at runtime.
func Detect(copied *snapshot.Snapshot) (Detection, error) {
	if copied == nil {
		return Detection{}, errors.New("inspect: nil project snapshot")
	}
	manifests, err := ReadManifests(copied)
	if err != nil {
		return Detection{}, err
	}
	builder := detectionBuilder{factKeys: make(map[string]struct{}), uncertaintyKeys: make(map[string]struct{})}
	for _, manifest := range manifests {
		if err := builder.detectManifest(manifest); err != nil {
			return Detection{}, err
		}
	}
	var sourceCount int
	var sourceTotal int64
	unsupportedSourcePath := ""
	for _, file := range copied.Files() {
		language, supported := sourceLanguage(file.Path)
		if !supported {
			if unsupportedSourcePath == "" && looksLikeUnsupportedSource(file.Path) {
				unsupportedSourcePath = file.Path
			}
			continue
		}
		if err := validateSourcePath(file.Path); err != nil {
			return Detection{}, err
		}
		if file.Size < 0 || file.Size > maxSourceFileBytes {
			return Detection{}, fmt.Errorf("inspect: source %q exceeds %d bytes", file.Path, maxSourceFileBytes)
		}
		if file.Size > maxSourceTotalBytes-sourceTotal {
			return Detection{}, fmt.Errorf("inspect: source bytes exceed %d total", maxSourceTotalBytes)
		}
		sourceTotal += file.Size
		sourceCount++
		if sourceCount > maxSourceFileCount {
			return Detection{}, fmt.Errorf("inspect: source file count exceeds %d", maxSourceFileCount)
		}
		content, err := readManifestFile(copied.Root(), file.Path, file.Size)
		if err != nil {
			return Detection{}, fmt.Errorf("inspect: read source %q: %w", file.Path, err)
		}
		if artifact.DigestBytes(content) != file.Digest {
			return Detection{}, fmt.Errorf("inspect: source %q changed after snapshot", file.Path)
		}
		if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
			return Detection{}, fmt.Errorf("inspect: source %q is not valid NUL-free UTF-8", file.Path)
		}
		tokens, err := lexSource(content, language)
		if err != nil {
			return Detection{}, fmt.Errorf("inspect: tokenize %q: %w", file.Path, err)
		}
		if len(tokens) > maxSourceTokens {
			return Detection{}, fmt.Errorf("inspect: source %q exceeds %d tokens", file.Path, maxSourceTokens)
		}
		builder.detectSource(file, language, tokens)
		if builder.err != nil {
			return Detection{}, builder.err
		}
	}
	if unsupportedSourcePath != "" {
		builder.recordUncertainty(Uncertainty{
			Subject: "unsupported-source-language",
			Reason:  "At least one source-like file uses a language outside the closed JavaScript, TypeScript, and Python detector set.",
			Path:    unsupportedSourcePath,
			Line:    1,
		})
	}
	if copied.FileCount() > 0 {
		builder.recordUncertainty(Uncertainty{
			Subject: "runtime-registration",
			Reason:  "Passive source inspection cannot enumerate registrations or branches created only during execution.",
		})
	}
	return builder.finish()
}

type detectionBuilder struct {
	facts           []Fact
	uncertainties   []Uncertainty
	initializations []OpenBoxInitialization
	factKeys        map[string]struct{}
	uncertaintyKeys map[string]struct{}
	err             error
}

func (builder *detectionBuilder) recordInitialization(initialization OpenBoxInitialization) {
	if builder.err != nil {
		return
	}
	if len(builder.initializations) >= maxInitializations {
		builder.err = fmt.Errorf("inspect: withOpenBox initializations exceed %d", maxInitializations)
		return
	}
	builder.initializations = append(builder.initializations, initialization)
}

func (builder *detectionBuilder) recordFact(fact Fact) {
	if builder.err == nil {
		builder.err = builder.addFact(fact)
	}
}

func (builder *detectionBuilder) recordUncertainty(uncertainty Uncertainty) {
	if builder.err == nil {
		builder.err = builder.addUncertainty(uncertainty)
	}
}

func (builder *detectionBuilder) addFact(fact Fact) error {
	if fact.Value == "" {
		return nil
	}
	if !validFactValue(fact.Value) {
		return fmt.Errorf("inspect: %s fact value is outside the bounded UTF-8 contract", fact.Kind)
	}
	key := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d", fact.Kind, fact.Value, fact.Evidence.Path, fact.Evidence.Line, fact.Evidence.Column)
	if _, exists := builder.factKeys[key]; exists {
		return nil
	}
	if len(builder.facts) >= maxDetectedFacts {
		return fmt.Errorf("inspect: detected facts exceed %d", maxDetectedFacts)
	}
	builder.factKeys[key] = struct{}{}
	builder.facts = append(builder.facts, fact)
	return nil
}

func (builder *detectionBuilder) addUncertainty(uncertainty Uncertainty) error {
	key := fmt.Sprintf("%s\x00%s\x00%s\x00%d", uncertainty.Subject, uncertainty.Reason, uncertainty.Path, uncertainty.Line)
	if _, exists := builder.uncertaintyKeys[key]; exists {
		return nil
	}
	if len(builder.uncertainties) >= maxUncertainties {
		return fmt.Errorf("inspect: uncertainties exceed %d", maxUncertainties)
	}
	builder.uncertaintyKeys[key] = struct{}{}
	builder.uncertainties = append(builder.uncertainties, uncertainty)
	return nil
}

func (builder *detectionBuilder) finish() (Detection, error) {
	if builder.err != nil {
		return Detection{}, builder.err
	}
	sort.Slice(builder.facts, func(left, right int) bool {
		a, b := builder.facts[left], builder.facts[right]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Value != b.Value {
			return a.Value < b.Value
		}
		if a.Evidence.Path != b.Evidence.Path {
			return a.Evidence.Path < b.Evidence.Path
		}
		if a.Evidence.Line != b.Evidence.Line {
			return a.Evidence.Line < b.Evidence.Line
		}
		return a.Evidence.Column < b.Evidence.Column
	})
	sort.Slice(builder.uncertainties, func(left, right int) bool {
		a, b := builder.uncertainties[left], builder.uncertainties[right]
		if a.Subject != b.Subject {
			return a.Subject < b.Subject
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Reason < b.Reason
	})
	sort.Slice(builder.initializations, func(left, right int) bool {
		a, b := builder.initializations[left], builder.initializations[right]
		if a.Evidence.Path != b.Evidence.Path {
			return a.Evidence.Path < b.Evidence.Path
		}
		if a.Evidence.Line != b.Evidence.Line {
			return a.Evidence.Line < b.Evidence.Line
		}
		return a.Evidence.Column < b.Evidence.Column
	})
	return Detection{facts: builder.facts, uncertainties: builder.uncertainties, initializations: cloneInitializations(builder.initializations)}, nil
}

func cloneInitializations(source []OpenBoxInitialization) []OpenBoxInitialization {
	result := append([]OpenBoxInitialization(nil), source...)
	for index := range result {
		result[index].Options = append([]InitializationOption(nil), result[index].Options...)
	}
	return result
}

func (builder *detectionBuilder) detectManifest(manifest Manifest) error {
	switch manifest.Kind() {
	case KindPackageJSON:
		return builder.detectPackageJSON(manifest)
	case KindRequirements:
		if err := builder.detectRequirements(manifest); err != nil {
			return err
		}
		return builder.addUncertainty(Uncertainty{
			Subject: "partially-parsed-manifest",
			Reason:  "Requirements input is inspected as bounded opaque lines; full requirement syntax and resolution are not evaluated.",
			Path:    manifest.path,
			Line:    1,
		})
	default:
		return builder.addUncertainty(Uncertainty{
			Subject: "opaque-manifest",
			Reason:  fmt.Sprintf("%s bytes are inventoried but their configuration semantics are not evaluated.", manifest.kind),
			Path:    manifest.path,
			Line:    1,
		})
	}
}

func (builder *detectionBuilder) detectPackageJSON(manifest Manifest) error {
	decoder := json.NewDecoder(bytes.NewReader(manifest.bytes))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		return err
	}
	locations, err := lexJSONLocations(manifest.bytes)
	if err != nil {
		return err
	}
	for _, section := range []string{"dependencies", "devDependencies", "peerDependencies", "optionalDependencies"} {
		dependencies, _ := document[section].(map[string]any)
		names := make([]string, 0, len(dependencies))
		for name := range dependencies {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			offset, found := jsonDependencyLocation(locations, section, name)
			if !found {
				return fmt.Errorf("inspect: cannot locate package.json dependency %q in %s", name, section)
			}
			line, column := offsetLocation(manifest.bytes, offset)
			declaredValue, _ := dependencies[name].(string)
			if err := builder.addFact(Fact{
				Kind: FactPackageDependency, Value: name,
				DeclaredValueDigest: artifact.DigestBytes([]byte(declaredValue)), Evidence: Evidence{
					Detector: "package-json-dependency", Basis: BasisDeclared, Confidence: ConfidenceHigh,
					Path: manifest.path, Line: line, Column: column, Digest: manifest.digest,
				}}); err != nil {
				return err
			}
			if isOpenBoxPackage(name) {
				if err := builder.addFact(Fact{Kind: FactOpenBoxSDK, Value: name, Evidence: Evidence{
					Detector: "package-json-openbox-sdk", Basis: BasisDeclared, Confidence: ConfidenceHigh,
					Path: manifest.path, Line: line, Column: column, Digest: manifest.digest,
				}}); err != nil {
					return err
				}
			}
		}
	}
	for _, field := range []string{"main", "module", "bin"} {
		switch value := document[field].(type) {
		case string:
			offset, found := jsonEntrypointLocation(locations, field, "", value)
			if !found {
				return fmt.Errorf("inspect: cannot locate package.json %s entrypoint", field)
			}
			line, column := offsetLocation(manifest.bytes, offset)
			if err := builder.addFact(Fact{Kind: FactEntrypoint, Value: value, Evidence: Evidence{Detector: "package-json-entrypoint", Basis: BasisDeclared, Confidence: ConfidenceHigh, Path: manifest.path, Line: line, Column: column, Digest: manifest.digest}}); err != nil {
				return err
			}
		case map[string]any:
			names := make([]string, 0, len(value))
			for name := range value {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				entry, ok := value[name].(string)
				if !ok {
					continue
				}
				offset, found := jsonEntrypointLocation(locations, field, name, entry)
				if !found {
					return fmt.Errorf("inspect: cannot locate package.json %s.%s entrypoint", field, name)
				}
				line, column := offsetLocation(manifest.bytes, offset)
				if err := builder.addFact(Fact{Kind: FactEntrypoint, Value: entry, Evidence: Evidence{Detector: "package-json-entrypoint", Basis: BasisDeclared, Confidence: ConfidenceHigh, Path: manifest.path, Line: line, Column: column, Digest: manifest.digest}}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (builder *detectionBuilder) detectRequirements(manifest Manifest) error {
	lines := bytes.Split(manifest.bytes, []byte("\n"))
	for index, raw := range lines {
		trimmed := strings.TrimSpace(string(raw))
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "-") || strings.Contains(trimmed, "://") {
			continue
		}
		name := leadingPackageName(trimmed)
		if name == "" {
			continue
		}
		column := int64(bytes.Index(raw, []byte(trimmed)) + 1)
		if err := builder.addFact(Fact{Kind: FactPackageDependency, Value: name, Evidence: Evidence{Detector: "requirements-dependency", Basis: BasisDeclared, Confidence: ConfidenceMedium, Path: manifest.path, Line: int64(index + 1), Column: column, Digest: manifest.digest}}); err != nil {
			return err
		}
		if isOpenBoxPackage(strings.ReplaceAll(name, "-", "_")) {
			if err := builder.addFact(Fact{Kind: FactOpenBoxSDK, Value: name, Evidence: Evidence{Detector: "requirements-openbox-sdk", Basis: BasisDeclared, Confidence: ConfidenceMedium, Path: manifest.path, Line: int64(index + 1), Column: column, Digest: manifest.digest}}); err != nil {
				return err
			}
		}
	}
	return nil
}

func leadingPackageName(value string) string {
	end := 0
	for end < len(value) {
		character := value[end]
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-') {
			break
		}
		end++
	}
	if end == 0 {
		return ""
	}
	return strings.ToLower(strings.ReplaceAll(value[:end], "_", "-"))
}

func offsetLocation(content []byte, offset int) (int64, int64) {
	line := int64(bytes.Count(content[:offset], []byte("\n")) + 1)
	lastNewline := bytes.LastIndexByte(content[:offset], '\n')
	return line, int64(offset - lastNewline)
}

func validateSourcePath(relative string) error {
	if !utf8.ValidString(relative) || utf8.RuneCountInString(relative) > maxManifestPathRunes || relative == "" || relative == "." || path.IsAbs(relative) || path.Clean(relative) != relative || strings.Contains(relative, "\\") || hasDrivePrefix(relative) {
		return fmt.Errorf("inspect: source path %q is outside the v1 path boundary", relative)
	}
	components := strings.Split(relative, "/")
	if len(components) > maxManifestPathDepth {
		return fmt.Errorf("inspect: source path %q exceeds depth %d", relative, maxManifestPathDepth)
	}
	for _, component := range components {
		for _, character := range component {
			if character <= 0x1f || character == 0x7f {
				return fmt.Errorf("inspect: source path %q contains a control character", relative)
			}
		}
	}
	return nil
}

func sourceLanguage(relative string) (language, bool) {
	if strings.HasSuffix(relative, ".d.ts") {
		return languageTypeScript, true
	}
	switch strings.ToLower(path.Ext(relative)) {
	case ".js", ".jsx", ".mjs", ".cjs":
		return languageJavaScript, true
	case ".ts", ".tsx", ".mts", ".cts":
		return languageTypeScript, true
	case ".py", ".pyi":
		return languagePython, true
	default:
		return 0, false
	}
}

func looksLikeUnsupportedSource(relative string) bool {
	switch strings.ToLower(path.Ext(relative)) {
	case ".go", ".java", ".rb", ".rs", ".php", ".swift", ".kt", ".kts", ".scala", ".cs", ".sh", ".bash", ".vue", ".svelte", ".lua", ".ex", ".exs", ".dart", ".c", ".cc", ".cpp", ".h", ".hpp":
		return true
	default:
		return false
	}
}

func isOpenBoxPackage(value string) bool {
	return strings.HasPrefix(value, "@openbox-ai/openbox-") || strings.HasPrefix(value, "openbox_") || strings.HasPrefix(value, "openbox-")
}

func safeDestination(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || strings.ContainsAny(parsed.Host, "{}") {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}
