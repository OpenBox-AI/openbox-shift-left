package legacyprofile

import (
	"encoding/json"
	"errors"
)

// validateClosedShape enforces the schema's exact, case-sensitive object keys
// before Go's case-insensitive struct decoding. bodyTemplate is intentionally
// opaque here because jsonValue permits arbitrary object keys.
func validateClosedShape(content []byte) error {
	root, err := closedObject(content,
		"apiVersion", "kind", "name", "application", "fixtures", "sdk", "environment", "budgets", "retention")
	if err != nil {
		return err
	}
	application, err := closedObject(root["application"], "protocol", "listen", "readiness", "stimulus")
	if err != nil {
		return err
	}
	if _, err := closedObject(application["listen"], "host", "portEnvironment"); err != nil {
		return err
	}
	if _, err := closedObject(application["readiness"], "method", "path", "expectedStatus", "startupTimeoutMs", "intervalMs"); err != nil {
		return err
	}
	stimulus, err := closedObject(application["stimulus"], "method", "path", "headers", "bodyTemplate", "completion")
	if err != nil {
		return err
	}
	if _, err := closedObject(stimulus["headers"], "content-type"); err != nil {
		return err
	}
	if _, err := closedObject(stimulus["completion"], "kind", "expectedStatuses"); err != nil {
		return err
	}
	fixtures, err := closedObject(root["fixtures"], "poison", "sink", "model")
	if err != nil {
		return err
	}
	if _, err := closedObject(fixtures["poison"], "urlEnvironment"); err != nil {
		return err
	}
	if _, err := closedObject(fixtures["sink"], "urlEnvironment"); err != nil {
		return err
	}
	modelProbe, err := closedObjectAtLeast(fixtures["model"], "mode", "urlEnvironment")
	if err != nil {
		return err
	}
	var mode string
	if err := json.Unmarshal(modelProbe["mode"], &mode); err != nil {
		return errors.New("run profile: invalid closed shape")
	}
	if mode == "deterministic_local" {
		if _, err := closedObject(fixtures["model"], "mode", "urlEnvironment"); err != nil {
			return err
		}
	} else {
		if _, err := closedObject(fixtures["model"], "mode", "urlEnvironment", "bearerEnvironment", "descriptor", "provider", "model", "destination", "pathFamily", "method", "followRedirects", "dataPosture"); err != nil {
			return err
		}
	}
	if _, err := closedObject(root["sdk"], "descriptor", "requiredActionClasses"); err != nil {
		return err
	}
	environment, err := closedObject(root["environment"], "generatedBindings", "static")
	if err != nil {
		return err
	}
	generated, err := rawArray(environment["generatedBindings"])
	if err != nil {
		return err
	}
	for _, binding := range generated {
		if _, err := closedObject(binding, "name", "source"); err != nil {
			return err
		}
	}
	static, err := rawArray(environment["static"])
	if err != nil {
		return err
	}
	for _, binding := range static {
		if _, err := closedObject(binding, "name", "value"); err != nil {
			return err
		}
	}
	if _, err := closedObject(root["budgets"], "maxProcesses", "maxRequests", "maxRequestBytes", "maxDurationMs", "maxStdoutBytes", "maxStderrBytes", "maxInputTokens", "maxOutputTokens", "maxCostUsd", "cleanupGraceMs"); err != nil {
		return err
	}
	if _, err := closedObject(root["retention"], "mode", "rawContent", "persist", "onRedactionFailure"); err != nil {
		return err
	}
	return nil
}

func closedObject(content []byte, keys ...string) (map[string]json.RawMessage, error) {
	object, err := closedObjectAtLeast(content, keys...)
	if err != nil || len(object) != len(keys) {
		return nil, errors.New("run profile: invalid closed shape")
	}
	return object, nil
}

func closedObjectAtLeast(content []byte, keys ...string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(content, &object); err != nil || object == nil {
		return nil, errors.New("run profile: invalid closed shape")
	}
	for _, key := range keys {
		if _, exists := object[key]; !exists {
			return nil, errors.New("run profile: invalid closed shape")
		}
	}
	return object, nil
}

func rawArray(content []byte) ([]json.RawMessage, error) {
	var values []json.RawMessage
	if err := json.Unmarshal(content, &values); err != nil || values == nil {
		return nil, errors.New("run profile: invalid closed shape")
	}
	return values, nil
}
