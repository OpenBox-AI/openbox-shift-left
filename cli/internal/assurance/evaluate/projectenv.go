package evaluate

import (
	"fmt"
	"sort"
	"strings"
)

// The declaration prefixes a project uses in `.env.sandbox`.
//
// A bare name in that file stays what it always was: a non-secret application
// default. The prefixes exist for the two things a bare name cannot express —
// a value that LOOKS like a credential but deliberately is not one, and a value
// that genuinely is one.
const (
	publicPrefix = "OPENBOX_SANDBOX_PUBLIC_"
	secretPrefix = "OPENBOX_SANDBOX_SECRET_"

	// modelRouteSetting selects whether the host owes an Ollama preflight.
	modelRouteSetting  = "OPENBOX_SANDBOX_MODEL_ROUTE"
	modelDigestSetting = "OPENBOX_SANDBOX_MODEL_DIGEST"

	// ModelRouteLocalOllama preserves the behaviour this lane shipped with: the
	// gateway's inference.local route is served by an Ollama on this host, so
	// the host can and should prove the model is present and cold.
	ModelRouteLocalOllama = "local-ollama"
	// ModelRouteGateway says the route is served by something this host cannot
	// see — OpenAI, Gemini, OpenRouter, a remote Ollama. There is nothing local
	// to preflight, and pretending otherwise would be a fabricated check.
	ModelRouteGateway = "gateway"
)

// projectEnvironment is one `.env.sandbox` file, classified.
//
// The three maps are separate all the way to the wire because they leave this
// host by three different routes, and collapsing them would mean the evaluator
// deciding which route a value takes by inspecting it. The developer decides.
type projectEnvironment struct {
	// public reaches the guest as an ordinary environment value.
	public map[string]string
	// placeholder reaches the guest under a credential-shaped name, carrying a
	// value that is deliberately not a credential. The sandbox service bounds
	// these to short simple tokens.
	placeholder map[string]string
	// secret never reaches the guest as a value at all. Each entry names an
	// OpenShell provider credential; the gateway resolves it and the policy
	// binds it to an endpoint.
	secret map[string]string

	// modelRoute and modelDigest describe what serves inference.local, which is
	// a gateway-side choice this lane can only record.
	modelRoute  string
	modelDigest string
}

func newProjectEnvironment() *projectEnvironment {
	return &projectEnvironment{
		public:      map[string]string{},
		placeholder: map[string]string{},
		secret:      map[string]string{},
		modelRoute:  ModelRouteLocalOllama,
	}
}

// classify files one declaration, returning the guest-visible name.
//
// The rule is positional, not value-based: the prefix says which channel, and
// whether the stripped name looks like a credential then decides between the
// two public channels. An evaluator that sniffed values instead would be
// guessing at the one thing the developer is best placed to state.
func (environment *projectEnvironment) classify(name, value string, line int) error {
	switch {
	case strings.HasPrefix(name, secretPrefix):
		guestName := strings.TrimPrefix(name, secretPrefix)
		if !environmentNamePattern.MatchString(guestName) {
			return fmt.Errorf("project evaluate: line %d declares an invalid secret name %s", line, guestName)
		}
		if environment.declared(guestName) {
			return fmt.Errorf("project evaluate: line %d redeclares %s", line, guestName)
		}
		environment.secret[guestName] = value
		return nil
	case strings.HasPrefix(name, publicPrefix):
		guestName := strings.TrimPrefix(name, publicPrefix)
		if !environmentNamePattern.MatchString(guestName) {
			return fmt.Errorf("project evaluate: line %d declares an invalid public name %s", line, guestName)
		}
		if environment.declared(guestName) {
			return fmt.Errorf("project evaluate: line %d redeclares %s", line, guestName)
		}
		if credentialNamePattern.MatchString(guestName) {
			environment.placeholder[guestName] = value
			return nil
		}
		environment.public[guestName] = value
		return nil
	default:
		if credentialNamePattern.MatchString(name) {
			return fmt.Errorf(
				"project evaluate: line %d uses credential-looking key %s; declare it as %s%s or %s%s",
				line, name, secretPrefix, name, publicPrefix, name)
		}
		if environment.declared(name) {
			return fmt.Errorf("project evaluate: line %d redeclares %s", line, name)
		}
		environment.public[name] = value
		return nil
	}
}

func (environment *projectEnvironment) declared(name string) bool {
	_, public := environment.public[name]
	_, placeholder := environment.placeholder[name]
	_, secret := environment.secret[name]
	return public || placeholder || secret
}

func (environment *projectEnvironment) entries() int {
	return len(environment.public) + len(environment.placeholder) + len(environment.secret)
}

// setting consumes an evaluator-directed setting line, which configures the run
// rather than the guest. Reports whether the name was one.
func (environment *projectEnvironment) setting(name, value string, line int) (bool, error) {
	switch name {
	case modelRouteSetting:
		if value != ModelRouteLocalOllama && value != ModelRouteGateway {
			return true, fmt.Errorf(
				"project evaluate: line %d sets %s to %q; expected %s or %s",
				line, modelRouteSetting, value, ModelRouteLocalOllama, ModelRouteGateway)
		}
		environment.modelRoute = value
		return true, nil
	case modelDigestSetting:
		if !digestPattern.MatchString(value) {
			return true, fmt.Errorf("project evaluate: line %d sets %s to a non-sha256 value", line, modelDigestSetting)
		}
		environment.modelDigest = value
		return true, nil
	default:
		return false, nil
	}
}

// secretNames lists declared secrets in a stable order. Names only — the values
// are never rendered, logged, or recorded.
func (environment *projectEnvironment) secretNames() []string {
	names := make([]string, 0, len(environment.secret))
	for name := range environment.secret {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
