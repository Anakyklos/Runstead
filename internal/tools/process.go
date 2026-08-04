package tools

import (
	"context"
	"os"
	"os/exec"
	"syscall"
)

type limitedBuffer struct {
	limit int
	total int64
	data  []byte
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	b.total += int64(len(data))
	if len(b.data) < b.limit {
		remaining := b.limit - len(b.data)
		if remaining > len(data) {
			remaining = len(data)
		}
		b.data = append(b.data, data[:remaining]...)
	}
	return len(data), nil
}

func runCommand(ctx context.Context, name string, args []string, dir string, stdoutLimit, stderrLimit int, env []string) CommandResult {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	if env != nil {
		command.Env = append(os.Environ(), env...)
	}
	var stdout, stderr limitedBuffer
	stdout.limit = stdoutLimit
	stderr.limit = stderrLimit
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	result := CommandResult{
		Stdout:      stdout.data,
		Stderr:      stderr.data,
		StdoutBytes: stdout.total,
		StderrBytes: stderr.total,
		ExitCode:    -1,
		Err:         err,
	}
	if command.ProcessState != nil {
		result.ExitCode = command.ProcessState.ExitCode()
		if status, ok := command.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			result.Signal = status.Signal().String()
		}
	}
	return result
}
