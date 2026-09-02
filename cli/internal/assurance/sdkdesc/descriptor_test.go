package sdkdesc

import (
	"reflect"
	"strings"
	"testing"
)

func TestMastraMVPMatchesQualifiedTupleAndIsDefensive(t *testing.T) {
	descriptor := MastraMVP()
	if descriptor.ID != "@openbox-ai/openbox-mastra-sdk@1.0.0+@openbox-ai/openbox-sdk-ts@1.0.1+@mastra/core@1.8.0" {
		t.Fatalf("descriptor ID = %q", descriptor.ID)
	}
	if descriptor.Source != (SourceQualification{
		Commit: "db9863bd6659f8b1ce6a33903ea61e4e564be38b", ArchiveSHA256: "1e85dfd57ea751846236b01a94972ee58a8569045d9b20788e051092ff6933d1",
		PackageJSONSHA256: "e11a34c50bb2bd4e4b5538fa9e1f5b638e0ce62ba1b4866d37930de6e06934e6",
		PackageLockSHA256: "55f50d943ca7ca386f1e5e0371da02de8e180d9e439c071cb97d0991ba1d2332",
	}) || descriptor.PublicIntegration != "withOpenBox" {
		t.Fatalf("qualified source or integration drifted: %#v", descriptor)
	}
	wantComponents := []Component{
		{Requested: "@openbox-ai/openbox-mastra-sdk", Resolved: "@openbox-ai/openbox-mastra-sdk", Version: "1.0.0"},
		{
			Requested: "@openbox-ai/openbox-sdk", Resolved: "@openbox-ai/openbox-sdk-ts", Version: "1.0.1",
			ResolvedURI: "https://registry.npmjs.org/@openbox-ai/openbox-sdk-ts/-/openbox-sdk-ts-1.0.1.tgz",
			Integrity:   "sha512-UWQ6EBLJYD5XhF3BSflfRHHcL6PMOFj7ubda7I/TW10aCNWPx7DuxoNH/VqGrAtFb0QIKVgUSHlEyBi+isLGgw==",
		},
		{
			Requested: "@mastra/core", Resolved: "@mastra/core", Version: "1.8.0",
			ResolvedURI: "https://registry.npmjs.org/@mastra/core/-/core-1.8.0.tgz",
			Integrity:   "sha512-AK6Isj21mWlwX1zIZNUxgAQvRfjJmdjsPsKoh1cOvaM+h748S4U48TJ5DsmundSj/8NBeKHmYXqH2RYqwN35nw==",
		},
	}
	if !reflect.DeepEqual(descriptor.Components, wantComponents) {
		t.Fatalf("components = %#v, want %#v", descriptor.Components, wantComponents)
	}
	wantConfig := []ConfigRule{
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
	}
	if !reflect.DeepEqual(descriptor.Configuration, wantConfig) {
		t.Fatalf("configuration = %#v, want %#v", descriptor.Configuration, wantConfig)
	}
	if len(descriptor.QualifiedActions) != 1 || descriptor.QualifiedActions[0] != (ActionCoverage{
		ActionClass: "recordingTool", Event: "ActivityStarted", PreEffect: true,
		Route: ReceiverRoute{Method: "POST", Path: "/api/v1/governance/evaluate"},
	}) {
		t.Fatalf("qualified action widened or drifted: %#v", descriptor.QualifiedActions)
	}
	if !reflect.DeepEqual(descriptor.ReadinessProbes, []ReadinessProbe{
		{ID: "sdk-auth", Route: ReceiverRoute{Method: "GET", Path: "/api/v1/auth/validate"}},
		{ID: "recording-tool-pre-effect", Route: ReceiverRoute{Method: "POST", Path: "/api/v1/governance/evaluate"}, Event: ActivityStarted, ActionClass: RecordingTool},
	}) || !reflect.DeepEqual(descriptor.IgnoredEndpoints, []IgnoredEndpointRule{{Option: "ignoredUrls", RequiredSource: "apiUrl", AdditionalAllowed: false}}) ||
		!reflect.DeepEqual(descriptor.UnsupportedReceiverRoutes, []ReceiverRoute{{Method: "POST", Path: "/api/v1/governance/approval"}}) {
		t.Fatalf("readiness, ignored endpoint, or unsupported route metadata drifted: descriptor=%#v", descriptor)
	}
	for _, unsupported := range descriptor.UnsupportedActionClasses {
		if unsupported == RecordingTool {
			t.Fatal("recordingTool is simultaneously qualified and unsupported")
		}
	}
	if len(descriptor.KnownBlindSpots) < 6 {
		t.Fatalf("known blind spots were dropped: %#v", descriptor.KnownBlindSpots)
	}

	descriptor.Components[0].Version = "changed"
	descriptor.Configuration[0].TrustedEnvironment = "changed"
	descriptor.QualifiedActions[0].ActionClass = "changed"
	descriptor.ReadinessProbes[0].ID = "changed"
	descriptor.IgnoredEndpoints[0].Option = "changed"
	descriptor.UnsupportedReceiverRoutes[0].Path = "changed"
	descriptor.KnownBlindSpots[0] = "changed"
	fresh := MastraMVP()
	if fresh.Components[0].Version != "1.0.0" || fresh.Configuration[0].TrustedEnvironment != "OPENBOX_URL" ||
		fresh.QualifiedActions[0].ActionClass != RecordingTool || fresh.ReadinessProbes[0].ID == "changed" ||
		fresh.IgnoredEndpoints[0].Option == "changed" || fresh.UnsupportedReceiverRoutes[0].Path == "changed" || fresh.KnownBlindSpots[0] == "changed" {
		t.Fatal("MastraMVP exposed mutable retained descriptor storage")
	}
}

func TestValidateAcceptsOnlyQualifiedInitializationShapes(t *testing.T) {
	omittedCandidate := qualifiedCandidate(qualifiedInitialization())
	if result := Validate(omittedCandidate); result.Status != Compatible || len(result.Problems) != 0 {
		t.Fatalf("omitted trusted fallbacks rejected: %#v", result)
	}
	explicit := Initialization{
		Function:               PublicFactory,
		Target:                 MastraTarget,
		APIURL:                 CoordinateBinding{Shape: BindingEnvironment, Environment: "OPENBOX_URL"},
		APIKey:                 CoordinateBinding{Shape: BindingEnvironment, Environment: "OPENBOX_API_KEY"},
		Validate:               ControlBinding{Shape: BindingLiteral, Literal: "true"},
		OnAPIError:             ControlBinding{Shape: BindingLiteral, Literal: "fail_closed"},
		SendActivityStartEvent: ControlBinding{Shape: BindingLiteral, Literal: "true"},
		EvaluateMaxRetries:     ControlBinding{Shape: BindingLiteral, Literal: "0"},
		GovernanceTimeout:      ControlBinding{Shape: BindingLiteral, Literal: "5"},
		HITLEnabled:            ControlBinding{Shape: BindingLiteral, Literal: "true"},
		HTTPCapture:            ControlBinding{Shape: BindingLiteral, Literal: "false"},
		InstrumentDatabases:    ControlBinding{Shape: BindingLiteral, Literal: "false"},
		InstrumentFileIO:       ControlBinding{Shape: BindingLiteral, Literal: "false"},
	}
	if result := Validate(qualifiedCandidate(explicit)); result.Status != Compatible || len(result.Problems) != 0 {
		t.Fatalf("exact explicit safe shape rejected: %#v", result)
	}
}

func TestValidateRejectsTupleAndInitializationDrift(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Candidate)
		code string
	}{
		{name: "missing package", edit: func(candidate *Candidate) { candidate.Packages = candidate.Packages[1:] }, code: "package_ambiguity"},
		{name: "duplicate package", edit: func(candidate *Candidate) { candidate.Packages = append(candidate.Packages, candidate.Packages[0]) }, code: "package_ambiguity"},
		{name: "version drift", edit: func(candidate *Candidate) { candidate.Packages[1].Version = "1.0.2" }, code: "package_drift"},
		{name: "alias drift", edit: func(candidate *Candidate) { candidate.Packages[1].Resolved = BaseAlias }, code: "package_drift"},
		{name: "resolution URI drift", edit: func(candidate *Candidate) { candidate.Packages[1].ResolvedURI = "changed" }, code: "package_drift"},
		{name: "integrity drift", edit: func(candidate *Candidate) { candidate.Packages[2].Integrity = "changed" }, code: "package_drift"},
		{name: "extra tuple package", edit: func(candidate *Candidate) {
			candidate.Packages = append(candidate.Packages, PackageResolution{Requested: "obx_live_secret", Resolved: "https://credential.invalid", Version: "1"})
		}, code: "unexpected_package"},
		{name: "source drift", edit: func(candidate *Candidate) { candidate.Source.Commit = "changed" }, code: "source_drift"},
		{name: "archive drift", edit: func(candidate *Candidate) { candidate.Source.ArchiveSHA256 = "changed" }, code: "source_drift"},
		{name: "package manifest drift", edit: func(candidate *Candidate) { candidate.Source.PackageJSONSHA256 = "changed" }, code: "source_drift"},
		{name: "package lock drift", edit: func(candidate *Candidate) { candidate.Source.PackageLockSHA256 = "changed" }, code: "source_drift"},
		{name: "missing initialization", edit: func(candidate *Candidate) { candidate.Initializations = nil }, code: "ambiguous_initialization"},
		{name: "duplicate initialization", edit: func(candidate *Candidate) {
			candidate.Initializations = append(candidate.Initializations, candidate.Initializations[0])
		}, code: "ambiguous_initialization"},
		{name: "wrong public function", edit: func(candidate *Candidate) { candidate.Initializations[0].Function = "OpenBoxPlugin" }, code: "unsupported_initialization"},
		{name: "dynamic target", edit: func(candidate *Candidate) { candidate.Initializations[0].Target = "" }, code: "ambiguous_target"},
		{name: "unclassified option", edit: func(candidate *Candidate) { candidate.Initializations[0].HasUnclassifiedOptions = true }, code: "unclassified_options"},
		{name: "wrong URL environment", edit: func(candidate *Candidate) {
			candidate.Initializations[0].APIURL = CoordinateBinding{Shape: BindingEnvironment, Environment: "OPENBOX_API_URL"}
		}, code: "invalid_coordinate_binding"},
		{name: "literal URL", edit: func(candidate *Candidate) {
			candidate.Initializations[0].APIURL = CoordinateBinding{Shape: BindingLiteral}
		}, code: "literal_coordinate"},
		{name: "dynamic key", edit: func(candidate *Candidate) {
			candidate.Initializations[0].APIKey = CoordinateBinding{Shape: BindingDynamic}
		}, code: "dynamic_coordinate"},
		{name: "unsafe validate", edit: func(candidate *Candidate) {
			candidate.Initializations[0].Validate = ControlBinding{Shape: BindingLiteral, Literal: "false"}
		}, code: "unsafe_control"},
		{name: "control environment expression", edit: func(candidate *Candidate) {
			candidate.Initializations[0].OnAPIError = ControlBinding{Shape: BindingEnvironment}
		}, code: "dynamic_safe_control"},
		{name: "dynamic start event", edit: func(candidate *Candidate) {
			candidate.Initializations[0].SendActivityStartEvent = ControlBinding{Shape: BindingDynamic}
		}, code: "dynamic_safe_control"},
		{name: "retry drift", edit: func(candidate *Candidate) { candidate.Initializations[0].EvaluateMaxRetries.Literal = "2" }, code: "unsafe_control"},
		{name: "timeout omitted", edit: func(candidate *Candidate) { candidate.Initializations[0].GovernanceTimeout = ControlBinding{} }, code: "unsafe_control"},
		{name: "HTTP capture enabled", edit: func(candidate *Candidate) { candidate.Initializations[0].HTTPCapture.Literal = "true" }, code: "unsafe_control"},
		{name: "database instrumentation enabled", edit: func(candidate *Candidate) { candidate.Initializations[0].InstrumentDatabases.Literal = "true" }, code: "unsafe_control"},
		{name: "DID present", edit: func(candidate *Candidate) {
			candidate.Initializations[0].AgentDID = IdentityBinding{Shape: BindingEnvironment}
		}, code: "signed_identity_unsupported"},
		{name: "key present", edit: func(candidate *Candidate) {
			candidate.Initializations[0].AgentPrivateKey = IdentityBinding{Shape: BindingDynamic}
		}, code: "signed_identity_unsupported"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := qualifiedCandidate(qualifiedInitialization())
			test.edit(&candidate)
			result := Validate(candidate)
			if result.Status != NotRunnable || !hasProblem(result, test.code) {
				t.Fatalf("result = %#v, want not_runnable with %q", result, test.code)
			}
			if result.DescriptorID != MastraDescriptorID {
				t.Fatalf("descriptor ID changed on rejection: %q", result.DescriptorID)
			}
		})
	}
}

func TestCompatibilityContainsNoCredentialOrCoordinateValueField(t *testing.T) {
	candidate := qualifiedCandidate(Initialization{
		Function:            PublicFactory,
		Target:              MastraTarget,
		APIURL:              CoordinateBinding{Shape: BindingLiteral},
		APIKey:              CoordinateBinding{Shape: BindingLiteral},
		EvaluateMaxRetries:  ControlBinding{Shape: BindingLiteral, Literal: "0"},
		GovernanceTimeout:   ControlBinding{Shape: BindingLiteral, Literal: "5"},
		HITLEnabled:         ControlBinding{Shape: BindingLiteral, Literal: "true"},
		HTTPCapture:         ControlBinding{Shape: BindingLiteral, Literal: "false"},
		InstrumentDatabases: ControlBinding{Shape: BindingLiteral, Literal: "false"},
		InstrumentFileIO:    ControlBinding{Shape: BindingLiteral, Literal: "false"},
	})
	candidate.Packages = append(candidate.Packages, PackageResolution{
		Requested: "obx_live_secret", Resolved: "https://credential.invalid", Version: "obx_key_hidden",
	})
	result := Validate(candidate)
	text := strings.ToLower(result.DescriptorID)
	for _, current := range result.Problems {
		text += " " + strings.ToLower(current.Code+" "+current.Field+" "+current.Message)
	}
	for _, forbidden := range []string{"obx_test_", "obx_live_", "obx_key_", "http://", "https://"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("compatibility retained a coordinate or credential value marker %q: %s", forbidden, text)
		}
	}
}

func qualifiedCandidate(initialization Initialization) Candidate {
	descriptor := MastraMVP()
	packages := make([]PackageResolution, len(descriptor.Components))
	for index, component := range descriptor.Components {
		packages[index] = PackageResolution{
			Requested: component.Requested, Resolved: component.Resolved, Version: component.Version,
			ResolvedURI: component.ResolvedURI, Integrity: component.Integrity,
		}
	}
	return Candidate{Source: descriptor.Source, Packages: packages, Initializations: []Initialization{initialization}}
}

func qualifiedInitialization() Initialization {
	return Initialization{
		Function:            PublicFactory,
		Target:              MastraTarget,
		EvaluateMaxRetries:  ControlBinding{Shape: BindingLiteral, Literal: "0"},
		GovernanceTimeout:   ControlBinding{Shape: BindingLiteral, Literal: "5"},
		HITLEnabled:         ControlBinding{Shape: BindingLiteral, Literal: "true"},
		HTTPCapture:         ControlBinding{Shape: BindingLiteral, Literal: "false"},
		InstrumentDatabases: ControlBinding{Shape: BindingLiteral, Literal: "false"},
		InstrumentFileIO:    ControlBinding{Shape: BindingLiteral, Literal: "false"},
	}
}

func hasProblem(result Compatibility, code string) bool {
	for _, current := range result.Problems {
		if current.Code == code {
			return true
		}
	}
	return false
}
