package workloadauth

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// asWorkloadError unwraps err to the package's typed *Error, failing the
// test when the chain holds none.
func asWorkloadError(t *testing.T, err error) *Error {
	t.Helper()
	var werr *Error
	if !errors.As(err, &werr) {
		t.Fatalf("error %v is not a *workloadauth.Error", err)
	}
	return werr
}

// TestBoundStringNeverSplitsARune a multi-byte character straddling the
// limit is dropped whole rather than cut into an invalid UTF-8 tail.
func TestBoundStringNeverSplitsARune(t *testing.T) {
	got := boundString("ab"+"é", 3) // é is two bytes; the limit falls inside it
	if got != "ab" {
		t.Fatalf("boundString = %q, want %q", got, "ab")
	}
	if !utf8.ValidString(boundString(strings.Repeat("世", 100), 64)) {
		t.Fatal("boundString produced invalid UTF-8")
	}
}
