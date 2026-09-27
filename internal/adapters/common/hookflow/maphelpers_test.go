package hookflow

import (
	"reflect"
	"testing"
)

func TestSplitMCPName(t *testing.T) {
	tests := []struct{ in, server, fn string }{
		{"mcp__github__create_issue", "github", "create_issue"},
		{"mcp__memory__create_entities", "memory", "create_entities"},
		{"mcp__srv__ns__deep_tool", "srv", "ns__deep_tool"},
		{"mcp__lonely", "lonely", ""},
		{"mcp__", "", ""},
	}
	for _, tt := range tests {
		s, f := SplitMCPName(tt.in)
		if s != tt.server || f != tt.fn {
			t.Errorf("SplitMCPName(%q) = (%q,%q), want (%q,%q)", tt.in, s, f, tt.server, tt.fn)
		}
	}
}

func TestIsFileSemantic(t *testing.T) {
	for _, sem := range []string{"file_read", "file_write", "file_open", "file_delete"} {
		if !IsFileSemantic(sem) {
			t.Errorf("IsFileSemantic(%q) = false, want true", sem)
		}
	}
	for _, sem := range []string{"", "internal", "llm_tool_call", "file", "FILE_READ"} {
		if IsFileSemantic(sem) {
			t.Errorf("IsFileSemantic(%q) = true, want false", sem)
		}
	}
}

func TestEnumOr(t *testing.T) {
	allowed := map[string]bool{"startup": true, "resume": true}
	if got := EnumOr("resume", allowed); got != "resume" {
		t.Errorf("EnumOr(allowed value) = %q, want %q", got, "resume")
	}
	for _, v := range []string{"", "Resume", "fork"} {
		if got := EnumOr(v, allowed); got != "" {
			t.Errorf("EnumOr(%q) = %q, want empty", v, got)
		}
	}
	if got := EnumOr("x", nil); got != "" {
		t.Errorf("EnumOr with nil allowlist = %q, want empty", got)
	}
}

func TestCompactDropsOnlyEmptyStrings(t *testing.T) {
	var nilMap map[string]any
	in := map[string]any{
		"kept":   "v",
		"empty":  "",
		"zero":   0,
		"false":  false,
		"nil":    nil,
		"nilmap": nilMap,
		"slice":  []string{},
	}
	got := Compact(in)
	want := map[string]any{"kept": "v", "zero": 0, "false": false, "nil": nil, "nilmap": nilMap, "slice": []string{}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Compact = %#v, want %#v", got, want)
	}
	// Compact edits in place and returns the same map, which is what the
	// adapters' metadata builders rely on when they add keys afterwards.
	if reflect.ValueOf(got).Pointer() != reflect.ValueOf(in).Pointer() {
		t.Error("Compact returned a different map; it must edit and return its argument")
	}
}
