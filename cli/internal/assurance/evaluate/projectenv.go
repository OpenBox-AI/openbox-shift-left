package evaluate

import (
	"fmt"
	"sort"
)

// Runner directives, not project environment. Prefixed so they cannot collide
// with a variable the workload wants; matched exactly and never passed to the
// guest. Everything else in `.env.sandbox` keeps its own name.
const (
	modelRouteSetting  = "OPENBOX_SANDBOX_MODEL_ROUTE"
	modelDigestSetting = "OPENBOX_SANDBOX_MODEL_DIGEST"

	// ModelRouteLocalOllama lets the host prove the model is present and cold.
	// ModelRouteGateway is served by something this host cannot see, so there
	// is nothing local to preflight.
	ModelRouteLocalOllama = "local-ollama"
	ModelRouteGateway     = "gateway"
)

// projectEnvironment is one `.env.sandbox` file, classified.
//
// public and secret both reach the guest as ordinary values; the split decides
// only what the pack discloses. The governed alternative is a policy
// credential_binding, which the connector key uses and which is unavailable to
// credentials with no endpoint to bind to.
type projectEnvironment struct {
	public      map[string]string
	secret      map[string]string
	modelRoute  string
	modelDigest string
}

func newProjectEnvironment() *projectEnvironment {
	return &projectEnvironment{
		public:     map[string]string{},
		secret:     map[string]string{},
		modelRoute: ModelRouteLocalOllama,
	}
}

// classify files one variable under its own name. Nothing is refused.
//
// The developer no longer marks credentials, so the name decides, and it errs
// toward secret: over-reporting a setting costs one line in the pack's
// limitations, under-reporting would make the pack assert something false.
func (environment *projectEnvironment) classify(name, value string, line int) error {
	if environment.declared(name) {
		return fmt.Errorf("project evaluate: line %d redeclares %s", line, name)
	}
	if credentialNamePattern.MatchString(name) {
		environment.secret[name] = value
		return nil
	}
	environment.public[name] = value
	return nil
}

func (environment *projectEnvironment) declared(name string) bool {
	_, public := environment.public[name]
	_, secret := environment.secret[name]
	return public || secret
}

func (environment *projectEnvironment) entries() int {
	return len(environment.public) + len(environment.secret)
}

// setting consumes a runner directive, reporting whether the name was one.
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

// secretNames lists what the pack must disclose, in a stable order. Names only;
// a value is never rendered, logged, or recorded.
func (environment *projectEnvironment) secretNames() []string {
	names := make([]string, 0, len(environment.secret))
	for name := range environment.secret {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
