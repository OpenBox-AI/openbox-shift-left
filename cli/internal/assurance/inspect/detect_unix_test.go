//go:build darwin || linux

package inspect

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/snapshot"
)

func TestDetectFindsBoundedEvidenceWithoutExecution(t *testing.T) {
	sentinel := filepath.Join(t.TempDir(), "must-not-exist")
	packageJSON := []byte(fmt.Sprintf(`{
  "name": "fixture",
  "main": "src/index.ts",
  "scripts": {"postinstall": "touch %s"},
  "dependencies": {"@openbox-ai/openbox-mastra-sdk": "1.0.0"}
}`, sentinel))
	python := []byte(fmt.Sprintf(`from openbox_sdk.client import OpenBox
import os, requests as http
from .local import helper
from . import relative_helper
from alpha import item; import beta
open(%q, "w").write("executed")
api_key = os.getenv("ANTHROPIC_API_KEY")
unknown = os.getenv(dynamic_name)
plugin = importlib.import_module(module_name)
known_plugin = __import__("known_plugin")
subprocess.run(["false"])
`, sentinel))
	typescript := []byte(`import { withOpenBox } from "@openbox-ai/openbox-mastra-sdk";
const lazy = require(packageName);
const key = process.env.OPENBOX_API_KEY;
const dynamic = process.env[keyName];
const endpoint = "https://user:secret@example.test:8443/v1?q=token#fragment";
const agent = new Agent();
const tool = createTool({ id: "recordingTool" });
registerApiRoute("/agent", handler);
fetch("https://api.example.test/models");
db.query("select 1");
trace.getTracer("fixture");
axios.get(endpoint);
app.post("/agent", handler);
mystery.get("value");
`)
	copied := copyManifestFixture(t, map[string][]byte{
		"package.json":     packageJSON,
		"pyproject.toml":   []byte("[project]\nname = \"opaque-fixture\"\n"),
		"requirements.txt": []byte("openbox_sdk==1.0.0\n"),
		"src/index.ts":     typescript,
		"src/runtime.py":   python,
	})

	first, err := Detect(copied)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Detect(copied)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Facts(), second.Facts()) || !reflect.DeepEqual(first.Uncertainties(), second.Uncertainties()) {
		t.Fatal("repeated detection changed its result")
	}
	if _, err := os.Lstat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("project source or package script executed: %v", err)
	}

	wants := []struct {
		kind  FactKind
		value string
	}{
		{FactPackageDependency, "@openbox-ai/openbox-mastra-sdk"},
		{FactPackageDependency, "openbox-sdk"},
		{FactPackageImport, "@openbox-ai/openbox-mastra-sdk"},
		{FactPackageImport, "openbox_sdk.client"},
		{FactPackageImport, "os"},
		{FactPackageImport, "requests"},
		{FactPackageImport, ".local"},
		{FactPackageImport, "."},
		{FactPackageImport, "beta"},
		{FactPackageImport, "known_plugin"},
		{FactEntrypoint, "src/index.ts"},
		{FactOpenBoxSDK, "@openbox-ai/openbox-mastra-sdk"},
		{FactAgent, "Agent"},
		{FactTool, "createTool"},
		{FactEnvironmentReference, "OPENBOX_API_KEY"},
		{FactEnvironmentReference, "ANTHROPIC_API_KEY"},
		{FactCredentialBoundary, "OPENBOX_API_KEY"},
		{FactCredentialBoundary, "ANTHROPIC_API_KEY"},
		{FactExternalDestination, "https://example.test:8443"},
		{FactExternalDestination, "https://api.example.test"},
		{FactProcessBoundary, "subprocess.run"},
		{FactPersistenceSink, "db.query"},
		{FactTelemetrySink, "trace.getTracer"},
		{FactNetworkBoundary, "axios.get"},
		{FactEntrypoint, "app.post"},
	}
	facts := first.Facts()
	mastraDependency, found := findFact(facts, FactPackageDependency, "@openbox-ai/openbox-mastra-sdk")
	if !found || mastraDependency.DeclaredValueDigest != artifact.DigestBytes([]byte("1.0.0")) {
		t.Fatalf("package dependency did not retain a non-secret declared-value digest: %#v", mastraDependency)
	}
	if _, found := findFact(facts, FactPackageImport, ".import"); found {
		t.Fatal("dot-only relative import was misclassified as .import")
	}
	for _, want := range wants {
		fact, found := findFact(facts, want.kind, want.value)
		if !found {
			t.Errorf("missing fact %s=%q in %#v", want.kind, want.value, facts)
			continue
		}
		if fact.Evidence.Detector == "" || fact.Evidence.Path == "" || fact.Evidence.Line < 1 || fact.Evidence.Column < 1 {
			t.Errorf("fact lacks exact source evidence: %#v", fact)
		}
		if fact.Evidence.Basis != BasisDeclared && fact.Evidence.Basis != BasisInferred {
			t.Errorf("fact has unsupported basis: %#v", fact)
		}
	}
	for _, fact := range facts {
		if strings.Contains(fact.Value, "secret") || strings.Contains(fact.Value, "/v1") || strings.Contains(fact.Value, "token") || strings.Contains(fact.Value, sentinel) {
			t.Errorf("fact retained credential, URL detail, or executable text: %#v", fact)
		}
	}
	for _, subject := range []string{"ambiguous-http-method", "dynamic-import", "dynamic-environment-reference", "opaque-manifest", "partially-parsed-manifest", "runtime-registration"} {
		if !hasUncertainty(first.Uncertainties(), subject) {
			t.Errorf("missing explicit %q uncertainty: %#v", subject, first.Uncertainties())
		}
	}
}

func TestDetectIgnoresCommentsStringsDocstringsAndRegexps(t *testing.T) {
	copied := copyManifestFixture(t, map[string][]byte{
		"safe.js": []byte(`// import "fake-comment"
/* createTool(); process.env.COMMENT_SECRET */
const text = "createTool() require('fake-string') process.env.STRING_SECRET";
const expression = /fetch\("fake-regexp"\)/;
const template = ` + "`createTool(${computed})`" + `;
if (ready) /createTool\(\)/.test(input);
import unresolved
const later = "fake-after-import";
import broken;
import x from (later = "false-module");
`),
		"safe.py": []byte(`# import fake_comment
"""createTool()
import fake_docstring
os.getenv("DOCSTRING_SECRET")
"""
text = "subprocess.run() import fake_string"
dynamic_url = f"https://{host}/secret"
dynamic_block = f"""createTool({computed})"""
`),
	})
	detection, err := Detect(copied)
	if err != nil {
		t.Fatal(err)
	}
	if facts := detection.Facts(); len(facts) != 0 {
		t.Fatalf("non-code syntax produced facts: %#v", facts)
	}
	if !hasUncertainty(detection.Uncertainties(), "runtime-registration") {
		t.Fatalf("runtime incompleteness was not retained: %#v", detection.Uncertainties())
	}
	for _, subject := range []string{"ambiguous-import-syntax", "ambiguous-slash-expression", "dynamic-string", "dynamic-template-expression"} {
		if !hasUncertainty(detection.Uncertainties(), subject) {
			t.Errorf("missing syntax-specific %q uncertainty: %#v", subject, detection.Uncertainties())
		}
	}
}

func TestDetectReportsExactManifestLocations(t *testing.T) {
	packageJSON := []byte("{\n  \"note\": \"dependencies\",\n  \"foo\": \"unrelated\",\n  \"dependencies\": {\n   \"f\\u006fo\": \"1\"\n  },\n  \"entryNote\": \"src/index.ts\",\n  \"main\": \"src/index.ts\"\n}\n")
	copied := copyManifestFixture(t, map[string][]byte{
		"package.json":     packageJSON,
		"requirements.txt": []byte("   requests==2.0\n"),
	})
	detection, err := Detect(copied)
	if err != nil {
		t.Fatal(err)
	}
	assertLocation := func(kind FactKind, value, path string, line, column int64) {
		t.Helper()
		fact, found := findFact(detection.Facts(), kind, value)
		if !found {
			t.Fatalf("missing %s=%q", kind, value)
		}
		if fact.Evidence.Path != path || fact.Evidence.Line != line || fact.Evidence.Column != column {
			t.Fatalf("%s=%q location = %s:%d:%d, want %s:%d:%d", kind, value, fact.Evidence.Path, fact.Evidence.Line, fact.Evidence.Column, path, line, column)
		}
	}
	assertLocation(FactPackageDependency, "foo", "package.json", 5, 4)
	assertLocation(FactEntrypoint, "src/index.ts", "package.json", 8, 11)
	assertLocation(FactPackageDependency, "requests", "requirements.txt", 1, 4)
}

func TestDetectMarksUnsupportedSourceWithoutClaimingFacts(t *testing.T) {
	copied := copyManifestFixture(t, map[string][]byte{"main.go": []byte("package main\n")})
	detection, err := Detect(copied)
	if err != nil {
		t.Fatal(err)
	}
	if len(detection.Facts()) != 0 {
		t.Fatalf("unsupported source produced facts: %#v", detection.Facts())
	}
	for _, subject := range []string{"runtime-registration", "unsupported-source-language"} {
		if !hasUncertainty(detection.Uncertainties(), subject) {
			t.Errorf("missing %q uncertainty: %#v", subject, detection.Uncertainties())
		}
	}
}

func TestDetectRejectsMalformedAndOversizedSource(t *testing.T) {
	tests := []struct {
		name    string
		content []byte
		want    string
	}{
		{name: "unterminated string", content: []byte(`const value = "open`), want: "unterminated string literal"},
		{name: "unterminated comment", content: []byte(`/* open`), want: "unterminated block comment"},
		{name: "unterminated regexp", content: []byte(`const value = /createTool(`), want: "unterminated regular expression literal"},
		{name: "invalid UTF-8", content: []byte{0xff}, want: "NUL-free UTF-8"},
		{name: "NUL", content: []byte("safe\x00hidden"), want: "NUL-free UTF-8"},
		{name: "oversized", content: []byte(strings.Repeat(" ", int(maxSourceFileBytes)+1)), want: "exceeds 524288 bytes"},
		{name: "too many tokens", content: []byte(strings.Repeat("x ", maxSourceTokens+1)), want: "exceeds 200000 tokens"},
		{name: "too many facts", content: []byte(strings.Repeat("createTool();\n", maxDetectedFacts+1)), want: "detected facts exceed 10000"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copied := copyManifestFixture(t, map[string][]byte{"source.ts": test.content})
			_, err := Detect(copied)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestDetectRejectsUnboundedManifestFactValue(t *testing.T) {
	name := strings.Repeat("a", 4097)
	copied := copyManifestFixture(t, map[string][]byte{"package.json": []byte(fmt.Sprintf(`{"dependencies":{%q:"1"}}`, name))})
	_, err := Detect(copied)
	if err == nil || !strings.Contains(err.Error(), "fact value is outside the bounded UTF-8 contract") {
		t.Fatalf("error = %v, want bounded fact-value rejection", err)
	}
}

func TestDetectSelectedMastraMVPClone(t *testing.T) {
	projectRoot := os.Getenv("OPENBOX_MASTRA_SDK_ROOT")
	if projectRoot == "" {
		t.Skip("set OPENBOX_MASTRA_SDK_ROOT to the qualified local clone for the MVP integration")
	}
	destinationParent := t.TempDir()
	destination := filepath.Join(destinationParent, "snapshot")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	project, err := snapshot.Resolve(projectRoot, snapshot.Boundaries{
		AuditOutput: filepath.Join(projectRoot, ".openbox", "audit", "current"),
		TempParent:  destinationParent,
	})
	if err != nil {
		t.Fatal(err)
	}
	copied, err := project.Copy(destination)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { makeManifestFixtureWritable(destination) })
	detection, err := Detect(copied)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		kind  FactKind
		value string
	}{
		{FactPackageDependency, "@openbox-ai/openbox-sdk"},
		{FactPackageImport, "@openbox-ai/openbox-sdk"},
		{FactTool, "createTool"},
		{FactAgent, "Agent"},
	} {
		if _, found := findFact(detection.Facts(), want.kind, want.value); !found {
			t.Errorf("qualified Mastra clone missing %s=%q", want.kind, want.value)
		}
	}
	if !hasUncertainty(detection.Uncertainties(), "runtime-registration") {
		t.Fatal("qualified Mastra clone was incorrectly treated as statically complete")
	}
}

func findFact(facts []Fact, kind FactKind, value string) (Fact, bool) {
	for _, fact := range facts {
		if fact.Kind == kind && fact.Value == value {
			return fact, true
		}
	}
	return Fact{}, false
}

func hasUncertainty(uncertainties []Uncertainty, subject string) bool {
	for _, uncertainty := range uncertainties {
		if uncertainty.Subject == subject {
			return true
		}
	}
	return false
}
