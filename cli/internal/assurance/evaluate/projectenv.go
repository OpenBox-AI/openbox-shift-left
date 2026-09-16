package evaluate

import (
	"fmt"
	"sort"
)

// The two evaluator directives a project may set in `.env.sandbox`.
//
// These keep an OPENBOX_SANDBOX_ prefix while ordinary variables have none, and
// the asymmetry is deliberate: they are not the project's environment, they are
// instructions to the runner, and an exact-name prefix is what keeps them from
// colliding with a variable the workload actually wants. Nothing else in the
// file is renamed, which is the point — `.env.sandbox` is meant to be a copy of
// the project's own `.env`.
const (
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
	// public and secret BOTH reach the guest as ordinary environment values,
	// under the names the file gives them. The split is about what the run can
	// honestly say afterwards, not about how a value travels: anything in
	// secret is ungoverned, and the pack names it in coverage_limitations.
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

// classify files one variable under its own name.
//
// There is no declaration syntax and nothing is refused. `.env.sandbox` is an
// ordinary dotenv file — a developer copies their `.env` to it and edits what
// the sandbox needs — so every variable reaches the guest exactly as written.
//
// The split is only about what the run can say afterwards. Since the developer
// no longer marks which values are credentials, the run reads the NAME, and the
// heuristic errs toward calling something a credential: over-reporting a
// setting costs one line in the pack's limitations, while under-reporting would
// have the pack assert something false about the run. That asymmetry is why a
// name-shaped guess is acceptable here and would not be acceptable as a
// refusal.
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

// secretNames lists the credential-shaped variables this run hands the workload
// in plaintext, in a stable order. Names only — a value is never rendered,
// logged or recorded, which stays true however the variable was classified.
func (environment *projectEnvironment) secretNames() []string {
	names := make([]string, 0, len(environment.secret))
	for name := range environment.secret {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
