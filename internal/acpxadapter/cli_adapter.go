package acpxadapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

type CommandRunner interface {
	Run(context.Context, string, []string, string) (string, error)
	Start(context.Context, string, []string, string) (RunningCommand, error)
}

type RunningCommand interface {
	Stdout() io.ReadCloser
	Stderr() io.ReadCloser
	Wait() error
	Close() error
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

func (execRunner) Start(ctx context.Context, binary string, args []string, dir string) (RunningCommand, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe for %s %s: %w", binary, strings.Join(args, " "), err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe for %s %s: %w", binary, strings.Join(args, " "), err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s %s: %w", binary, strings.Join(args, " "), err)
	}

	return &execProcess{
		cmd:    cmd,
		stdout: stdout,
		stderr: stderr,
	}, nil
}

type execProcess struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr io.ReadCloser
}

func (p *execProcess) Stdout() io.ReadCloser {
	return p.stdout
}

func (p *execProcess) Stderr() io.ReadCloser {
	return p.stderr
}

func (p *execProcess) Wait() error {
	return p.cmd.Wait()
}

func (p *execProcess) Close() error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	err := p.cmd.Process.Kill()
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

type CLIAdapter struct {
	binary string
	runner CommandRunner
}

var _ Adapter = (*CLIAdapter)(nil)

func NewCLIAdapter(binary string, runner CommandRunner) *CLIAdapter {
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
	stream, err := a.StartPrompt(ctx, req)
	if err != nil {
		return SessionResult{}, err
	}
	defer func() {
		_ = stream.Close()
	}()

	events, err := CollectPromptStream(stream)
	if err != nil {
		return SessionResult{}, err
	}

	output := ""
	if promptStream, ok := stream.(*cliPromptStream); ok {
		output = promptStream.RawOutput()
	}
	if output == "" {
		output = encodePromptEvents(events)
	}

	return SessionResult{
		SessionName: stream.SessionName(),
		Output:      output,
	}, nil
}

func (a *CLIAdapter) StartPrompt(ctx context.Context, req SessionRequest) (PromptStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a == nil {
		return nil, fmt.Errorf("acpx adapter is required")
	}
	if strings.TrimSpace(req.ProjectPath) == "" {
		return nil, fmt.Errorf("project path is required")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("prompt is required")
	}

	sessionName, err := validateThreadSession(req.ThreadTS)
	if err != nil {
		return nil, err
	}

	if req.ForceNew {
		if _, err := a.run(ctx, req.ProjectPath, "codex", "sessions", "new", "--name", sessionName); err != nil {
			return nil, fmt.Errorf("create session %q: %w", sessionName, err)
		}
	} else {
		if _, err := a.EnsureSession(ctx, SessionRequest{
			ProjectPath: req.ProjectPath,
			ThreadTS:    req.ThreadTS,
		}); err != nil {
			return nil, err
		}
	}

	commandArgs, err := a.commandArgs("codex", "prompt", "-s", sessionName, req.Prompt)
	if err != nil {
		return nil, err
	}

	runner := a.runner
	if runner == nil {
		runner = execRunner{}
	}

	command, err := runner.Start(ctx, a.binary, commandArgs, req.ProjectPath)
	if err != nil {
		return nil, fmt.Errorf("send prompt to session %q: %w", sessionName, err)
	}

	return newCLIPromptStream(sessionName, a.binary, commandArgs, command), nil
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

	commandArgs, err := a.commandArgs(args...)
	if err != nil {
		return "", err
	}

	runner := a.runner
	if runner == nil {
		runner = execRunner{}
	}

	return runner.Run(ctx, a.binary, commandArgs, dir)
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

func (a *CLIAdapter) commandArgs(args ...string) ([]string, error) {
	if a == nil {
		return nil, fmt.Errorf("acpx adapter is required")
	}
	if strings.TrimSpace(a.binary) == "" {
		return nil, fmt.Errorf("acpx binary is required")
	}

	// Slack runtime runs ACPX non-interactively, so command/tool permissions must
	// be pre-approved or ACPX will fail the turn with PERMISSION_DENIED.
	return append([]string{"--format", "json", "--json-strict", "--approve-all"}, args...), nil
}

type cliPromptStream struct {
	sessionName string
	binary      string
	args        []string
	command     RunningCommand
	events      chan Event
	done        chan struct{}

	mu        sync.Mutex
	rawOutput strings.Builder
	stderr    strings.Builder
	waitErr   error
	closeErr  error
	closeOnce sync.Once
}

func newCLIPromptStream(sessionName, binary string, args []string, command RunningCommand) *cliPromptStream {
	stream := &cliPromptStream{
		sessionName: sessionName,
		binary:      binary,
		args:        append([]string(nil), args...),
		command:     command,
		events:      make(chan Event, 32),
		done:        make(chan struct{}),
	}
	go stream.run()
	return stream
}

func (s *cliPromptStream) SessionName() string {
	return s.sessionName
}

func (s *cliPromptStream) Events() <-chan Event {
	return s.events
}

func (s *cliPromptStream) Wait() error {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waitErr
}

func (s *cliPromptStream) Close() error {
	s.closeOnce.Do(func() {
		if s.command != nil {
			s.closeErr = s.command.Close()
		}
	})
	return s.closeErr
}

func (s *cliPromptStream) RawOutput() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSuffix(s.rawOutput.String(), "\n")
}

func (s *cliPromptStream) run() {
	defer close(s.done)
	defer close(s.events)

	parser := newEventLineParser()
	var stdoutErr error
	var stderrErr error
	var errMu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		scanner := newLineScanner(s.command.Stdout())
		for scanner.Scan() {
			line := scanner.Text()
			s.appendRawLine(line)

			lineEvents, err := parser.Parse(line)
			if err != nil {
				errMu.Lock()
				if stdoutErr == nil {
					stdoutErr = err
				}
				errMu.Unlock()
				continue
			}
			for _, event := range lineEvents {
				s.events <- event
			}
		}
		if err := scanner.Err(); err != nil {
			errMu.Lock()
			if stdoutErr == nil {
				stdoutErr = err
			}
			errMu.Unlock()
		}
		errMu.Lock()
		currentErr := stdoutErr
		errMu.Unlock()
		if currentErr == nil {
			for _, event := range parser.Finish() {
				s.events <- event
			}
		}
	}()

	go func() {
		defer wg.Done()
		scanner := newLineScanner(s.command.Stderr())
		for scanner.Scan() {
			s.appendStderrLine(scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			errMu.Lock()
			stderrErr = err
			errMu.Unlock()
		}
	}()

	wg.Wait()
	waitErr := s.command.Wait()
	errMu.Lock()
	currentStdoutErr := stdoutErr
	currentStderrErr := stderrErr
	errMu.Unlock()

	s.mu.Lock()
	s.waitErr = s.buildWaitError(waitErr, currentStdoutErr, currentStderrErr)
	s.mu.Unlock()
}

func (s *cliPromptStream) appendRawLine(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rawOutput.WriteString(line)
	s.rawOutput.WriteByte('\n')
}

func (s *cliPromptStream) appendStderrLine(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stderr.WriteString(line)
	s.stderr.WriteByte('\n')
}

func (s *cliPromptStream) buildWaitError(waitErr, stdoutErr, stderrErr error) error {
	if stdoutErr != nil {
		return stdoutErr
	}
	if stderrErr != nil {
		return stderrErr
	}
	if waitErr == nil {
		return nil
	}

	return fmt.Errorf(
		"run %s %s: %w%s",
		s.binary,
		strings.Join(s.args, " "),
		waitErr,
		formatCommandOutput([]byte(s.errorOutput())),
	)
}

func (s *cliPromptStream) errorOutput() string {
	stdout := strings.TrimSpace(s.rawOutput.String())
	stderr := strings.TrimSpace(s.stderr.String())
	switch {
	case stdout != "" && stderr != "":
		return stdout + "\n" + stderr
	case stdout != "":
		return stdout
	default:
		return stderr
	}
}

func newLineScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return scanner
}

func encodePromptEvents(events []Event) string {
	lines := make([]string, 0, len(events))
	for _, event := range events {
		payload, err := jsonMarshal(event)
		if err != nil {
			continue
		}
		lines = append(lines, payload)
	}
	return strings.Join(lines, "\n")
}

func jsonMarshal(event Event) (string, error) {
	data, err := json.Marshal(event)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
