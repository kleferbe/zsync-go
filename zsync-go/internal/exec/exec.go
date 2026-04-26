// Package exec provides a unified interface for executing commands
// locally or on a remote host via SSH. Both implementations use
// os/exec under the hood – the SSH variant simply wraps commands
// with the ssh(1) binary and ControlMaster for connection reuse.
package exec

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
)

// Executor runs commands and returns their stdout.
// Implementations exist for local execution and remote execution via SSH.
type Executor interface {
	// Run executes a command and returns its combined stdout.
	// The command string is passed as-is (no shell expansion for Local,
	// wrapped in ssh for Remote).
	Run(ctx context.Context, name string, args ...string) (string, error)

	// RunPipe connects the stdout of a sender command to the stdin of a
	// receiver command. This is used for zfs send | zfs receive pipelines.
	// The sender runs on this executor; the receiver runs on recvExec.
	RunPipe(ctx context.Context, recvExec Executor, sendName string, sendArgs []string, recvName string, recvArgs []string) error

	// Command returns an *osexec.Cmd ready to start. This is the low-level
	// building block used by Run and RunPipe.
	Command(ctx context.Context, name string, args ...string) *osexec.Cmd

	// String returns a human-readable label (e.g. "local" or "ssh root@pve1:22").
	String() string
}

// ---------------------------------------------------------------------------
// LocalExecutor
// ---------------------------------------------------------------------------

// LocalExecutor runs commands on the local machine.
type LocalExecutor struct{}

// NewLocal returns an executor for local command execution.
func NewLocal() *LocalExecutor { return &LocalExecutor{} }

func (l *LocalExecutor) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := l.Command(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	slog.Debug("exec local", "cmd", cmd.String())
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("exec local %q: %w\nstderr: %s", cmd.String(), err, stderr.String())
	}
	return strings.TrimRight(string(out), "\n"), nil
}

func (l *LocalExecutor) Command(ctx context.Context, name string, args ...string) *osexec.Cmd {
	return osexec.CommandContext(ctx, name, args...)
}

func (l *LocalExecutor) RunPipe(ctx context.Context, recvExec Executor, sendName string, sendArgs []string, recvName string, recvArgs []string) error {
	return runPipe(ctx, l, recvExec, sendName, sendArgs, recvName, recvArgs)
}

func (l *LocalExecutor) String() string { return "local" }

// ---------------------------------------------------------------------------
// SSHExecutor
// ---------------------------------------------------------------------------

// SSHExecutor runs commands on a remote host via the ssh(1) binary.
// It uses ControlMaster for connection multiplexing.
type SSHExecutor struct {
	host        string // user@host
	port        int
	controlPath string
}

// NewSSH returns an executor that runs commands on a remote host.
// It sets up ControlMaster options so that all invocations share a
// single TCP connection.
func NewSSH(host string, port int) *SSHExecutor {
	// Build a per-host control socket path inside the user's home dir.
	home, _ := os.UserHomeDir()
	controlDir := filepath.Join(home, ".ssh")
	controlPath := filepath.Join(controlDir, fmt.Sprintf("zsync-%%r@%%h-%%p"))

	return &SSHExecutor{
		host:        host,
		port:        port,
		controlPath: controlPath,
	}
}

// sshArgs returns the base SSH arguments including ControlMaster options.
func (s *SSHExecutor) sshArgs() []string {
	return []string{
		"-oControlMaster=auto",
		"-oControlPath=" + s.controlPath,
		"-oControlPersist=60",
		"-oBatchMode=yes",
		fmt.Sprintf("-p%d", s.port),
		s.host,
	}
}

func (s *SSHExecutor) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := s.Command(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	slog.Debug("exec ssh", "host", s.host, "cmd", cmd.String())
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("exec ssh %s %q: %w\nstderr: %s", s.host, name, err, stderr.String())
	}
	return strings.TrimRight(string(out), "\n"), nil
}

func (s *SSHExecutor) Command(ctx context.Context, name string, args ...string) *osexec.Cmd {
	// Build the remote command as a single shell string.
	remoteCmd := name
	for _, a := range args {
		remoteCmd += " " + shellQuote(a)
	}

	sshCmdArgs := append(s.sshArgs(), remoteCmd)
	return osexec.CommandContext(ctx, "ssh", sshCmdArgs...)
}

func (s *SSHExecutor) RunPipe(ctx context.Context, recvExec Executor, sendName string, sendArgs []string, recvName string, recvArgs []string) error {
	return runPipe(ctx, s, recvExec, sendName, sendArgs, recvName, recvArgs)
}

func (s *SSHExecutor) String() string {
	return fmt.Sprintf("ssh %s:%d", s.host, s.port)
}

// ---------------------------------------------------------------------------
// Shared pipe implementation
// ---------------------------------------------------------------------------

// runPipe connects stdout of the sender to stdin of the receiver.
func runPipe(ctx context.Context, sendExec, recvExec Executor, sendName string, sendArgs []string, recvName string, recvArgs []string) error {
	sendCmd := sendExec.Command(ctx, sendName, sendArgs...)
	recvCmd := recvExec.Command(ctx, recvName, recvArgs...)

	pr, pw := io.Pipe()
	sendCmd.Stdout = pw
	recvCmd.Stdin = pr

	var sendStderr, recvStderr bytes.Buffer
	sendCmd.Stderr = &sendStderr
	recvCmd.Stderr = &recvStderr

	slog.Debug("exec pipe", "send", sendCmd.String(), "recv", recvCmd.String())

	if err := recvCmd.Start(); err != nil {
		return fmt.Errorf("starting receiver %q: %w", recvCmd.String(), err)
	}
	if err := sendCmd.Start(); err != nil {
		_ = recvCmd.Process.Kill()
		return fmt.Errorf("starting sender %q: %w", sendCmd.String(), err)
	}

	// Wait for sender to finish, then close the pipe writer so
	// the receiver sees EOF.
	sendErr := sendCmd.Wait()
	pw.Close()
	recvErr := recvCmd.Wait()

	if sendErr != nil {
		return fmt.Errorf("sender %q failed: %w\nstderr: %s", sendCmd.String(), sendErr, sendStderr.String())
	}
	if recvErr != nil {
		return fmt.Errorf("receiver %q failed: %w\nstderr: %s", recvCmd.String(), recvErr, recvStderr.String())
	}
	return nil
}

// shellQuote wraps a string in single quotes for safe shell transport.
// Single quotes within the string are escaped.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	// No quoting needed for simple alphanumeric/path strings.
	safe := true
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '/' || c == '.' || c == ',' || c == ':' || c == '@' || c == '=') {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
