package acpxadapter

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

type commandRunner interface {
	Run(context.Context, string, []string, string) (string, error)
}

type commandStreamRunner interface {
	RunStream(context.Context, string, []string, string, PromptStreamFunc) (string, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, binary string, args []string, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir

	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("run %s %s: %w%s", binary, strings.Join(args, " "), err, formatCommandOutput(output))
	}
	return string(output), nil
}

func (execRunner) RunStream(ctx context.Context, binary string, args []string, dir string, onOutput PromptStreamFunc) (string, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("open stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("open stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start %s %s: %w", binary, strings.Join(args, " "), err)
	}

	var mu sync.Mutex
	var output strings.Builder
	var callbackErr error
	var wg sync.WaitGroup

	appendOutput := func(text string, stream bool) {
		mu.Lock()
		defer mu.Unlock()

		output.WriteString(text)
		if stream && onOutput != nil && callbackErr == nil {
			callbackErr = onOutput(output.String())
		}
	}

	wg.Add(2)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			appendOutput(scanner.Text()+"\n", true)
		}
		if err := scanner.Err(); err != nil {
			appendOutput(fmt.Sprintf("stdout scan error: %v\n", err), false)
		}
	}()
	go func() {
		defer wg.Done()
		data, err := io.ReadAll(stderr)
		if len(data) > 0 {
			appendOutput(string(data), false)
		}
		if err != nil {
			appendOutput(fmt.Sprintf("stderr read error: %v\n", err), false)
		}
	}()

	wg.Wait()
	waitErr := cmd.Wait()

	result := output.String()
	if callbackErr != nil {
		return result, callbackErr
	}
	if waitErr != nil {
		return result, fmt.Errorf("run %s %s: %w%s", binary, strings.Join(args, " "), waitErr, formatCommandOutput([]byte(result)))
	}
	return result, nil
}

type CLIAdapter struct {
	binary string
	runner commandRunner
}

var _ Adapter = (*CLIAdapter)(nil)

func NewCLIAdapter(binary string, runner commandRunner) *CLIAdapter {
	if strings.TrimSpace(binary) == "" {
		if override := strings.TrimSpace(os.Getenv("SPEXUS_AGENT_ACPX_BIN")); override != "" {
			binary = override
		} else {
			binary = "acpx"
		}
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
	if strings.TrimSpace(req.ProjectPath) == "" {
		return SessionResult{}, fmt.Errorf("project path is required")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return SessionResult{}, fmt.Errorf("prompt is required")
	}

	sessionName, err := validateThreadSession(req.ThreadTS)
	if err != nil {
		return SessionResult{}, err
	}

	if req.ForceNew {
		if err := a.createSession(ctx, req.ProjectPath, sessionName); err != nil {
			return SessionResult{}, err
		}
	}

	output, err := a.run(ctx, req.ProjectPath, "codex", "prompt", "-s", sessionName, req.Prompt)
	if err != nil {
		if !isRecoverablePromptError(err) {
			return SessionResult{}, fmt.Errorf("send prompt to session %q: %w", sessionName, err)
		}
		if recreateErr := a.createSession(ctx, req.ProjectPath, sessionName); recreateErr != nil {
			return SessionResult{}, fmt.Errorf("send prompt to session %q: %w", sessionName, err)
		}

		output, err = a.run(ctx, req.ProjectPath, "codex", "prompt", "-s", sessionName, req.Prompt)
		if err != nil {
			return SessionResult{}, fmt.Errorf("send prompt to session %q: %w", sessionName, err)
		}
	}

	return SessionResult{
		SessionName: sessionName,
		Output:      output,
	}, nil
}

func (a *CLIAdapter) SendPromptStream(ctx context.Context, req SessionRequest, onOutput PromptStreamFunc) (SessionResult, error) {
	if err := ctx.Err(); err != nil {
		return SessionResult{}, err
	}
	if strings.TrimSpace(req.ProjectPath) == "" {
		return SessionResult{}, fmt.Errorf("project path is required")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return SessionResult{}, fmt.Errorf("prompt is required")
	}

	sessionName, err := validateThreadSession(req.ThreadTS)
	if err != nil {
		return SessionResult{}, err
	}

	if req.ForceNew {
		if err := a.createSession(ctx, req.ProjectPath, sessionName); err != nil {
			return SessionResult{}, err
		}
	} else if _, err := a.run(ctx, req.ProjectPath, "codex", "sessions", "ensure", "--name", sessionName); err != nil {
		if createErr := a.createSession(ctx, req.ProjectPath, sessionName); createErr != nil {
			return SessionResult{}, fmt.Errorf("ensure session %q: %w", sessionName, err)
		}
	}

	streamOutput := recoverableAwareStreamFunc(onOutput)
	output, err := a.runStream(ctx, req.ProjectPath, streamOutput, "codex", "prompt", "-s", sessionName, req.Prompt)
	if err != nil {
		if !isRecoverablePromptError(err) {
			return SessionResult{}, fmt.Errorf("send prompt to session %q: %w", sessionName, err)
		}
		if recreateErr := a.createSession(ctx, req.ProjectPath, sessionName); recreateErr != nil {
			return SessionResult{}, fmt.Errorf("send prompt to session %q: %w", sessionName, err)
		}

		output, err = a.runStream(ctx, req.ProjectPath, streamOutput, "codex", "prompt", "-s", sessionName, req.Prompt)
		if err != nil {
			return SessionResult{}, fmt.Errorf("send prompt to session %q: %w", sessionName, err)
		}
	}

	return SessionResult{
		SessionName: sessionName,
		Output:      output,
	}, nil
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

func (a *CLIAdapter) runStream(ctx context.Context, dir string, onOutput PromptStreamFunc, args ...string) (string, error) {
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

	commandArgs := append([]string{"--format", "json", "--json-strict", "--approve-all"}, args...)
	if streamRunner, ok := runner.(commandStreamRunner); ok {
		return streamRunner.RunStream(ctx, a.binary, commandArgs, dir, onOutput)
	}

	output, err := runner.Run(ctx, a.binary, commandArgs, dir)
	if err == nil && onOutput != nil {
		if callbackErr := onOutput(output); callbackErr != nil {
			return output, callbackErr
		}
	}
	return output, err
}

func (a *CLIAdapter) createSession(ctx context.Context, dir, sessionName string) error {
	if _, err := a.run(ctx, dir, "codex", "sessions", "new", "--name", sessionName); err != nil {
		return fmt.Errorf("create session %q: %w", sessionName, err)
	}
	return nil
}

func recoverableAwareStreamFunc(onOutput PromptStreamFunc) PromptStreamFunc {
	if onOutput == nil {
		return nil
	}
	return func(output string) error {
		if isRecoverablePromptOutput(output) {
			return nil
		}
		return onOutput(output)
	}
}

func isRecoverablePromptError(err error) bool {
	if err == nil {
		return false
	}

	return containsRecoverablePromptMessage(err.Error())
}

func isRecoverablePromptOutput(output string) bool {
	output = strings.TrimSpace(output)
	if output == "" {
		return false
	}
	return containsRecoverablePromptMessage(output) &&
		(strings.Contains(output, `"error"`) || strings.Contains(output, "acpxCode"))
}

func containsRecoverablePromptMessage(message string) bool {
	message = strings.ToLower(message)
	recoverable := []string{
		"no session",
		"no acpx session",
		"resource not found",
		"session queue owner failed to start",
		"queue owner unavailable",
	}
	for _, token := range recoverable {
		if strings.Contains(message, token) {
			return true
		}
	}
	return false
}

func validateThreadSession(threadTS string) (string, error) {
	threadTS = strings.TrimSpace(threadTS)
	if threadTS == "" {
		return "", fmt.Errorf("thread timestamp is required")
	}

	return SessionName(threadTS), nil
}

func formatCommandOutput(output []byte) string {
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return ""
	}
	return ": " + trimmed
}
