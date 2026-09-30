package muse

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"
)

// binaryName is the Muse executable, resolved on PATH.
const binaryName = "muse"

// ErrNotOnPath means there is no `muse` executable to run. It is an answer, not
// a fault: a machine may install OpenBox's hooks before Muse itself.
var ErrNotOnPath = errors.New("muse is not on PATH")

// RunResult is one finished `muse` invocation. A non-zero exit is a result, not
// an error: `muse config validate` answers with its exit code.
type RunResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Runner runs the muse binary with args, in dir when it is non-empty. It is the
// seam every `muse` subprocess goes through, so a test never reaches a real
// binary. It returns ErrNotOnPath when there is no binary, and the context's
// error when ctx ended first.
type Runner func(ctx context.Context, dir string, args ...string) (RunResult, error)

// maxRunOutput bounds what a runner keeps of each stream; a verbose or runaway
// muse must not become a memory problem in a diagnostic.
const maxRunOutput = 64 << 10

// ExecRunner is the Runner that runs the real binary.
func ExecRunner(ctx context.Context, dir string, args ...string) (RunResult, error) {
	path, err := exec.LookPath(binaryName)
	if err != nil {
		return RunResult{}, ErrNotOnPath
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = dir
	// A child that ignores the kill must not hold Wait past the deadline.
	cmd.WaitDelay = time.Second
	stdout, stderr := &cappedBuffer{}, &cappedBuffer{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	runErr := cmd.Run()
	res := RunResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return res, ctxErr
	}
	var exit *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exit):
		res.ExitCode = exit.ExitCode()
	default:
		return res, runErr
	}
	return res, nil
}

// cappedBuffer keeps the first maxRunOutput bytes and swallows the rest, so the
// child never blocks on a full pipe.
type cappedBuffer struct{ buf bytes.Buffer }

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := maxRunOutput - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (c *cappedBuffer) Bytes() []byte { return c.buf.Bytes() }
