package main

import (
	"os"
	"path/filepath"
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

	t.Run("a missing or removed subcommand fails with usage and creates nothing", func(t *testing.T) {
		output := filepath.Join(t.TempDir(), "must-not-exist")
		for _, args := range [][]string{
			{"project"},
			// Everything below was a project subcommand before the backend took
			// evaluation over (ADR-0023). None of them has a fallback.
			{"project", "inspect", t.TempDir(), "--output", output},
			{"project", "finalize", "--evaluation", output, "--analysis", output, "--output", output},
			{"project", "verify", output},
			{"project", "report", "--pack", output},
			{"project", "propose", "--pack", output},
			{"project", "test", output, "--output", output},
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
			t.Fatalf("a removed route created output: %v", err)
		}
	})

	t.Run("top-level usage advertises only project evaluate", func(t *testing.T) {
		a, _, errOut := testApp(nil)
		if code := a.run([]string{"help"}); code != exitOK {
			t.Fatalf("exit = %d", code)
		}
		const evaluate = "openbox project evaluate --image IMAGE --env-file FILE --openbox-agent AGENT_ID [--wait]"
		if count := strings.Count(errOut.String(), evaluate); count != 1 {
			t.Fatalf("project evaluate usage count = %d\n%s", count, errOut.String())
		}
		for _, removed := range []string{
			"project inspect", "project finalize", "project verify", "project report", "project propose",
			"project test", "project rerun", "--output", "--sandbox", "--scenario", "--profile",
			"OBSERVATION_PACK", "native-host", "observation pack",
		} {
			if strings.Contains(errOut.String(), removed) {
				t.Fatalf("removed surface %q remains in help\n%s", removed, errOut.String())
			}
		}
	})
}
