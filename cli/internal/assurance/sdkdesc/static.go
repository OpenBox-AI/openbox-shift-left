package sdkdesc

import (
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/inspect"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/model"
)

// ValidateStaticProject classifies only the bounded withOpenBox syntax retained
// by the passive graph. The exact local source tuple comes from the qualified
// descriptor; this does not attest installed consumer bytes or execution.
func ValidateStaticProject(graph model.Graph) Compatibility {
	descriptor := MastraMVP()
	packages := make([]PackageResolution, len(descriptor.Components))
	for index, component := range descriptor.Components {
		packages[index] = PackageResolution{
			Requested: component.Requested, Resolved: component.Resolved, Version: component.Version,
			ResolvedURI: component.ResolvedURI, Integrity: component.Integrity,
		}
	}
	source := descriptor.Source
	initializations := graph.Initializations()
	converted := make([]Initialization, len(initializations))
	for index, current := range initializations {
		converted[index] = convertStaticInitialization(current)
	}
	return Validate(Candidate{Source: source, Packages: packages, Initializations: converted})
}

func convertStaticInitialization(source inspect.OpenBoxInitialization) Initialization {
	result := Initialization{
		Function: source.Function, Target: source.Target,
		HasUnclassifiedOptions: source.HasUnclassifiedOptions,
	}
	for _, option := range source.Options {
		switch option.Name {
		case "apiUrl":
			result.APIURL = staticCoordinate(option)
		case "apiKey":
			result.APIKey = staticCoordinate(option)
		case "validate":
			result.Validate = staticControl(option)
		case "onApiError":
			result.OnAPIError = staticControl(option)
		case "sendActivityStartEvent":
			result.SendActivityStartEvent = staticControl(option)
		case "evaluateMaxRetries":
			result.EvaluateMaxRetries = staticControl(option)
		case "governanceTimeout":
			result.GovernanceTimeout = staticControl(option)
		case "hitlEnabled":
			result.HITLEnabled = staticControl(option)
		case "httpCapture":
			result.HTTPCapture = staticControl(option)
		case "instrumentDatabases":
			result.InstrumentDatabases = staticControl(option)
		case "instrumentFileIo":
			result.InstrumentFileIO = staticControl(option)
		case "agentDid":
			result.AgentDID = IdentityBinding{Shape: staticShape(option.Shape)}
		case "agentPrivateKey":
			result.AgentPrivateKey = IdentityBinding{Shape: staticShape(option.Shape)}
		}
	}
	return result
}

func staticCoordinate(source inspect.InitializationOption) CoordinateBinding {
	return CoordinateBinding{Shape: staticShape(source.Shape), Environment: source.Environment}
}

func staticControl(source inspect.InitializationOption) ControlBinding {
	return ControlBinding{Shape: staticShape(source.Shape), Literal: source.Literal}
}

func staticShape(source inspect.InitializationBindingShape) BindingShape {
	switch source {
	case inspect.InitializationBindingEnvironment:
		return BindingEnvironment
	case inspect.InitializationBindingLiteral:
		return BindingLiteral
	default:
		return BindingDynamic
	}
}
