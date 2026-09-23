package workloadauth

import (
	"errors"
	"testing"
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
