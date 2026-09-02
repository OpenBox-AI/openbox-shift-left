package evaluate

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

type systemRandomReader struct{}

func (systemRandomReader) Read(buffer []byte) (int, error) { return rand.Read(buffer) }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

func (realClock) Sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type systemCommandRunner struct{}

func (systemCommandRunner) Run(ctx context.Context, command Command) (CommandResult, error) {
	process, err := startSystemCommand(ctx, command)
	if err != nil {
		return CommandResult{}, err
	}
	result := process.Wait()
	if result.ExitCode != 0 {
		return result, &commandExitError{Code: result.ExitCode}
	}
	return result, nil
}

func (systemCommandRunner) Start(ctx context.Context, command Command) (Process, error) {
	return startSystemCommand(ctx, command)
}

func startSystemCommand(ctx context.Context, command Command) (*systemProcess, error) {
	if command.Name == "" {
		return nil, errors.New("evaluate: empty command")
	}
	cmd := exec.CommandContext(ctx, command.Name, command.Args...)
	if command.Env != nil {
		cmd.Env = append(os.Environ(), command.Env...)
	}
	stdout := &boundedBuffer{limit: maxCaptureBytes}
	stderr := &boundedBuffer{limit: maxCaptureBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &systemProcess{cmd: cmd, stdout: stdout, stderr: stderr}, nil
}

type systemProcess struct {
	cmd    *exec.Cmd
	stdout *boundedBuffer
	stderr *boundedBuffer
	once   sync.Once
	result CommandResult
}

func (process *systemProcess) Wait() CommandResult {
	process.once.Do(func() {
		err := process.cmd.Wait()
		exitCode := 0
		if err != nil {
			exitCode = -1
			var exitError *exec.ExitError
			if errors.As(err, &exitError) {
				exitCode = exitError.ExitCode()
			}
		}
		process.result = CommandResult{
			Stdout:          process.stdout.Bytes(),
			Stderr:          process.stderr.Bytes(),
			StdoutTruncated: process.stdout.Truncated(),
			StderrTruncated: process.stderr.Truncated(),
			ExitCode:        exitCode,
		}
	})
	return process.result
}

type commandExitError struct{ Code int }

func (err *commandExitError) Error() string { return "command exited non-zero" }

type boundedBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	limit     int64
	truncated bool
}

func (buffer *boundedBuffer) Write(content []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	written := len(content)
	remaining := buffer.limit - int64(buffer.buffer.Len())
	if remaining <= 0 {
		buffer.truncated = buffer.truncated || len(content) > 0
		return written, nil
	}
	if int64(len(content)) > remaining {
		content = content[:remaining]
		buffer.truncated = true
	}
	_, _ = buffer.buffer.Write(content)
	return written, nil
}

func (buffer *boundedBuffer) Bytes() []byte {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return bytes.Clone(buffer.buffer.Bytes())
}

func (buffer *boundedBuffer) Truncated() bool {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.truncated
}

var _ io.Writer = (*boundedBuffer)(nil)
