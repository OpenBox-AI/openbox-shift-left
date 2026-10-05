package evaluate

import (
	"fmt"
	"sort"
)

// projectEnvironment is one `.env.sandbox` file, classified.
//
// public and secret both reach the guest as ordinary values; the split decides
// only what the run warns about. The governed alternative is a policy
// credential_binding, which the connector key uses and which is unavailable to
// credentials with no endpoint to bind to.
type projectEnvironment struct {
	public map[string]string
	secret map[string]string
}

func newProjectEnvironment() *projectEnvironment {
	return &projectEnvironment{
		public: map[string]string{},
		secret: map[string]string{},
	}
}

// classify files one variable under its own name. Nothing is refused.
//
// The developer no longer marks credentials, so the name decides, and it errs
// toward secret: over-reporting a setting costs one warning line,
// under-reporting would hide a plaintext credential from the developer.
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

// secretNames lists what the run must warn about, in a stable order. Names only;
// a value is never rendered, logged, or recorded.
func (environment *projectEnvironment) secretNames() []string {
	names := make([]string, 0, len(environment.secret))
	for name := range environment.secret {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
