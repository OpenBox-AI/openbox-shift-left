package main

import (
	"strings"
	"testing"
)

func TestParseProjectInspectArgs(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		want       projectInspectOptions
		wantCode   int
		wantOK     bool
		wantStderr string
	}{
		{name: "defaults", want: projectInspectOptions{path: "."}, wantOK: true},
		{name: "path", args: []string{"fixture"}, want: projectInspectOptions{path: "fixture"}, wantOK: true},
		{name: "documented interspersed output", args: []string{"fixture", "--output", "artifacts"}, want: projectInspectOptions{path: "fixture", output: "artifacts"}, wantOK: true},
		{name: "output before path", args: []string{"--output", "artifacts", "fixture"}, want: projectInspectOptions{path: "fixture", output: "artifacts"}, wantOK: true},
		{name: "equals output", args: []string{"fixture", "--output=artifacts"}, want: projectInspectOptions{path: "fixture", output: "artifacts"}, wantOK: true},
		{name: "dash-prefixed path after delimiter", args: []string{"--", "-fixture"}, want: projectInspectOptions{path: "-fixture"}, wantOK: true},
		{name: "too many paths", args: []string{"one", "two"}, wantCode: exitError, wantStderr: "at most one path"},
		{name: "empty path", args: []string{""}, wantCode: exitError, wantStderr: "path must not be empty"},
		{name: "empty output", args: []string{"--output="}, wantCode: exitError, wantStderr: "--output must not be empty"},
		{name: "missing output value", args: []string{"--output"}, wantCode: exitError, wantStderr: "flag needs an argument"},
		{name: "missing output value after path", args: []string{"fixture", "--output"}, wantCode: exitError, wantStderr: "flag needs an argument"},
		{name: "unknown flag", args: []string{"--execute"}, wantCode: exitError, wantStderr: "flag provided but not defined"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a, out, errOut := testApp(nil)
			got, code, ok := a.parseProjectInspectArgs(test.args)
			if ok != test.wantOK || code != test.wantCode || got != test.want {
				t.Fatalf("got options=%+v code=%d ok=%v, want options=%+v code=%d ok=%v", got, code, ok, test.want, test.wantCode, test.wantOK)
			}
			if out.Len() != 0 {
				t.Fatalf("unexpected stdout %q", out.String())
			}
			if test.wantStderr != "" && !strings.Contains(errOut.String(), test.wantStderr) {
				t.Fatalf("stderr %q does not contain %q", errOut.String(), test.wantStderr)
			}
		})
	}
}

func TestProjectInspectHelp(t *testing.T) {
	want := projectUsage + "  -output string\n    \twrite exactly three local artifacts to DIR (default .openbox/inspect/<inspection-id>)\n"
	for _, args := range [][]string{{"fixture", "--help"}, {"fixture", "--help", "--output"}} {
		a, out, errOut := testApp(nil)
		_, code, ok := a.parseProjectInspectArgs(args)
		if ok || code != exitOK {
			t.Fatalf("%v code=%d ok=%v", args, code, ok)
		}
		if out.Len() != 0 {
			t.Fatalf("%v unexpected stdout %q", args, out.String())
		}
		if errOut.String() != want {
			t.Fatalf("%v help mismatch\nwant:\n%s\ngot:\n%s", args, want, errOut.String())
		}
	}
}
