package acpxadapter

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

type commandRunner interface {
	Run(context.Context, string, []string, string) (string, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, binary string, args []string, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir

	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("run %s %s: %w", binary, strings.Join(args, " "), err)
	}
	return string(output), nil
}

type CLIAdapter struct {
	binary string
	runner commandRunner
}

var _ Adapter = (*CLIAdapter)(nil)

func NewCLIAdapter(binary string, runner commandRunner) *CLIAdapter {
	if strings.TrimSpace(binary) == "" {
		binary = "acpx"
	}
	if runner == nil {
		runner = execRunner{}
	}

	return &CLIAdapter{
		binary: binary,
		runner: runner,
	}
}

func (a *CLIAdapter) EnsureSession(ctx context.Context, req SessionRequest) (SessionResult, error) {
	if err := ctx.Err(); err != nil {
		return SessionResult{}, err
	}
	if a == nil {
		return SessionResult{}, fmt.Errorf("acpx adapter is required")
	}
	if strings.TrimSpace(req.ProjectPath) == "" {
		return SessionResult{}, fmt.Errorf("project path is required")
	}

	sessionName, err := validateThreadSession(req.ThreadTS)
	if err != nil {
		return SessionResult{}, err
	}

	output, err := a.run(ctx, req.ProjectPath, "codex", "sessions", "ensure", "--name", sessionName)
	if err != nil {
		return SessionResult{}, fmt.Errorf("ensure session %q: %w", sessionName, err)
	}

	return SessionResult{
		SessionName: sessionName,
		Output:      output,
	}, nil
}

func (a *CLIAdapter) SendPrompt(ctx context.Context, req SessionRequest) (SessionResult, error) {
	if err := ctx.Err(); err != nil {
		return SessionResult{}, err
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return SessionResult{}, fmt.Errorf("prompt is required")
	}

	session, err := a.EnsureSession(ctx, req)
	if err != nil {
		return SessionResult{}, err
	}

	output, err := a.run(ctx, req.ProjectPath, "codex", "prompt", "-s", session.SessionName, req.Prompt)
	if err != nil {
		return SessionResult{}, fmt.Errorf("send prompt to session %q: %w", session.SessionName, err)
	}

	session.Output = output
	return session, nil
}

func (a *CLIAdapter) Status(ctx context.Context, threadTS string) (SessionResult, error) {
	if err := ctx.Err(); err != nil {
		return SessionResult{}, err
	}

	sessionName, err := validateThreadSession(threadTS)
	if err != nil {
		return SessionResult{}, err
	}

	output, err := a.run(ctx, "", "codex", "status", "-s", sessionName)
	if err != nil {
		return SessionResult{}, fmt.Errorf("status session %q: %w", sessionName, err)
	}

	return SessionResult{
		SessionName: sessionName,
		Output:      output,
	}, nil
}

func (a *CLIAdapter) Cancel(ctx context.Context, threadTS string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	sessionName, err := validateThreadSession(threadTS)
	if err != nil {
		return err
	}

	if _, err := a.run(ctx, "", "codex", "cancel", "-s", sessionName); err != nil {
		return fmt.Errorf("cancel session %q: %w", sessionName, err)
	}

	return nil
}

func (a *CLIAdapter) run(ctx context.Context, dir string, args ...string) (string, error) {
	if a == nil {
		return "", fmt.Errorf("acpx adapter is required")
	}
	if strings.TrimSpace(a.binary) == "" {
		return "", fmt.Errorf("acpx binary is required")
	}

	runner := a.runner
	if runner == nil {
		runner = execRunner{}
	}

	// Slack runtime runs ACPX non-interactively, so command/tool permissions must
	// be pre-approved or ACPX will fail the turn with PERMISSION_DENIED.
	commandArgs := append([]string{"--format", "json", "--json-strict", "--approve-all"}, args...)
	return runner.Run(ctx, a.binary, commandArgs, dir)
}

func validateThreadSession(threadTS string) (string, error) {
	threadTS = strings.TrimSpace(threadTS)
	if threadTS == "" {
		return "", fmt.Errorf("thread timestamp is required")
	}

	return SessionName(threadTS), nil
}
