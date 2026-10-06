package git

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestSessionResolver_Env(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want []string
	}{
		{"single", "sess-A", []string{"sess-A"}},
		{"comma", "sess-A,sess-B", []string{"sess-A", "sess-B"}},
		{"newline", "sess-A\nsess-B\n", []string{"sess-A", "sess-B"}},
		{"whitespace", "  sess-A   sess-B ", []string{"sess-A", "sess-B"}},
		{"dedupe", "sess-A,sess-A,sess-B", []string{"sess-A", "sess-B"}},
		{"empty", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := SessionResolver{Getenv: func(k string) string {
				if k == EnvSession {
					return c.env
				}
				return ""
			}}
			// No EquateEmpty: a resolver returning no ids and one returning an
			// empty slice are different answers about whether it found a session.
			if diff := cmp.Diff(c.want, r.Resolve("")); diff != "" {
				t.Fatalf("Resolve() (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSessionResolver_FileUnion(t *testing.T) {
	env := map[string]string{
		EnvSession:     "sess-A",
		EnvSessionFile: "/state/sessions",
	}
	r := SessionResolver{
		Getenv: func(k string) string { return env[k] },
		ReadFile: func(p string) ([]byte, error) {
			if p == "/state/sessions" {
				return []byte("sess-B\nsess-A\nsess-C\n"), nil
			}
			return nil, fmt.Errorf("no such file")
		},
	}
	got := r.Resolve("")
	want := []string{"sess-A", "sess-B", "sess-C"} // env first, file union, deduped
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Resolve() (-want +got):\n%s", diff)
	}
}

// TestSessionResolver_FileMissingIsBestEffort a missing/unreadable session
// file must not error; observe-only never blocks.
func TestSessionResolver_FileMissingIsBestEffort(t *testing.T) {
	env := map[string]string{EnvSession: "sess-A", EnvSessionFile: "/nope"}
	r := SessionResolver{
		Getenv:   func(k string) string { return env[k] },
		ReadFile: func(string) ([]byte, error) { return nil, fmt.Errorf("boom") },
	}
	if diff := cmp.Diff([]string{"sess-A"}, r.Resolve("")); diff != "" {
		t.Fatalf("Resolve() (-want +got):\n%s", diff)
	}
}

func TestSessionResolver_NoneWhenEmpty(t *testing.T) {
	r := SessionResolver{Getenv: func(string) string { return "" }}
	if got := r.Resolve(""); len(got) != 0 {
		t.Fatalf("Resolve() = %v, want empty", got)
	}
}
