package muse

import (
	"context"
	"errors"
	"testing"
)

func fakeRunner(res RunResult, err error) Runner {
	return func(context.Context, string, ...string) (RunResult, error) { return res, err }
}

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]Version{
		"1.4.0":                        {Major: 1, Minor: 4, Patch: 0},
		"muse 1.4.2\n":                 {Major: 1, Minor: 4, Patch: 2},
		"Muse Code v1.10.3 (abc1234)":  {Major: 1, Minor: 10, Patch: 3},
		"muse 1.4.0-rc.1":              {Major: 1, Minor: 4, Pre: "rc.1"},
		"build 2 of muse 2.0.11 today": {Major: 2, Minor: 0, Patch: 11},
	} {
		got, err := ParseVersion(in)
		if err != nil || got != want {
			t.Errorf("ParseVersion(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "muse", "1.4", "no digits here"} {
		if _, err := ParseVersion(in); err == nil {
			t.Errorf("ParseVersion(%q) accepted", in)
		}
	}
}

func TestCheckVersionTable(t *testing.T) {
	cases := []struct {
		name string
		res  RunResult
		err  error
		want VersionState
	}{
		{"prerelease of the floor is older", RunResult{Stdout: []byte("muse 1.4.0-rc.1")}, nil, VersionTooOld},
		{"prerelease of a later release", RunResult{Stdout: []byte("muse 1.4.1-rc.1")}, nil, VersionSupported},
		{"later prerelease number compares numerically", RunResult{Stdout: []byte("muse 1.4.1-rc.10")}, nil, VersionSupported},
		{"git describe past the floor", RunResult{Stdout: []byte("muse 1.4.0-3-gabc1234")}, nil, VersionSupported},
		{"git describe on a tested tag", RunResult{Stdout: []byte("muse 1.4.1-3-gabc1234-dirty")}, nil, VersionSupported},
		{"prerelease of the untested release", RunResult{Stdout: []byte("muse 1.5.0-dev")}, nil, VersionUntested},
		{"floor", RunResult{Stdout: []byte("muse 1.4.0")}, nil, VersionSupported},
		{"inside range", RunResult{Stdout: []byte("muse 1.4.9")}, nil, VersionSupported},
		{"too old", RunResult{Stdout: []byte("muse 1.3.9")}, nil, VersionTooOld},
		{"much older", RunResult{Stdout: []byte("0.9.0")}, nil, VersionTooOld},
		{"above tested", RunResult{Stdout: []byte("muse 1.5.0")}, nil, VersionUntested},
		{"next major", RunResult{Stdout: []byte("muse 2.0.0")}, nil, VersionUntested},
		{"not on path", RunResult{}, ErrNotOnPath, VersionNotOnPath},
		{"timed out", RunResult{}, context.DeadlineExceeded, VersionUnreadable},
		{"other error", RunResult{}, errors.New("exec format error"), VersionUnreadable},
		{"non zero exit", RunResult{ExitCode: 3, Stdout: []byte("muse 1.4.0")}, nil, VersionUnreadable},
		{"no number", RunResult{Stdout: []byte("muse dev")}, nil, VersionUnreadable},
		{"version on stderr", RunResult{Stderr: []byte("muse 1.4.1")}, nil, VersionSupported},
	}
	for _, c := range cases {
		if got := CheckVersion(fakeRunner(c.res, c.err)); got.State != c.want {
			t.Errorf("%s: state = %v (%s), want %v", c.name, got.State, got.Detail, c.want)
		}
	}
}

func TestCheckVersionAsksForDashDashVersionUnderADeadline(t *testing.T) {
	var got []string
	CheckVersion(func(ctx context.Context, _ string, args ...string) (RunResult, error) {
		got = args
		if _, ok := ctx.Deadline(); !ok {
			t.Error("the version run has no deadline")
		}
		return RunResult{Stdout: []byte("1.4.0")}, nil
	})
	if len(got) != 1 || got[0] != "--version" {
		t.Errorf("args = %v", got)
	}
}

func TestExecRunnerReportsAMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := ExecRunner(context.Background(), "", "--version"); !errors.Is(err, ErrNotOnPath) {
		t.Fatalf("err = %v, want ErrNotOnPath", err)
	}
}

func TestVersionLessIsSemverPrecedence(t *testing.T) {
	// Ascending: each is older than the next (the semver spec's own example).
	order := []string{
		"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta",
		"1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1-rc.2", "1.0.1", "1.1.0",
	}
	var vs []Version
	for _, in := range order {
		v, err := ParseVersion(in)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", in, err)
		}
		vs = append(vs, v)
	}
	for i := range vs {
		for j := range vs {
			if got, want := vs[i].Less(vs[j]), i < j; got != want {
				t.Errorf("%s.Less(%s) = %v, want %v", order[i], order[j], got, want)
			}
		}
	}
}

func TestParseVersionDropsAGitDescribeSuffixButKeepsARealPrerelease(t *testing.T) {
	for in, want := range map[string]Version{
		"1.4.1-3-gabc1234":        {Major: 1, Minor: 4, Patch: 1},
		"1.4.1-12-gabc1234-dirty": {Major: 1, Minor: 4, Patch: 1},
		"1.4.1-rc.3":              {Major: 1, Minor: 4, Patch: 1, Pre: "rc.3"},
		"1.5.0-dev":               {Major: 1, Minor: 5, Pre: "dev"},
	} {
		got, err := ParseVersion(in)
		if err != nil || got != want {
			t.Errorf("ParseVersion(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
}
