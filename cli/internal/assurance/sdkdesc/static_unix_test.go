//go:build darwin || linux

package sdkdesc

import (
	"strings"
	"testing"
)

func TestValidateStaticProjectAcceptsOnlyExactBoundedInitialization(t *testing.T) {
	exact := `import { withOpenBox } from "@openbox-ai/openbox-mastra-sdk";
withOpenBox(mastra, {
  apiUrl: process.env.OPENBOX_URL,
  apiKey: process.env.OPENBOX_API_KEY,
  validate: true,
  onApiError: "fail_closed",
  sendActivityStartEvent: true,
  evaluateMaxRetries: 0,
  governanceTimeout: 5,
  hitlEnabled: true,
  httpCapture: false,
  instrumentDatabases: false,
  instrumentFileIo: false
});
createTool({ id: "recording-tool" });`
	graph := coverageGraphFixture(t, map[string][]byte{
		"package.json": []byte(`{"main":"src/index.ts","dependencies":{"@openbox-ai/openbox-mastra-sdk":"1.0.0"}}`),
		"src/index.ts": []byte(exact),
	})
	if result := ValidateStaticProject(graph); result.Status != Compatible || len(result.Problems) != 0 {
		t.Fatalf("exact static initialization = %#v", result)
	}

	tests := []struct {
		name string
		edit func(string) string
		code string
	}{
		{name: "literal coordinate", edit: func(value string) string {
			return strings.Replace(value, "process.env.OPENBOX_URL", `"https://production.invalid"`, 1)
		}, code: "literal_coordinate"},
		{name: "wrong target", edit: func(value string) string { return strings.Replace(value, "withOpenBox(mastra", "withOpenBox(app", 1) }, code: "ambiguous_target"},
		{name: "unsafe control", edit: func(value string) string {
			return strings.Replace(value, `onApiError: "fail_closed"`, `onApiError: "fail_open"`, 1)
		}, code: "unsafe_control"},
		{name: "dynamic control", edit: func(value string) string { return strings.Replace(value, "validate: true", "validate: configured", 1) }, code: "dynamic_safe_control"},
		{name: "unknown option", edit: func(value string) string {
			return strings.Replace(value, "instrumentFileIo: false", "instrumentFileIo: false, ignoredUrls: []", 1)
		}, code: "unclassified_options"},
		{name: "second initialization", edit: func(value string) string { return value + "\nwithOpenBox(mastra, {});" }, code: "ambiguous_initialization"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			graph := coverageGraphFixture(t, map[string][]byte{
				"package.json": []byte(`{"main":"src/index.ts","dependencies":{"@openbox-ai/openbox-mastra-sdk":"1.0.0"}}`),
				"src/index.ts": []byte(test.edit(exact)),
			})
			result := ValidateStaticProject(graph)
			if result.Status != NotRunnable || !hasProblem(result, test.code) {
				t.Fatalf("result = %#v, want %s", result, test.code)
			}
		})
	}
}

func TestValidateStaticProjectDoesNotBorrowTestOnlyInitialization(t *testing.T) {
	graph := coverageGraphFixture(t, map[string][]byte{
		"package.json": []byte(`{"main":"src/index.ts","dependencies":{"@openbox-ai/openbox-mastra-sdk":"1.0.0"}}`),
		"src/index.ts": []byte(`import { withOpenBox } from "@openbox-ai/openbox-mastra-sdk"; createTool({});`),
	})
	result := ValidateStaticProject(graph)
	if result.Status != NotRunnable || !hasProblem(result, "ambiguous_initialization") {
		t.Fatalf("missing source initialization = %#v", result)
	}
}
