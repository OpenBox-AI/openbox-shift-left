package activation

import (
	"regexp"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// credentialKey matches an env key whose value may be a credential. A lane
// owns only URL and switch keys, but the settings env block is shared with
// whatever the developer keeps there, and a key it displaces or restores can
// be a token; the trace's one exclusion is credentials, so those values are
// masked while the key names still say what changed.
var credentialKey = regexp.MustCompile(`(?i)(key|token|secret|password|auth|header|credential)`)

const maskedValue = "[masked]"

func maskEnv(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if credentialKey.MatchString(k) {
			v = maskedValue
		}
		out[k] = v
	}
	return out
}

// maskReplaced masks Applied.Replaced's "KEY: old -> new" entries by key.
func maskReplaced(entries []string) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if key, _, ok := strings.Cut(e, ":"); ok && credentialKey.MatchString(key) {
			e = key + ": " + maskedValue
		}
		out = append(out, e)
	}
	return out
}

// traceEnvChange records one Activate/Deactivate of a lane's settings env
// keys, success or failure, in the activation-step shape every other step
// uses (sysmacos.go's traceActivationStep): outcome ok/failed, the step
// named in detail.
func traceEnvChange(lane Lane, settingsPath, step string, err error, detail map[string]any) {
	detail["step"] = step
	detail["settings_path"] = settingsPath
	r := trace.Record{
		Stage:   trace.StageActivation,
		Lane:    string(lane),
		Outcome: "ok",
		Detail:  detail,
	}
	if err != nil {
		r.Outcome = "failed"
		r.Err = err.Error()
	}
	trace.Emit(r)
}
