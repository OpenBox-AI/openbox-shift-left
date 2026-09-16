package evaluate

import (
	"fmt"
	"sort"
	"strings"
)

// The declaration prefixes a project uses in `.env.sandbox`.
//
// They decide how a value is REPORTED, not how it travels — everything here
// reaches the guest as an ordinary environment variable. PUBLIC_ says "this is
// a setting", SECRET_ says "this is a credential and I know the workload gets
// it in plaintext". A bare name is read by shape, erring toward SECRET_.
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
type projectEnvironment struct {
	// public and secret BOTH reach the guest as ordinary environment values.
	// The split is about what the run can honestly say afterwards, not about
	// how the value travels: a secret here is ungoverned, and the pack names it
	// as such in coverage_limitations.
	//
	// The governed alternative is a policy `credential_binding`, where the
	// proxy holds the credential and the workload cannot use it off-policy. The
	// connector key takes that path. It is not available for every credential —
	// non-HTTP protocols, SDKs that sign their own requests, endpoints unknown
	// until runtime — which is why this one exists rather than being refused.
	public map[string]string
	secret map[string]string

	// modelRoute and modelDigest describe what serves inference.local, which is
	// a gateway-side choice this lane can only record.
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

// classify files one declaration.
//
// Nothing is refused. Every declaration reaches the guest as an ordinary
// environment variable; the prefix decides only how the run REPORTS it, and
// reporting is the whole job here. The evidence has to be able to say which
// values were credentials, and the developer is the only one who knows.
//
// A bare credential-shaped name is filed as a secret rather than as public.
// That is the safe direction of error: over-reporting a setting as an
// ungoverned credential costs one line in the pack's limitations, while
// under-reporting a real credential as non-secret would make the pack assert
// something false about the run.
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
		environment.public[guestName] = value
		return nil
	default:
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
}

func (environment *projectEnvironment) declared(name string) bool {
	_, public := environment.public[name]
	_, secret := environment.secret[name]
	return public || secret
}

func (environment *projectEnvironment) entries() int {
	return len(environment.public) + len(environment.secret)
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

// secretNames lists the credentials this run hands the workload in plaintext,
// in a stable order. Names only — a value is never rendered, logged or recorded,
// which is the one thing that stays true whichever channel it took.
func (environment *projectEnvironment) secretNames() []string {
	names := make([]string, 0, len(environment.secret))
	for name := range environment.secret {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
