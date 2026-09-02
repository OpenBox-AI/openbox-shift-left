package snapshot

import (
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNormalizeRelativeRejectsCrossPlatformDrivePrefix(t *testing.T) {
	for _, candidate := range []string{"C:/windows", "c:relative"} {
		if normalized, err := normalizeRelative(candidate); err == nil {
			t.Fatalf("accepted drive-prefixed v1 path %q as %q", candidate, normalized)
		}
	}
}

func FuzzNormalizeRelativePath(f *testing.F) {
	for _, seed := range []string{
		"package.json",
		"src/main.ts",
		".",
		"../outside",
		"dir/../outside",
		"dir\\file",
		"/absolute",
		"C:/windows",
		"nul\x00path",
		"é/文件.ts",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, candidate string) {
		normalized, err := normalizeRelative(candidate)
		if len(candidate) >= 2 && ((candidate[0] >= 'A' && candidate[0] <= 'Z') || (candidate[0] >= 'a' && candidate[0] <= 'z')) && candidate[1] == ':' {
			if err == nil {
				t.Fatalf("accepted drive-prefixed v1 path %q as %q", candidate, normalized)
			}
			return
		}
		if err != nil {
			return
		}
		if !utf8.ValidString(normalized) || normalized == "." || normalized == ".." ||
			filepath.IsAbs(normalized) || strings.Contains(normalized, "\\") ||
			strings.Contains(normalized, "//") || strings.HasSuffix(normalized, "/") {
			t.Fatalf("accepted non-normalized path %q from %q", normalized, candidate)
		}
		components := strings.Split(normalized, "/")
		for _, component := range components {
			if component == "" || component == "." || component == ".." {
				t.Fatalf("accepted unsafe component in %q", normalized)
			}
		}
		repeated, err := normalizeRelative(normalized)
		if err != nil || repeated != normalized {
			t.Fatalf("normalization is not idempotent: %q -> %q, %v", normalized, repeated, err)
		}
	})
}
