package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func TestProjectRouting(t *testing.T) {
	t.Run("help is exact and successful", func(t *testing.T) {
		a, out, errOut := testApp(nil)
		if code := a.run([]string{"project", "help"}); code != exitOK {
			t.Fatalf("exit = %d", code)
		}
		if out.Len() != 0 || errOut.String() != projectUsage {
			t.Fatalf("stdout=%q stderr=%q", out.String(), errOut.String())
		}
	})

	t.Run("missing and unknown subcommands fail without execution", func(t *testing.T) {
		output := filepath.Join(t.TempDir(), "must-not-exist")
		for _, args := range [][]string{
			{"project"},
			{"project", "test", filepath.Join(t.TempDir(), "missing"), "--output", output},
			{"project", "rerun", "--output", output},
		} {
			a, out, errOut := testApp(nil)
			if code := a.run(args); code != exitError {
				t.Fatalf("%v exit = %d", args, code)
			}
			if out.Len() != 0 || !strings.Contains(errOut.String(), projectUsage) {
				t.Fatalf("%v stdout=%q stderr=%q", args, out.String(), errOut.String())
			}
		}
		if _, err := os.Lstat(output); !os.IsNotExist(err) {
			t.Fatalf("removed execution route created output: %v", err)
		}
		a, out, errOut := testApp(nil)
		if code := a.run([]string{"project", "verify"}); code != exitError || out.Len() != 0 || errOut.String() != projectVerifyUsage {
			t.Fatalf("verify missing args: stdout=%q stderr=%q", out.String(), errOut.String())
		}
	})

	t.Run("inspect route writes only the standalone artifacts", func(t *testing.T) {
		if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
			t.Skip("filesystem-only inspection is supported on Darwin and Linux")
		}
		root := filepath.Join(t.TempDir(), "fixture")
		if err := os.MkdirAll(filepath.Join(root, "src"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"dependencies":{"@openbox-ai/openbox-mastra-sdk":"1.0.0"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "src", "index.ts"), []byte(`import "@openbox-ai/openbox-mastra-sdk"; createTool({ id: "recording-tool" });`), 0o600); err != nil {
			t.Fatal(err)
		}
		output := filepath.Join(t.TempDir(), "inspection")
		a, out, errOut := testApp(nil)
		if code := a.run([]string{"project", "inspect", root, "--output", output}); code != exitOK {
			t.Fatalf("exit = %d", code)
		}
		if errOut.Len() != 0 || !strings.Contains(out.String(), "standalone inspection output is not an audit pack") {
			t.Fatalf("stdout=%q stderr=%q", out.String(), errOut.String())
		}
		entries, err := os.ReadDir(output)
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, len(entries))
		for index, entry := range entries {
			names[index] = entry.Name()
			content, readErr := os.ReadFile(filepath.Join(output, entry.Name()))
			if readErr != nil || entry.IsDir() || len(content) == 0 {
				t.Fatalf("artifact %q: bytes=%d error=%v", entry.Name(), len(content), readErr)
			}
		}
		sort.Strings(names)
		if want := []string{"project-model.json", "project-snapshot.json", "sdk-coverage.json"}; !reflect.DeepEqual(names, want) {
			t.Fatalf("inspection files = %v, want %v", names, want)
		}
		if code := a.run([]string{"project", "inspect", root, "--output", output}); code != exitError || !strings.Contains(errOut.String(), "already exists") {
			t.Fatalf("no-clobber exit=%d stderr=%q", code, errOut.String())
		}
	})

	t.Run("default inspection output is excluded and byte stable", func(t *testing.T) {
		if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
			t.Skip("filesystem-only inspection is supported on Darwin and Linux")
		}
		root := filepath.Join(t.TempDir(), "fixture")
		if err := os.MkdirAll(filepath.Join(root, "src"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"dependencies":{"@openbox-ai/openbox-mastra-sdk":"1.0.0"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "src", "index.ts"), []byte(`import "@openbox-ai/openbox-mastra-sdk"; createTool({ id: "recording-tool" });`), 0o600); err != nil {
			t.Fatal(err)
		}
		outputs := make([]string, 0, 2)
		for range 2 {
			a, out, errOut := testApp(nil)
			if code := a.run([]string{"project", "inspect", root}); code != exitOK {
				t.Fatalf("exit=%d stderr=%q", code, errOut.String())
			}
			line := strings.SplitN(out.String(), "\n", 2)[0]
			const prefix = "project inspection written: "
			if !strings.HasPrefix(line, prefix) {
				t.Fatalf("stdout=%q", out.String())
			}
			outputs = append(outputs, strings.TrimPrefix(line, prefix))
		}
		if outputs[0] == outputs[1] {
			t.Fatalf("default outputs reused a directory: %v", outputs)
		}
		for _, name := range []string{"project-model.json", "project-snapshot.json", "sdk-coverage.json"} {
			first, err := os.ReadFile(filepath.Join(outputs[0], name))
			if err != nil {
				t.Fatal(err)
			}
			second, err := os.ReadFile(filepath.Join(outputs[1], name))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(first, second) {
				t.Fatalf("%s changed across repeated default inspection", name)
			}
		}
	})

	t.Run("top-level usage advertises only the implemented project routes", func(t *testing.T) {
		a, _, errOut := testApp(nil)
		if code := a.run([]string{"help"}); code != exitOK {
			t.Fatalf("exit = %d", code)
		}
		if count := strings.Count(errOut.String(), "openbox project inspect [path] [--output DIR]"); count != 1 {
			t.Fatalf("project inspect usage count = %d\n%s", count, errOut.String())
		}
		if count := strings.Count(errOut.String(), "openbox project verify PACK"); count != 1 {
			t.Fatalf("project verify usage count = %d\n%s", count, errOut.String())
		}
		for _, available := range []string{
			"openbox project report --pack DIR [--format markdown|json|sarif]",
			"openbox project propose --pack DIR [--format json|markdown]",
		} {
			if count := strings.Count(errOut.String(), available); count != 1 {
				t.Fatalf("project route usage count for %q = %d\n%s", available, count, errOut.String())
			}
		}
		if count := strings.Count(errOut.String(), "openbox project evaluate --image IMAGE --env-file FILE --openbox-agent AGENT_ID --output DIR"); count != 1 {
			t.Fatalf("project evaluate usage count = %d\n%s", count, errOut.String())
		}
		for _, removed := range []string{"project test", "project rerun", "--sandbox", "--scenario", "--profile"} {
			if strings.Contains(errOut.String(), removed) {
				t.Fatalf("removed execution surface %q remains in help\n%s", removed, errOut.String())
			}
		}
	})
}

func TestProjectInspectFailureRemovesOnlyNewEmptyDefaultParents(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("filesystem-only inspection is supported on Darwin and Linux")
	}
	for _, preexistingOpenBox := range []bool{false, true} {
		t.Run(fmt.Sprintf("preexisting-openbox-%t", preexistingOpenBox), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "fixture")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if preexistingOpenBox {
				if err := os.Mkdir(filepath.Join(root, ".openbox"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("no detectable graph node\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			a, _, stderr := testApp(nil)
			if code := a.run([]string{"project", "inspect", root}); code != exitError {
				t.Fatalf("exit=%d stderr=%q", code, stderr.String())
			}
			if _, err := os.Lstat(filepath.Join(root, ".openbox", "inspect")); !os.IsNotExist(err) {
				t.Fatalf("failed inspection left output scaffold: %v", err)
			}
			_, err := os.Lstat(filepath.Join(root, ".openbox"))
			if (preexistingOpenBox && err != nil) || (!preexistingOpenBox && !os.IsNotExist(err)) {
				t.Fatalf("preexisting .openbox preservation = %v", err)
			}
		})
	}
}
