package git

import (
	"fmt"
	"reflect"
	"testing"
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
			if got := r.Resolve(""); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Resolve() = %v, want %v", got, c.want)
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
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %v, want %v", got, want)
	}
}

// A missing/unreadable session file must not error — observe-only never blocks.
func TestSessionResolver_FileMissingIsBestEffort(t *testing.T) {
	env := map[string]string{EnvSession: "sess-A", EnvSessionFile: "/nope"}
	r := SessionResolver{
		Getenv:   func(k string) string { return env[k] },
		ReadFile: func(string) ([]byte, error) { return nil, fmt.Errorf("boom") },
	}
	if got := r.Resolve(""); !reflect.DeepEqual(got, []string{"sess-A"}) {
		t.Fatalf("Resolve() = %v, want [sess-A]", got)
	}
}

func TestSessionResolver_NoneWhenEmpty(t *testing.T) {
	r := SessionResolver{Getenv: func(string) string { return "" }}
	if got := r.Resolve(""); len(got) != 0 {
		t.Fatalf("Resolve() = %v, want empty", got)
	}
}
