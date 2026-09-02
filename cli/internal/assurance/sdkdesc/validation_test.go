package sdkdesc

import (
	"encoding/json"
	"testing"
)

func TestPackagesFromLockSupportsOnlyQualifiedRootShape(t *testing.T) {
	content := packageLockFixture(t, true, nil)
	packages, problems := packagesFromLock("package-lock.json", content)
	if len(problems) != 0 {
		t.Fatalf("problems=%#v", problems)
	}
	descriptor := MastraMVP()
	if len(packages) != len(descriptor.Components) {
		t.Fatalf("packages=%#v", packages)
	}
	for index, component := range descriptor.Components {
		want := PackageResolution{
			Requested: component.Requested, Resolved: component.Resolved, Version: component.Version,
			ResolvedURI: component.ResolvedURI, Integrity: component.Integrity,
		}
		if packages[index] != want {
			t.Fatalf("package[%d]=%#v, want %#v", index, packages[index], want)
		}
	}
}

func TestPackagesFromLockRejectsAmbiguityAndDrift(t *testing.T) {
	tests := []struct {
		name string
		edit func(map[string]any)
		code string
	}{
		{name: "old lock", edit: func(lock map[string]any) { lock["lockfileVersion"] = 2 }, code: "unsupported_package_lock"},
		{name: "no packages", edit: func(lock map[string]any) { delete(lock, "packages") }, code: "unsupported_package_lock"},
		{name: "consumer package", edit: func(lock map[string]any) {
			packagesMap(lock)["node_modules/"+MastraPackage] = map[string]any{"version": "1.0.0"}
		}, code: "consumer_package_unsupported"},
		{name: "root name drift", edit: func(lock map[string]any) { lock["name"] = "fixture" }, code: "root_package_drift"},
		{name: "missing alias", edit: func(lock map[string]any) { delete(packagesMap(lock), "node_modules/"+BaseAlias) }, code: "missing_base_sdk"},
		{name: "missing core", edit: func(lock map[string]any) { delete(packagesMap(lock), "node_modules/"+MastraCore) }, code: "missing_mastra_core"},
		{name: "alias target drift", edit: func(lock map[string]any) {
			packagesMap(lock)["node_modules/"+BaseAlias] = map[string]any{"name": BaseAlias, "version": "1.0.1"}
		}, code: "base_alias_drift"},
		{name: "core name drift", edit: func(lock map[string]any) {
			packagesMap(lock)["node_modules/"+MastraCore] = map[string]any{"name": "other", "version": "1.8.0"}
		}, code: "package_name_drift"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := packageLockFixture(t, true, test.edit)
			_, problems := packagesFromLock("package-lock.json", content)
			if !problemSliceHas(problems, test.code) {
				t.Fatalf("problems = %#v, want %q", problems, test.code)
			}
		})
	}
}

func packageLockFixture(t *testing.T, rootSDK bool, edit func(map[string]any)) []byte {
	t.Helper()
	rootName := "fixture"
	rootVersion := "0.0.0"
	packages := map[string]any{
		"":                              map[string]any{"name": rootName, "version": rootVersion},
		"node_modules/" + MastraPackage: map[string]any{"version": "1.0.0"},
		"node_modules/" + BaseAlias: map[string]any{
			"name": BasePackage, "version": "1.0.1",
			"resolved":  "https://registry.npmjs.org/@openbox-ai/openbox-sdk-ts/-/openbox-sdk-ts-1.0.1.tgz",
			"integrity": "sha512-UWQ6EBLJYD5XhF3BSflfRHHcL6PMOFj7ubda7I/TW10aCNWPx7DuxoNH/VqGrAtFb0QIKVgUSHlEyBi+isLGgw==",
		},
		"node_modules/" + MastraCore: map[string]any{
			"version": "1.8.0", "resolved": "https://registry.npmjs.org/@mastra/core/-/core-1.8.0.tgz",
			"integrity": "sha512-AK6Isj21mWlwX1zIZNUxgAQvRfjJmdjsPsKoh1cOvaM+h748S4U48TJ5DsmundSj/8NBeKHmYXqH2RYqwN35nw==",
		},
	}
	if rootSDK {
		rootName = MastraPackage
		rootVersion = "1.0.0"
		packages[""] = map[string]any{"name": rootName, "version": rootVersion}
		delete(packages, "node_modules/"+MastraPackage)
	}
	lock := map[string]any{
		"name": rootName, "version": rootVersion, "lockfileVersion": 3,
		"packages": packages,
	}
	if edit != nil {
		edit(lock)
	}
	content, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func packagesMap(lock map[string]any) map[string]any {
	return lock["packages"].(map[string]any)
}

func problemSliceHas(problems []Problem, code string) bool {
	for _, current := range problems {
		if current.Code == code {
			return true
		}
	}
	return false
}
