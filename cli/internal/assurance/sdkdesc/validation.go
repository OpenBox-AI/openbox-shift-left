package sdkdesc

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/inspect"
)

type BindingShape string

const (
	BindingOmitted     BindingShape = "omitted"
	BindingEnvironment BindingShape = "environment"
	BindingLiteral     BindingShape = "literal"
	BindingDynamic     BindingShape = "dynamic"
)

// CoordinateBinding classifies a coordinate option without providing any
// field in which a credential or URL literal could be retained.
type CoordinateBinding struct {
	Shape       BindingShape
	Environment string
}

// ControlBinding retains only a non-secret literal safe-control value.
type ControlBinding struct {
	Shape   BindingShape
	Literal string
}

// IdentityBinding classifies an optional identity option without retaining its
// environment reference or value. Every non-omitted shape is unsupported in
// the unsigned MVP.
type IdentityBinding struct {
	Shape BindingShape
}

type Initialization struct {
	Function               string
	Target                 string
	APIURL                 CoordinateBinding
	APIKey                 CoordinateBinding
	Validate               ControlBinding
	OnAPIError             ControlBinding
	SendActivityStartEvent ControlBinding
	EvaluateMaxRetries     ControlBinding
	GovernanceTimeout      ControlBinding
	HITLEnabled            ControlBinding
	HTTPCapture            ControlBinding
	InstrumentDatabases    ControlBinding
	InstrumentFileIO       ControlBinding
	AgentDID               IdentityBinding
	AgentPrivateKey        IdentityBinding
	HasUnclassifiedOptions bool
}

type PackageResolution struct {
	Requested   string
	Resolved    string
	Version     string
	ResolvedURI string
	Integrity   string
}

// SourceAttestation is supplied only by the trusted local-source qualifier. A
// project manifest cannot self-assert or derive this authority.
type SourceAttestation struct {
	Commit        string
	ArchiveSHA256 string
}

type Candidate struct {
	Source          SourceQualification
	Packages        []PackageResolution
	Initializations []Initialization
}

type Status string

const (
	Compatible  Status = "compatible"
	NotRunnable Status = "not_runnable"
)

type Problem struct {
	Code    string
	Field   string
	Message string
}

type Compatibility struct {
	DescriptorID string
	Status       Status
	Problems     []Problem
}

// Validate checks the closed candidate shape against the only MVP descriptor.
// It never treats package presence as runtime observation.
func Validate(candidate Candidate) Compatibility {
	problems := validateSource(candidate.Source)
	problems = append(problems, validatePackages(candidate.Packages)...)
	if len(candidate.Initializations) != 1 {
		problems = append(problems, problem("ambiguous_initialization", "initializations", "exactly one withOpenBox initialization is required"))
	} else {
		problems = append(problems, validateInitialization(candidate.Initializations[0])...)
	}
	return compatibility(problems)
}

// ValidateManifests binds the trusted local-clone attestation to the exact
// qualified root package.json and package-lock.json bytes, then applies
// Validate. Registry/consumer package entries are deliberately unsupported.
func ValidateManifests(manifests []inspect.Manifest, source SourceAttestation, initializations []Initialization) Compatibility {
	locks := make([]inspect.Manifest, 0, 1)
	packages := make([]inspect.Manifest, 0, 1)
	for _, manifest := range manifests {
		if manifest.Kind() == inspect.KindPackageLock {
			locks = append(locks, manifest)
		}
		if manifest.Kind() == inspect.KindPackageJSON && manifest.Path() == "package.json" {
			packages = append(packages, manifest)
		}
	}
	problems := make([]Problem, 0)
	if len(locks) != 1 || len(locks) == 1 && locks[0].Path() != "package-lock.json" {
		problems = append(problems, problem("ambiguous_package_lock", "package-lock", "exactly one root package-lock.json is required for the local Mastra MVP source"))
	}
	if len(packages) != 1 {
		problems = append(problems, problem("ambiguous_package_manifest", "package.json", "exactly one root package.json is required for the local Mastra MVP source"))
	}
	if len(problems) != 0 {
		return compatibility(problems)
	}
	resolutions, lockProblems := packagesFromLock(locks[0].Path(), locks[0].Bytes())
	if len(lockProblems) != 0 {
		return compatibility(lockProblems)
	}
	return Validate(Candidate{
		Source: SourceQualification{
			Commit: source.Commit, ArchiveSHA256: source.ArchiveSHA256,
			PackageJSONSHA256: digestHex(packages[0].Digest().String()),
			PackageLockSHA256: digestHex(locks[0].Digest().String()),
		},
		Packages: resolutions, Initializations: append([]Initialization(nil), initializations...),
	})
}

func validateSource(source SourceQualification) []Problem {
	expected := MastraMVP().Source
	if source != expected {
		return []Problem{problem("source_drift", "source", "candidate must match the exact qualified local source commit, archive, package manifest, and lock fingerprints")}
	}
	return nil
}

func validatePackages(packages []PackageResolution) []Problem {
	expected := MastraMVP().Components
	byRequested := make(map[string][]PackageResolution)
	for _, current := range packages {
		byRequested[current.Requested] = append(byRequested[current.Requested], current)
	}
	problems := make([]Problem, 0)
	for _, wanted := range expected {
		matches := byRequested[wanted.Requested]
		field := "packages[" + wanted.Requested + "]"
		delete(byRequested, wanted.Requested)
		if len(matches) != 1 {
			problems = append(problems, problem("package_ambiguity", field, "exactly one locked resolution is required"))
			continue
		}
		if matches[0].Resolved != wanted.Resolved || matches[0].Version != wanted.Version ||
			matches[0].ResolvedURI != wanted.ResolvedURI || matches[0].Integrity != wanted.Integrity {
			problems = append(problems, problem("package_drift", field, fmt.Sprintf("requires %s@%s", wanted.Resolved, wanted.Version)))
		}
	}
	if len(byRequested) != 0 {
		problems = append(problems, problem("unexpected_package", "packages", "candidate contains a package outside the closed MVP tuple"))
	}
	return problems
}

func validateInitialization(initialization Initialization) []Problem {
	problems := make([]Problem, 0)
	if initialization.Function != PublicFactory {
		problems = append(problems, problem("unsupported_initialization", "initialization.function", "public integration must be withOpenBox"))
	}
	if initialization.Target != MastraTarget {
		problems = append(problems, problem("ambiguous_target", "initialization.target", "withOpenBox target must be statically identified as the Mastra instance"))
	}
	if initialization.HasUnclassifiedOptions {
		problems = append(problems, problem("unclassified_options", "initialization.options", "every explicit withOpenBox option must be classified before compatibility can be established"))
	}
	problems = append(problems, validateCoordinate("apiUrl", initialization.APIURL, "OPENBOX_URL")...)
	problems = append(problems, validateCoordinate("apiKey", initialization.APIKey, "OPENBOX_API_KEY")...)
	problems = append(problems, validateControl("validate", initialization.Validate, "true", true)...)
	problems = append(problems, validateControl("onApiError", initialization.OnAPIError, "fail_closed", true)...)
	problems = append(problems, validateControl("sendActivityStartEvent", initialization.SendActivityStartEvent, "true", true)...)
	problems = append(problems, validateControl("evaluateMaxRetries", initialization.EvaluateMaxRetries, "0", false)...)
	problems = append(problems, validateControl("governanceTimeout", initialization.GovernanceTimeout, "5", false)...)
	problems = append(problems, validateControl("hitlEnabled", initialization.HITLEnabled, "true", false)...)
	problems = append(problems, validateControl("httpCapture", initialization.HTTPCapture, "false", false)...)
	problems = append(problems, validateControl("instrumentDatabases", initialization.InstrumentDatabases, "false", false)...)
	problems = append(problems, validateControl("instrumentFileIo", initialization.InstrumentFileIO, "false", false)...)
	problems = append(problems, validateIdentity("agentDid", initialization.AgentDID)...)
	problems = append(problems, validateIdentity("agentPrivateKey", initialization.AgentPrivateKey)...)
	return problems
}

func validateCoordinate(field string, binding CoordinateBinding, environment string) []Problem {
	if binding.Shape == "" {
		binding.Shape = BindingOmitted
	}
	switch binding.Shape {
	case BindingOmitted:
		if binding.Environment == "" {
			return nil
		}
	case BindingEnvironment:
		if binding.Environment == environment {
			return nil
		}
	case BindingLiteral:
		return []Problem{problem("literal_coordinate", "initialization."+field, "coordinate literals are not accepted; use the exact trusted child variable or omit the option")}
	case BindingDynamic:
		return []Problem{problem("dynamic_coordinate", "initialization."+field, "dynamic coordinate binding cannot be proven safe")}
	}
	return []Problem{problem("invalid_coordinate_binding", "initialization."+field, "binding must be omitted or reference exactly "+environment)}
}

func validateControl(field string, binding ControlBinding, literal string, allowOmission bool) []Problem {
	if binding.Shape == "" {
		binding.Shape = BindingOmitted
	}
	switch binding.Shape {
	case BindingOmitted:
		if allowOmission && binding.Literal == "" {
			return nil
		}
	case BindingLiteral:
		if binding.Literal == literal {
			return nil
		}
	case BindingDynamic, BindingEnvironment:
		return []Problem{problem("dynamic_safe_control", "initialization."+field, "control must use the exact qualified literal; only runner-owned safe controls may be omitted")}
	}
	message := "control requires exact literal " + literal
	if allowOmission {
		message += " or omission for trusted runner injection"
	}
	return []Problem{problem("unsafe_control", "initialization."+field, message)}
}

func validateIdentity(field string, binding IdentityBinding) []Problem {
	if binding.Shape == "" {
		binding.Shape = BindingOmitted
	}
	if binding.Shape == BindingOmitted {
		return nil
	}
	return []Problem{problem("signed_identity_unsupported", "initialization."+field, "unsigned MVP requires this option to be omitted")}
}

type packageLock struct {
	Name            string                      `json:"name"`
	Version         string                      `json:"version"`
	LockfileVersion int                         `json:"lockfileVersion"`
	Packages        map[string]packageLockEntry `json:"packages"`
}

type packageLockEntry struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Resolved  string `json:"resolved"`
	Integrity string `json:"integrity"`
}

func packagesFromLock(path string, content []byte) ([]PackageResolution, []Problem) {
	var lock packageLock
	if err := json.Unmarshal(content, &lock); err != nil {
		return nil, []Problem{problem("invalid_package_lock", path, "package-lock.json could not be decoded")}
	}
	if lock.LockfileVersion != 3 || lock.Packages == nil {
		return nil, []Problem{problem("unsupported_package_lock", path, "Mastra MVP requires package-lock v3 with a packages map")}
	}
	adapter, rootSDK := lock.Packages[""]
	if !rootSDK || adapter.Name != MastraPackage || lock.Name != MastraPackage || lock.Version != adapter.Version {
		return nil, []Problem{problem("root_package_drift", path, "root package metadata conflicts with the locked Mastra SDK entry")}
	}
	if _, consumerSDK := lock.Packages["node_modules/"+MastraPackage]; consumerSDK {
		return nil, []Problem{problem("consumer_package_unsupported", path, "registry or consumer Mastra SDK packages are outside the local-clone MVP qualification")}
	}

	base, baseOK := lock.Packages["node_modules/"+BaseAlias]
	core, coreOK := lock.Packages["node_modules/"+MastraCore]
	problems := make([]Problem, 0)
	if !baseOK {
		problems = append(problems, problem("missing_base_sdk", path, "lock lacks the direct @openbox-ai/openbox-sdk alias resolution"))
	}
	if !coreOK {
		problems = append(problems, problem("missing_mastra_core", path, "lock lacks the direct @mastra/core resolution"))
	}
	if len(problems) != 0 {
		return nil, problems
	}
	if base.Name != BasePackage {
		problems = append(problems, problem("base_alias_drift", path, "base alias must resolve to @openbox-ai/openbox-sdk-ts"))
	}
	if core.Name != "" && core.Name != MastraCore {
		problems = append(problems, problem("package_name_drift", path, "Mastra Core package name conflicts with its locked path"))
	}
	if len(problems) != 0 {
		return nil, problems
	}
	return []PackageResolution{
		{Requested: MastraPackage, Resolved: MastraPackage, Version: adapter.Version},
		{Requested: BaseAlias, Resolved: BasePackage, Version: base.Version, ResolvedURI: base.Resolved, Integrity: base.Integrity},
		{Requested: MastraCore, Resolved: MastraCore, Version: core.Version, ResolvedURI: core.Resolved, Integrity: core.Integrity},
	}, nil
}

func digestHex(value string) string {
	const prefix = "sha256:"
	if len(value) > len(prefix) && value[:len(prefix)] == prefix {
		return value[len(prefix):]
	}
	return value
}

func compatibility(problems []Problem) Compatibility {
	sort.Slice(problems, func(left, right int) bool {
		if problems[left].Field != problems[right].Field {
			return problems[left].Field < problems[right].Field
		}
		if problems[left].Code != problems[right].Code {
			return problems[left].Code < problems[right].Code
		}
		return problems[left].Message < problems[right].Message
	})
	status := Compatible
	if len(problems) != 0 {
		status = NotRunnable
	}
	return Compatibility{DescriptorID: MastraDescriptorID, Status: status, Problems: append([]Problem(nil), problems...)}
}

func problem(code, field, message string) Problem {
	return Problem{Code: code, Field: field, Message: message}
}
