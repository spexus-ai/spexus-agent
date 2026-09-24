// Package piadapter runs Pi's JSONL RPC protocol. A stable session file carries
// conversation history between turns; owner runners may keep one idle process.
package piadapter

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/config"
	"github.com/spexus-ai/spexus-agent/internal/harness"
)

type Adapter struct {
	profile  config.AgentProfile
	binary   string
	mu       sync.Mutex
	active   map[string]*promptStream
	resident bool
	idle     *piProcess
	closed   bool
}

type piProcess struct {
	key     string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	scanner *bufio.Scanner
}

func New(profile config.AgentProfile, binary string) (*Adapter, error) {
	if binary == "" {
		binary = os.Getenv("SPEXUS_AGENT_PI_BIN")
	}
	if binary == "" {
		binary = "pi"
	}
	path, err := exec.LookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("Pi executable: %w", err)
	}
	if profile.ID == "" || profile.Workspace == "" || profile.Model == "" || profile.Provider == "" || profile.SystemPrompt == "" || profile.SessionDirectory == "" {
		return nil, errors.New("resolved agent profile is required")
	}
	if err := os.MkdirAll(profile.SessionDirectory, 0700); err != nil {
		return nil, err
	}
	return &Adapter{profile: profile, binary: path, active: make(map[string]*promptStream)}, nil
}

// NewResident keeps one completed RPC process for the next prompt in the same
// session. Other sessions and interrupted turns start from their durable file.
func NewResident(profile config.AgentProfile, binary string) (*Adapter, error) {
	a, err := New(profile, binary)
	if err != nil {
		return nil, err
	}
	a.resident = true
	return a, nil
}

func (a *Adapter) sessionFile(req harness.SessionRequest) string {
	sum := sha256.Sum256([]byte(a.profile.ID + "\x00" + a.profile.Workspace + "\x00" + req.ChannelID + "\x00" + req.ThreadTS))
	return filepath.Join(a.profile.SessionDirectory, hex.EncodeToString(sum[:])+".jsonl")
}

func (a *Adapter) EnsureSession(ctx context.Context, req harness.SessionRequest) (harness.SessionResult, error) {
	if err := ctx.Err(); err != nil {
		return harness.SessionResult{}, err
	}
	if strings.TrimSpace(req.ThreadTS) == "" {
		return harness.SessionResult{}, errors.New("thread timestamp is required")
	}
	return harness.SessionResult{SessionName: harness.SessionName(req.ThreadTS)}, nil
}

func (a *Adapter) StartPrompt(ctx context.Context, req harness.SessionRequest) (harness.PromptStream, error) {
	if _, err := a.EnsureSession(ctx, req); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, errors.New("prompt is required")
	}
	// The profile is authoritative. Never silently run a registered channel's task
	// in a different project's workspace.
	if filepath.Clean(req.ProjectPath) != filepath.Clean(a.profile.Workspace) {
		return nil, fmt.Errorf("channel workspace does not match agent %q workspace", a.profile.ID)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, errors.New("Pi adapter is closed")
	}
	if a.active[req.ThreadTS] != nil {
		return nil, errors.New("Pi thread already has an active turn")
	}
	if a.resident && len(a.active) != 0 {
		return nil, errors.New("resident Pi already has an active turn")
	}
	key := a.sessionFile(req)
	var process *piProcess
	reused := false
	if a.resident && a.idle != nil {
		old := a.idle
		a.idle = nil
		if old.key == key {
			process = old
			reused = true
		} else {
			stopProcess(old)
		}
	}
	if process == nil {
		var err error
		process, err = a.startProcess(ctx, req, key)
		if err != nil {
			return nil, err
		}
	}
	newStream := func(p *piProcess) *promptStream {
		return &promptStream{name: harness.SessionName(req.ThreadTS), cmd: p.cmd, stdin: p.stdin, scanner: p.scanner, resident: a.resident, events: make(chan harness.Event, 64), done: make(chan struct{}), released: make(chan struct{}), ctx: ctx}
	}
	s := newStream(process)
	if err := s.send(map[string]any{"id": "prompt", "type": "prompt", "message": req.Prompt}); err != nil {
		stopProcess(process)
		if !reused || ctx.Err() != nil {
			return nil, fmt.Errorf("send Pi prompt: %w", err)
		}
		// An idle child may have exited between turns. The session file is the
		// authority, so restart it once before reporting a failed owner turn.
		process, err = a.startProcess(ctx, req, key)
		if err != nil {
			return nil, err
		}
		s = newStream(process)
		if err = s.send(map[string]any{"id": "prompt", "type": "prompt", "message": req.Prompt}); err != nil {
			stopProcess(process)
			return nil, fmt.Errorf("send Pi prompt after restart: %w", err)
		}
	}
	a.active[req.ThreadTS] = s
	go s.run(func(keep bool) {
		a.mu.Lock()
		delete(a.active, req.ThreadTS)
		closed := a.closed
		if keep && !closed {
			a.idle = process
		}
		a.mu.Unlock()
		if keep && closed {
			stopProcess(process)
		}
	})
	if a.resident {
		go s.watchCancellation()
	}
	return s, nil
}

func (a *Adapter) startProcess(ctx context.Context, req harness.SessionRequest, key string) (*piProcess, error) {
	args := []string{"--mode", "rpc", "--provider", a.profile.Provider, "--model", a.profile.Model, "--thinking", a.profile.Thinking, "--system-prompt", a.profile.SystemPrompt, "--session", a.sessionFile(req), "--name", a.profile.ID + " / " + req.ThreadTS, "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes", "--offline"}
	if a.profile.Tools != nil {
		if len(a.profile.Tools) == 0 {
			args = append(args, "--no-tools")
		} else {
			args = append(args, "--tools", strings.Join(a.profile.Tools, ","))
		}
	}
	for _, extension := range a.profile.Extensions {
		args = append(args, "--extension", extension)
	}
	var cmd *exec.Cmd
	if a.resident {
		cmd = exec.Command(a.binary, args...)
	} else {
		cmd = exec.CommandContext(ctx, a.binary, args...)
	}
	cmd.Dir = a.profile.Workspace
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	// Pi stderr may contain provider diagnostics. Do not echo credentials or raw
	// provider bodies to a shared Slack thread.
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("start Pi: %w", err)
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 16*1024*1024)
	return &piProcess{key: key, cmd: cmd, stdin: stdin, stdout: stdout, scanner: scanner}, nil
}

func stopProcess(p *piProcess) {
	_ = p.stdin.Close()
	timer := time.AfterFunc(5*time.Second, func() { _ = p.cmd.Process.Kill() })
	_ = p.cmd.Wait()
	timer.Stop()
	_ = p.stdout.Close()
}

func (a *Adapter) Close() error {
	a.mu.Lock()
	a.closed = true
	idle := a.idle
	a.idle = nil
	active := make([]*promptStream, 0, len(a.active))
	for _, s := range a.active {
		active = append(active, s)
	}
	a.mu.Unlock()
	for _, s := range active {
		_ = s.abort()
	}
	if idle != nil {
		stopProcess(idle)
	}
	for _, s := range active {
		<-s.done
	}
	return nil
}

func (a *Adapter) Cancel(ctx context.Context, threadTS string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	s := a.active[threadTS]
	a.mu.Unlock()
	if s == nil {
		return errors.New("no active Pi turn in this runtime; use stop in its Slack thread")
	}
	return s.abort()
}

func (a *Adapter) Status(ctx context.Context, threadTS string) (harness.SessionResult, error) {
	if err := ctx.Err(); err != nil {
		return harness.SessionResult{}, err
	}
	a.mu.Lock()
	active := a.active[threadTS] != nil
	a.mu.Unlock()
	return harness.SessionResult{SessionName: harness.SessionName(threadTS), Output: fmt.Sprintf("Pi active=%t", active)}, nil
}

func (a *Adapter) SendPrompt(ctx context.Context, req harness.SessionRequest) (harness.SessionResult, error) {
	s, err := a.StartPrompt(ctx, req)
	if err != nil {
		return harness.SessionResult{}, err
	}
	defer s.Close()
	var text strings.Builder
	for e := range s.Events() {
		if e.Kind == harness.EventAssistantMessageChunk {
			text.WriteString(e.Text)
		}
	}
	return harness.SessionResult{SessionName: s.SessionName(), Output: text.String()}, s.Wait()
}

type promptStream struct {
	name        string
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	scanner     *bufio.Scanner
	resident    bool
	ctx         context.Context
	events      chan harness.Event
	done        chan struct{}
	released    chan struct{}
	releaseOnce sync.Once
	writeMu     sync.Mutex
	mu          sync.Mutex
	err         error
	cancelled   bool
	completed   bool
	settled     bool
}

func (s *promptStream) watchCancellation() {
	select {
	case <-s.done:
		return
	case <-s.ctx.Done():
		s.mu.Lock()
		settled := s.settled
		s.mu.Unlock()
		if !settled {
			_ = s.abort()
		}
	}
}

func (s *promptStream) SessionName() string          { return s.name }
func (s *promptStream) Events() <-chan harness.Event { return s.events }
func (s *promptStream) Wait() error                  { <-s.done; s.mu.Lock(); defer s.mu.Unlock(); return s.err }

// Completed distinguishes settled model completion from a cancellation arriving
// after output, independently of whether that output is empty or invalid JSON.
func (s *promptStream) Completed() bool {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.completed
}
func (s *promptStream) send(v any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return json.NewEncoder(s.stdin).Encode(v)
}
func (s *promptStream) abort() error {
	s.mu.Lock()
	if s.settled {
		s.mu.Unlock()
		return nil
	}
	s.cancelled = true
	s.mu.Unlock()
	err := s.send(map[string]any{"id": "abort", "type": "abort"})
	go func() {
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
			_ = s.cmd.Process.Kill()
		}
	}()
	return err
}
func (s *promptStream) Close() error {
	select {
	case <-s.done:
		return nil
	default:
	}
	_ = s.abort()
	s.releaseOnce.Do(func() { close(s.released) })
	return nil
}
func (s *promptStream) emit(e harness.Event) {
	select {
	case s.events <- e:
	case <-s.ctx.Done():
	case <-s.released:
	}
}

func (s *promptStream) run(cleanup func(bool)) {
	keep := false
	defer close(s.done)
	defer close(s.events)
	defer func() { cleanup(keep) }()
	s.emit(harness.Event{Kind: harness.EventSessionStarted, SessionName: s.name})
	scanner := s.scanner
	settled := false
	var runErr error
	var modelErr error
	var finalText string
	for scanner.Scan() {
		var r record
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			runErr = errors.New("invalid Pi RPC output")
			break
		}
		switch r.Type {
		case "response":
			if !r.Success && r.Command != "abort" {
				runErr = fmt.Errorf("Pi %s: %s", r.Command, r.Error)
			}
		case "message_update":
			switch r.AssistantMessageEvent.Type {
			case "thinking_delta":
				// Thinking content is private. Report only that the model is active.
				s.emit(harness.Event{Kind: harness.EventAssistantThinking})
			case "text_delta":
				s.emit(harness.Event{Kind: harness.EventAssistantMessageChunk, Text: r.AssistantMessageEvent.Delta})
			}
		case "message_end":
			if r.Message.Role == "assistant" {
				finalText = finalAssistantText(r.Message.Content)
				if r.Message.StopReason == "error" {
					modelErr = fmt.Errorf("Pi model error: %s", r.Message.ErrorMessage)
				} else {
					modelErr = nil
				}
				if r.Message.StopReason == "aborted" {
					s.mu.Lock()
					s.cancelled = true
					s.mu.Unlock()
				}
			}
		case "tool_execution_start":
			s.emit(harness.Event{Kind: harness.EventToolStarted, ToolName: r.ToolName, ToolStatus: "running"})
		case "tool_execution_end":
			status := "completed"
			if r.IsError {
				status = "failed"
			}
			s.emit(harness.Event{Kind: harness.EventToolFinished, ToolName: r.ToolName, ToolStatus: status})
		case "extension_ui_request":
			// No silent approval. Interactive extensions are outside prototype 1.
			if r.Method == "confirm" || r.Method == "select" || r.Method == "input" || r.Method == "editor" {
				_ = s.send(map[string]any{"type": "extension_ui_response", "id": r.ID, "cancelled": true})
				runErr = errors.New("Pi extension requested interactive input; this prototype requires a Slack follow-up")
			}
		case "agent_settled":
			settled = true
		}
		if settled || (r.Type == "response" && !r.Success && r.Command == "prompt") {
			break
		}
	}
	if err := scanner.Err(); err != nil && runErr == nil {
		runErr = fmt.Errorf("read Pi output: %w", err)
	}
	s.mu.Lock()
	// This latch prevents a late context cancellation from sending abort into
	// the idle process after the model has already settled.
	s.settled = settled
	cancelled := s.cancelled
	s.mu.Unlock()
	keep = s.resident && settled && !cancelled && runErr == nil && modelErr == nil && s.ctx.Err() == nil
	var processErr error
	if !keep {
		s.writeMu.Lock()
		_ = s.stdin.Close()
		s.writeMu.Unlock()
		// Bound shutdown even if an extension retains handles after stdin closes.
		timer := time.AfterFunc(5*time.Second, func() { _ = s.cmd.Process.Kill() })
		processErr = s.cmd.Wait()
		timer.Stop()
	}
	if cancelled {
		s.emit(harness.Event{Kind: harness.EventSessionCancelled, Text: "cancelled"})
		runErr = nil
	} else {
		if runErr == nil {
			runErr = modelErr
		}
		if runErr == nil && processErr != nil {
			runErr = fmt.Errorf("Pi process failed: %w", processErr)
		}
		if runErr == nil && !settled {
			runErr = errors.New("Pi exited before completing the turn")
		}
		if runErr != nil {
			s.emit(harness.Event{Kind: harness.EventSessionError, Text: runErr.Error()})
		} else {
			if finalText != "" {
				s.emit(harness.Event{Kind: harness.EventAssistantMessageFinal, Text: finalText})
			}
			s.emit(harness.Event{Kind: harness.EventSessionDone})
		}
	}
	s.mu.Lock()
	s.err = runErr
	s.completed = settled && !cancelled
	s.mu.Unlock()
}

type record struct {
	Type                  string `json:"type"`
	ID                    string `json:"id"`
	Command               string `json:"command"`
	Success               bool   `json:"success"`
	Error                 string `json:"error"`
	Method                string `json:"method"`
	ToolName              string `json:"toolName"`
	IsError               bool   `json:"isError"`
	AssistantMessageEvent struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	} `json:"assistantMessageEvent"`
	Message struct {
		Content      []textBlock `json:"content"`
		Role         string      `json:"role"`
		StopReason   string      `json:"stopReason"`
		ErrorMessage string      `json:"errorMessage"`
	} `json:"message"`
}

// Pi TextContent.textSignature is either a legacy opaque message ID or encoded
// TextSignatureV1 JSON. OpenAI Responses stores commentary and final_answer
// blocks together in one assistant message; only final-answer data is executable.
type textBlock struct {
	Type          string `json:"type"`
	Text          string `json:"text"`
	TextSignature string `json:"textSignature"`
}

func finalAssistantText(content []textBlock) string {
	var final, unphased strings.Builder
	hasFinal := false
	for _, block := range content {
		if block.Type != "text" {
			continue
		}
		phase := ""
		if strings.HasPrefix(block.TextSignature, "{") {
			var signature struct {
				V     int    `json:"v"`
				ID    string `json:"id"`
				Phase string `json:"phase"`
			}
			if json.Unmarshal([]byte(block.TextSignature), &signature) != nil || signature.V != 1 || signature.ID == "" {
				continue
			}
			phase = signature.Phase
		}
		switch phase {
		case "final_answer":
			hasFinal = true
			final.WriteString(block.Text)
		case "":
			unphased.WriteString(block.Text)
			// Commentary and unknown explicit phases must never become final data.
		}
	}
	if hasFinal {
		return final.String()
	}
	return unphased.String()
}
