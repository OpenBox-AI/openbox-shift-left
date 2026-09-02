// Package sdkdesc contains the closed, data-only SDK description used by the
// project assurance MVP. It does not load an SDK, execute project code, or
// establish observed runtime coverage.
package sdkdesc

const (
	MastraDescriptorID      = "@openbox-ai/openbox-mastra-sdk@1.0.0+@openbox-ai/openbox-sdk-ts@1.0.1+@mastra/core@1.8.0"
	MastraSourceCommit      = "db9863bd6659f8b1ce6a33903ea61e4e564be38b"
	MastraArchiveSHA256     = "1e85dfd57ea751846236b01a94972ee58a8569045d9b20788e051092ff6933d1"
	MastraPackageJSONSHA256 = "e11a34c50bb2bd4e4b5538fa9e1f5b638e0ce62ba1b4866d37930de6e06934e6"
	MastraPackageLockSHA256 = "55f50d943ca7ca386f1e5e0371da02de8e180d9e439c071cb97d0991ba1d2332"

	MastraPackage   = "@openbox-ai/openbox-mastra-sdk"
	BaseAlias       = "@openbox-ai/openbox-sdk"
	BasePackage     = "@openbox-ai/openbox-sdk-ts"
	MastraCore      = "@mastra/core"
	PublicFactory   = "withOpenBox"
	MastraTarget    = "mastra"
	RecordingTool   = "recordingTool"
	ActivityStarted = "ActivityStarted"
)

// Component is one exact package resolution in the qualified tuple. Requested
// and Resolved differ only for the npm alias used by the base SDK.
type Component struct {
	Requested   string
	Resolved    string
	Version     string
	ResolvedURI string
	Integrity   string
}

// SourceQualification is the exact local source evidence qualified in
// SE-00-04. Registry publication bytes were not qualified.
type SourceQualification struct {
	Commit            string
	ArchiveSHA256     string
	PackageJSONSHA256 string
	PackageLockSHA256 string
}

// ConfigRule describes one public withOpenBox option without retaining a
// credential or coordinate value.
type ConfigRule struct {
	Option              string
	TrustedEnvironment  string
	RequiredLiteral     string
	AllowOmission       bool
	AllowEnvironmentRef bool
	AllowLiteral        bool
}

// ReceiverRoute is an endpoint observed in the bounded qualification probe.
type ReceiverRoute struct {
	Method string
	Path   string
}

// ActionCoverage is a semantic class directly qualified before its effect.
type ActionCoverage struct {
	ActionClass string
	Event       string
	PreEffect   bool
	Route       ReceiverRoute
}

// ReadinessProbe is a Phase 04 startup observation required before a semantic
// class can be claimed as available. It remains expected, not observed, here.
type ReadinessProbe struct {
	ID          string
	Route       ReceiverRoute
	Event       string
	ActionClass string
}

// IgnoredEndpointRule describes the only endpoint suppression qualified in
// the middleware: its own resolved apiUrl. Caller-added ignoredUrls were not
// exercised and are not accepted by the MVP compatibility validator.
type IgnoredEndpointRule struct {
	Option            string
	RequiredSource    string
	AdditionalAllowed bool
}

// Descriptor is the sole framework SDK description supported by the MVP.
// Returned slices are defensive copies.
type Descriptor struct {
	ID                        string
	Source                    SourceQualification
	PublicIntegration         string
	Components                []Component
	Configuration             []ConfigRule
	ReceiverSubset            []ReceiverRoute
	ReadinessProbes           []ReadinessProbe
	IgnoredEndpoints          []IgnoredEndpointRule
	UnsupportedReceiverRoutes []ReceiverRoute
	QualifiedActions          []ActionCoverage
	UnsupportedActionClasses  []string
	KnownBlindSpots           []string
}

var mastraMVP = Descriptor{
	ID: MastraDescriptorID,
	Source: SourceQualification{
		Commit: MastraSourceCommit, ArchiveSHA256: MastraArchiveSHA256,
		PackageJSONSHA256: MastraPackageJSONSHA256, PackageLockSHA256: MastraPackageLockSHA256,
	},
	PublicIntegration: PublicFactory,
	Components: []Component{
		{Requested: MastraPackage, Resolved: MastraPackage, Version: "1.0.0"},
		{
			Requested: BaseAlias, Resolved: BasePackage, Version: "1.0.1",
			ResolvedURI: "https://registry.npmjs.org/@openbox-ai/openbox-sdk-ts/-/openbox-sdk-ts-1.0.1.tgz",
			Integrity:   "sha512-UWQ6EBLJYD5XhF3BSflfRHHcL6PMOFj7ubda7I/TW10aCNWPx7DuxoNH/VqGrAtFb0QIKVgUSHlEyBi+isLGgw==",
		},
		{
			Requested: MastraCore, Resolved: MastraCore, Version: "1.8.0",
			ResolvedURI: "https://registry.npmjs.org/@mastra/core/-/core-1.8.0.tgz",
			Integrity:   "sha512-AK6Isj21mWlwX1zIZNUxgAQvRfjJmdjsPsKoh1cOvaM+h748S4U48TJ5DsmundSj/8NBeKHmYXqH2RYqwN35nw==",
		},
	},
	Configuration: []ConfigRule{
		{Option: "apiUrl", TrustedEnvironment: "OPENBOX_URL", AllowOmission: true, AllowEnvironmentRef: true},
		{Option: "apiKey", TrustedEnvironment: "OPENBOX_API_KEY", AllowOmission: true, AllowEnvironmentRef: true},
		{Option: "validate", TrustedEnvironment: "OPENBOX_VALIDATE", RequiredLiteral: "true", AllowOmission: true, AllowLiteral: true},
		{Option: "onApiError", TrustedEnvironment: "OPENBOX_GOVERNANCE_POLICY", RequiredLiteral: "fail_closed", AllowOmission: true, AllowLiteral: true},
		{Option: "sendActivityStartEvent", TrustedEnvironment: "OPENBOX_SEND_ACTIVITY_START_EVENT", RequiredLiteral: "true", AllowOmission: true, AllowLiteral: true},
		{Option: "evaluateMaxRetries", RequiredLiteral: "0", AllowLiteral: true},
		{Option: "governanceTimeout", RequiredLiteral: "5", AllowLiteral: true},
		{Option: "hitlEnabled", RequiredLiteral: "true", AllowLiteral: true},
		{Option: "httpCapture", RequiredLiteral: "false", AllowLiteral: true},
		{Option: "instrumentDatabases", RequiredLiteral: "false", AllowLiteral: true},
		{Option: "instrumentFileIo", RequiredLiteral: "false", AllowLiteral: true},
		{Option: "agentDid", AllowOmission: true},
		{Option: "agentPrivateKey", AllowOmission: true},
	},
	ReceiverSubset: []ReceiverRoute{
		{Method: "GET", Path: "/api/v1/auth/validate"},
		{Method: "POST", Path: "/api/v1/governance/evaluate"},
	},
	ReadinessProbes: []ReadinessProbe{
		{ID: "sdk-auth", Route: ReceiverRoute{Method: "GET", Path: "/api/v1/auth/validate"}},
		{
			ID: "recording-tool-pre-effect", Route: ReceiverRoute{Method: "POST", Path: "/api/v1/governance/evaluate"},
			Event: ActivityStarted, ActionClass: RecordingTool,
		},
	},
	IgnoredEndpoints:          []IgnoredEndpointRule{{Option: "ignoredUrls", RequiredSource: "apiUrl", AdditionalAllowed: false}},
	UnsupportedReceiverRoutes: []ReceiverRoute{{Method: "POST", Path: "/api/v1/governance/approval"}},
	QualifiedActions: []ActionCoverage{{
		ActionClass: RecordingTool,
		Event:       ActivityStarted,
		PreEffect:   true,
		Route:       ReceiverRoute{Method: "POST", Path: "/api/v1/governance/evaluate"},
	}},
	UnsupportedActionClasses: []string{
		"agent", "approval", "database", "file", "function", "http", "lifecycle",
		"mcp", "model", "retrieval", "workflow",
	},
	KnownBlindSpots: []string{
		"Package presence and static initialization evidence do not prove that withOpenBox ran.",
		"Only a direct top-level recordingTool ActivityStarted pre-effect event was qualified.",
		"Agent, model, retrieval, HTTP, workflow, lifecycle, approval, database, file, function, and MCP instrumentation were not qualified.",
		"Optional DID signing, approval polling, model/provider execution, agent/workflow wrappers, and hook-level instrumentation were not exercised.",
		"ActivityCompleted omitted activity_type with HITL enabled and is not a qualified semantic action class.",
		"SDK response application against a mock BLOCK is not a real OpenBox decision or blocked outcome.",
		"The SDK direct-tool start payload requires backend-collector and evaluation-contract byte limits.",
	},
}

// MastraMVP returns the exact accepted SDK description. It is data, not a
// registry or extension point.
func MastraMVP() Descriptor {
	result := mastraMVP
	result.Components = append([]Component(nil), mastraMVP.Components...)
	result.Configuration = append([]ConfigRule(nil), mastraMVP.Configuration...)
	result.ReceiverSubset = append([]ReceiverRoute(nil), mastraMVP.ReceiverSubset...)
	result.ReadinessProbes = append([]ReadinessProbe(nil), mastraMVP.ReadinessProbes...)
	result.IgnoredEndpoints = append([]IgnoredEndpointRule(nil), mastraMVP.IgnoredEndpoints...)
	result.UnsupportedReceiverRoutes = append([]ReceiverRoute(nil), mastraMVP.UnsupportedReceiverRoutes...)
	result.QualifiedActions = append([]ActionCoverage(nil), mastraMVP.QualifiedActions...)
	result.UnsupportedActionClasses = append([]string(nil), mastraMVP.UnsupportedActionClasses...)
	result.KnownBlindSpots = append([]string(nil), mastraMVP.KnownBlindSpots...)
	return result
}
