package inspect

import (
	"reflect"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/snapshot"
)

func TestDetectOpenBoxInitializationClassifiesClosedSafeShapeWithoutSecrets(t *testing.T) {
	source := `const governed = withOpenBox(mastra, {
  apiUrl: process.env.OPENBOX_URL,
  apiKey: process.env["OPENBOX_API_KEY"],
  validate: true,
  onApiError: "fail_closed",
  sendActivityStartEvent: true,
  evaluateMaxRetries: 0,
  governanceTimeout: 5,
  hitlEnabled: true,
  httpCapture: false,
  instrumentDatabases: false,
  instrumentFileIo: false
});`
	first := initializationDetection(t, source)
	second := initializationDetection(t, source)
	if !reflect.DeepEqual(first, second) || len(first) != 1 {
		t.Fatalf("initializations = %#v / %#v", first, second)
	}
	initialization := first[0]
	if initialization.Function != "withOpenBox" || initialization.Target != "mastra" || initialization.HasUnclassifiedOptions ||
		initialization.Evidence.Detector != "source-withopenbox-initialization" {
		t.Fatalf("initialization = %#v", initialization)
	}
	if len(initialization.Options) != 11 {
		t.Fatalf("option count = %d, want 11", len(initialization.Options))
	}
	byName := make(map[string]InitializationOption)
	for _, option := range initialization.Options {
		byName[option.Name] = option
	}
	if got := byName["apiUrl"]; got.Shape != InitializationBindingEnvironment || got.Environment != "OPENBOX_URL" {
		t.Fatalf("apiUrl = %#v", got)
	}
	if got := byName["apiKey"]; got.Shape != InitializationBindingEnvironment || got.Environment != "OPENBOX_API_KEY" {
		t.Fatalf("apiKey = %#v", got)
	}
	for name, literal := range map[string]string{
		"validate": "true", "onApiError": "fail_closed", "sendActivityStartEvent": "true",
		"evaluateMaxRetries": "0", "governanceTimeout": "5", "hitlEnabled": "true",
		"httpCapture": "false", "instrumentDatabases": "false", "instrumentFileIo": "false",
	} {
		if got := byName[name]; got.Shape != InitializationBindingLiteral || got.Literal != literal {
			t.Fatalf("%s = %#v", name, got)
		}
	}
}

func TestDetectOpenBoxInitializationFailsClosedWithoutRetainingValues(t *testing.T) {
	const secret = "obx_secret_must_not_survive"
	tests := []struct {
		name   string
		source string
		check  func(*testing.T, OpenBoxInitialization)
	}{
		{
			name:   "literal coordinate and nonmatching control",
			source: `withOpenBox(mastra, { apiKey: "` + secret + `", onApiError: "custom-secret-value" });`,
			check: func(t *testing.T, got OpenBoxInitialization) {
				if got.Options[0].Name != "apiKey" || got.Options[0].Shape != InitializationBindingLiteral || got.Options[0].Literal != "" ||
					got.Options[1].Name != "onApiError" || got.Options[1].Literal != "other" {
					t.Fatalf("options = %#v", got.Options)
				}
			},
		},
		{
			name:   "dynamic object",
			source: `withOpenBox(mastra, options);`,
			check: func(t *testing.T, got OpenBoxInitialization) {
				if !got.HasUnclassifiedOptions || len(got.Options) != 0 {
					t.Fatalf("initialization = %#v", got)
				}
			},
		},
		{
			name:   "unknown and duplicate option",
			source: `withOpenBox(mastra, { validate: true, validate: false, ignoredUrls: ["https://example.invalid"] });`,
			check: func(t *testing.T, got OpenBoxInitialization) {
				if !got.HasUnclassifiedOptions || len(got.Options) != 1 || got.Options[0].Literal != "true" {
					t.Fatalf("initialization = %#v", got)
				}
			},
		},
		{
			name:   "dynamic target",
			source: `withOpenBox(getMastra(), {});`,
			check: func(t *testing.T, got OpenBoxInitialization) {
				if got.Target != "" {
					t.Fatalf("target = %q", got.Target)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			initializations := initializationDetection(t, test.source)
			if len(initializations) != 1 {
				t.Fatalf("initializations = %#v", initializations)
			}
			if strings.Contains(strings.ToLower(strings.TrimSpace(formatInitialization(initializations[0]))), strings.ToLower(secret)) ||
				strings.Contains(formatInitialization(initializations[0]), "custom-secret-value") {
				t.Fatalf("retained sensitive literal: %#v", initializations[0])
			}
			test.check(t, initializations[0])
		})
	}
}

func initializationDetection(t *testing.T, content string) []OpenBoxInitialization {
	t.Helper()
	tokens, err := lexSource([]byte(content), languageTypeScript)
	if err != nil {
		t.Fatal(err)
	}
	builder := detectionBuilder{factKeys: make(map[string]struct{}), uncertaintyKeys: make(map[string]struct{})}
	file := snapshot.File{Path: "src/index.ts", Digest: artifact.DigestBytes([]byte(content)), Size: int64(len(content))}
	builder.detectSource(file, languageTypeScript, tokens)
	detection, err := builder.finish()
	if err != nil {
		t.Fatal(err)
	}
	return detection.Initializations()
}

func formatInitialization(initialization OpenBoxInitialization) string {
	var builder strings.Builder
	builder.WriteString(initialization.Function)
	builder.WriteString(initialization.Target)
	for _, option := range initialization.Options {
		builder.WriteString(option.Name)
		builder.WriteString(string(option.Shape))
		builder.WriteString(option.Environment)
		builder.WriteString(option.Literal)
	}
	return builder.String()
}
