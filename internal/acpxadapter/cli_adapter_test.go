package acpxadapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

type recordedCall struct {
	binary string
	args   []string
	dir    string
}

type runnerResult struct {
	output string
	err    error
}

type startRunnerResult struct {
	stdout   string
	stderr   string
	waitErr  error
	closeErr error
}

type fakeRunner struct {
	calls        []recordedCall
	results      []runnerResult
	startResults []startRunnerResult
}

func (f *fakeRunner) Run(_ context.Context, binary string, args []string, dir string) (string, error) {
	f.calls = append(f.calls, recordedCall{
		binary: binary,
		args:   append([]string(nil), args...),
		dir:    dir,
	})
	if len(f.results) == 0 {
		return "", nil
	}
	result := f.results[0]
	f.results = f.results[1:]
	return result.output, result.err
}

func (f *fakeRunner) Start(_ context.Context, binary string, args []string, dir string) (RunningCommand, error) {
	f.calls = append(f.calls, recordedCall{
		binary: binary,
		args:   append([]string(nil), args...),
		dir:    dir,
	})
	if len(f.startResults) == 0 {
		return &fakeRunningCommand{}, nil
	}
	result := f.startResults[0]
	f.startResults = f.startResults[1:]
	return &fakeRunningCommand{
		stdout:   io.NopCloser(strings.NewReader(result.stdout)),
		stderr:   io.NopCloser(strings.NewReader(result.stderr)),
		waitErr:  result.waitErr,
		closeErr: result.closeErr,
	}, nil
}

type fakeRunningCommand struct {
	stdout   io.ReadCloser
	stderr   io.ReadCloser
	waitErr  error
	closeErr error
}

func (c *fakeRunningCommand) Stdout() io.ReadCloser {
	if c.stdout == nil {
		return io.NopCloser(strings.NewReader(""))
	}
	return c.stdout
}

func (c *fakeRunningCommand) Stderr() io.ReadCloser {
	if c.stderr == nil {
		return io.NopCloser(strings.NewReader(""))
	}
	return c.stderr
}

func (c *fakeRunningCommand) Wait() error {
	return c.waitErr
}

func (c *fakeRunningCommand) Close() error {
	return c.closeErr
}

type blockingRunningCommand struct {
	stdout   io.ReadCloser
	stderr   io.ReadCloser
	waitCh   chan error
	closeErr error
}

func (c *blockingRunningCommand) Stdout() io.ReadCloser {
	return c.stdout
}

func (c *blockingRunningCommand) Stderr() io.ReadCloser {
	return c.stderr
}

func (c *blockingRunningCommand) Wait() error {
	return <-c.waitCh
}

func (c *blockingRunningCommand) Close() error {
	return c.closeErr
}

// Test: session names are derived deterministically from Slack thread timestamps.
// Validates: AC-1795 (REQ-1156 - cancel requests cancellation without corrupting session mapping), EP-151 system design session mapping policy (`thread_ts -> session_name = slack-<thread_ts>`)
func TestSessionNameTrimsAndPrefixesSlackThreadTimestamp(t *testing.T) {
	t.Parallel()

	got := SessionName(" 1713686400.000100 ")
	if got != "slack-1713686400.000100" {
		t.Fatalf("SessionName() = %q, want slack-1713686400.000100", got)
	}
}

// Test: root messages and thread replies dispatch through the same derived ACPX session name.
// Validates: AC-1787 (REQ-1148 - root messages create or ensure a thread session), AC-1788 (REQ-1149 - replies continue the existing thread session)
func TestCLIAdapterEnsuresAndReusesSessionForPromptDispatch(t *testing.T) {
	t.Parallel()

	rootOutput := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"root answer"}}}}
{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`
	replyOutput := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"thread reply"}}}}
{"jsonrpc":"2.0","id":2,"result":{"stopReason":"end_turn"}}`

	runner := &fakeRunner{
		results: []runnerResult{
			{output: "ensured"},
			{output: "ensured"},
		},
		startResults: []startRunnerResult{
			{stdout: rootOutput},
			{stdout: replyOutput},
		},
	}
	adapter := NewCLIAdapter("acpx", runner)

	rootResult, err := adapter.SendPrompt(context.Background(), SessionRequest{
		ProjectPath: "/workspace/alpha",
		ThreadTS:    "1713686400.000100",
		Prompt:      "root message",
	})
	if err != nil {
		t.Fatalf("SendPrompt(root) error = %v", err)
	}
	if rootResult.SessionName != "slack-1713686400.000100" {
		t.Fatalf("SendPrompt(root) session = %q, want slack-1713686400.000100", rootResult.SessionName)
	}
	if rootResult.Output != rootOutput {
		t.Fatalf("SendPrompt(root) output = %q, want raw streamed output", rootResult.Output)
	}

	replyResult, err := adapter.SendPrompt(context.Background(), SessionRequest{
		ProjectPath: "/workspace/alpha",
		ThreadTS:    "1713686400.000100",
		Prompt:      "thread reply",
	})
	if err != nil {
		t.Fatalf("SendPrompt(reply) error = %v", err)
	}
	if replyResult.SessionName != rootResult.SessionName {
		t.Fatalf("SendPrompt(reply) session = %q, want %q", replyResult.SessionName, rootResult.SessionName)
	}
	if replyResult.Output != replyOutput {
		t.Fatalf("SendPrompt(reply) output = %q, want raw streamed output", replyResult.Output)
	}

	if got, want := len(runner.calls), 4; got != want {
		t.Fatalf("runner calls = %d, want %d", got, want)
	}

	wantEnsure := []string{"--format", "json", "--json-strict", "--approve-all", "codex", "sessions", "ensure", "--name", "slack-1713686400.000100"}
	wantRootPrompt := []string{"--format", "json", "--json-strict", "--approve-all", "codex", "prompt", "-s", "slack-1713686400.000100", "root message"}
	wantReplyPrompt := []string{"--format", "json", "--json-strict", "--approve-all", "codex", "prompt", "-s", "slack-1713686400.000100", "thread reply"}
	if fmt.Sprint(runner.calls[0].args) != fmt.Sprint(wantEnsure) {
		t.Fatalf("first call args = %#v, want %#v", runner.calls[0].args, wantEnsure)
	}
	if runner.calls[0].dir != "/workspace/alpha" {
		t.Fatalf("first call dir = %q, want /workspace/alpha", runner.calls[0].dir)
	}
	if fmt.Sprint(runner.calls[1].args) != fmt.Sprint(wantRootPrompt) {
		t.Fatalf("second call args = %#v, want %#v", runner.calls[1].args, wantRootPrompt)
	}
	if fmt.Sprint(runner.calls[2].args) != fmt.Sprint(wantEnsure) {
		t.Fatalf("third call args = %#v, want %#v", runner.calls[2].args, wantEnsure)
	}
	if fmt.Sprint(runner.calls[3].args) != fmt.Sprint(wantReplyPrompt) {
		t.Fatalf("fourth call args = %#v, want %#v", runner.calls[3].args, wantReplyPrompt)
	}
}

// Test: the prompt stream exposes incremental ACPX events while preserving the derived Slack session name.
// Validates: AC-1978 (REQ-1424 - ACPX prompt executes as a stream and emits prompt events before terminal completion)
func TestCLIAdapterStartPromptStreamsEvents(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{
		results: []runnerResult{
			{output: "ensured"},
		},
		startResults: []startRunnerResult{{
			stdout: `{"jsonrpc":"2.0","id":2,"method":"session/new","result":{"sessionId":"019db13d-f733-7ce0-8186-5aced7cdb2a7"}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"019db13d-f733-7ce0-8186-5aced7cdb2a7","update":{"sessionUpdate":"tool_call","toolCallId":"call_1","title":"Run pwd","kind":"execute","status":"in_progress"}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"019db13d-f733-7ce0-8186-5aced7cdb2a7","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_1","status":"completed"}}}
{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}`,
		}},
	}
	adapter := NewCLIAdapter("acpx", runner)

	stream, err := adapter.StartPrompt(context.Background(), SessionRequest{
		ProjectPath: "/workspace/alpha",
		ThreadTS:    "1713686400.000100",
		Prompt:      "root message",
	})
	if err != nil {
		t.Fatalf("StartPrompt() error = %v", err)
	}
	defer func() {
		_ = stream.Close()
	}()

	events, err := CollectPromptStream(stream)
	if err != nil {
		t.Fatalf("CollectPromptStream() error = %v", err)
	}
	if stream.SessionName() != "slack-1713686400.000100" {
		t.Fatalf("SessionName() = %q, want slack-1713686400.000100", stream.SessionName())
	}
	if got, want := len(events), 4; got != want {
		t.Fatalf("streamed events = %d, want %d", got, want)
	}
	if events[0].Kind != EventSessionStarted || events[1].Kind != EventToolStarted || events[2].Kind != EventToolFinished || events[3].Kind != EventSessionDone {
		t.Fatalf("streamed event kinds = %#v", events)
	}
}

// Test: prompt streams parse newline-delimited ACPX output incrementally and keep Wait blocked until the process exits.
// Validates: AC-1978 (REQ-1424 - ACPX prompt executes as a stream), AC-1980 (REQ-1427 - terminal completion waits for the ACPX process to finish before ending the stream)
func TestCLIPromptStreamParsesIncrementallyBeforeWaitCompletes(t *testing.T) {
	t.Parallel()

	stdoutReader, stdoutWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()
	command := &blockingRunningCommand{
		stdout: stdoutReader,
		stderr: stderrReader,
		waitCh: make(chan error, 1),
	}
	stream := newCLIPromptStream(
		"slack-1713686400.000100",
		"acpx",
		[]string{"--format", "json", "codex", "prompt", "-s", "slack-1713686400.000100", "status"},
		command,
	)
	defer func() {
		_ = stdoutWriter.Close()
		_ = stderrWriter.Close()
		_ = stream.Close()
	}()

	if _, err := io.WriteString(stdoutWriter, `{"jsonrpc":"2.0","method":"session/new","result":{"sessionId":"s1"}}`); err != nil {
		t.Fatalf("WriteString(partial session/new) error = %v", err)
	}

	select {
	case event := <-stream.Events():
		t.Fatalf("Events() delivered %#v before the first newline completed the record", event)
	case <-time.After(100 * time.Millisecond):
	}

	if _, err := io.WriteString(stdoutWriter, "\n"); err != nil {
		t.Fatalf("WriteString(session/new newline) error = %v", err)
	}

	var first Event
	select {
	case first = <-stream.Events():
	case <-time.After(time.Second):
		t.Fatal("Events() did not deliver the first streamed event in time")
	}
	if first.Kind != EventSessionStarted || first.Text != "s1" {
		t.Fatalf("first streamed event = %#v, want session started", first)
	}

	if _, err := io.WriteString(stdoutWriter, `{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`+"\n"); err != nil {
		t.Fatalf("WriteString(stop reason) error = %v", err)
	}
	if err := stdoutWriter.Close(); err != nil {
		t.Fatalf("stdoutWriter.Close() error = %v", err)
	}
	if err := stderrWriter.Close(); err != nil {
		t.Fatalf("stderrWriter.Close() error = %v", err)
	}

	waitResult := make(chan error, 1)
	go func() {
		waitResult <- stream.Wait()
	}()

	select {
	case err := <-waitResult:
		t.Fatalf("Wait() returned early with %v before the process exit was reported", err)
	case <-time.After(100 * time.Millisecond):
	}

	command.waitCh <- nil

	if err := <-waitResult; err != nil {
		t.Fatalf("Wait() error = %v", err)
	}

	var remaining []Event
	for event := range stream.Events() {
		remaining = append(remaining, event)
	}

	if got, want := len(remaining), 1; got != want {
		t.Fatalf("remaining streamed events = %d, want %d", got, want)
	}
	if remaining[0].Kind != EventSessionDone {
		t.Fatalf("terminal streamed event = %#v, want session_done", remaining[0])
	}
}

// Test: thread-scoped control operations resolve the same derived session name inside the adapter.
// Validates: AC-1795 (REQ-1156 - cancel requests cancellation without corrupting the thread/session mapping)
func TestCLIAdapterStatusAndCancelResolveThreadSession(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{
		results: []runnerResult{{output: "ok"}},
	}
	adapter := NewCLIAdapter("acpx", runner)

	statusResult, err := adapter.Status(context.Background(), "1713686400.000100")
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if statusResult.SessionName != "slack-1713686400.000100" {
		t.Fatalf("Status() session = %q, want slack-1713686400.000100", statusResult.SessionName)
	}
	if statusResult.Output != "ok" {
		t.Fatalf("Status() output = %q, want ok", statusResult.Output)
	}

	if err := adapter.Cancel(context.Background(), "1713686400.000100"); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}

	if got, want := len(runner.calls), 2; got != want {
		t.Fatalf("runner calls = %d, want %d", got, want)
	}

	if fmt.Sprint(runner.calls[0].args) != fmt.Sprint([]string{"--format", "json", "--json-strict", "--approve-all", "codex", "status", "-s", "slack-1713686400.000100"}) {
		t.Fatalf("status call args = %#v, want status session args", runner.calls[0].args)
	}
	if fmt.Sprint(runner.calls[1].args) != fmt.Sprint([]string{"--format", "json", "--json-strict", "--approve-all", "codex", "cancel", "-s", "slack-1713686400.000100"}) {
		t.Fatalf("cancel call args = %#v, want cancel session args", runner.calls[1].args)
	}
}

// Test: invalid thread context is rejected before the adapter can issue a CLI command.
// Validates: AC-1787/AC-1788/AC-1795 session mapping inputs require a thread timestamp
func TestCLIAdapterRejectsEmptyThreadTimestamp(t *testing.T) {
	t.Parallel()

	adapter := NewCLIAdapter("acpx", &fakeRunner{})
	_, err := adapter.EnsureSession(context.Background(), SessionRequest{
		ProjectPath: "/workspace/alpha",
		ThreadTS:    " ",
	})
	if err == nil {
		t.Fatalf("EnsureSession() error = nil, want non-nil")
	}
}

func TestCLIAdapterRejectsEmptyPrompt(t *testing.T) {
	t.Parallel()

	adapter := NewCLIAdapter("acpx", &fakeRunner{})
	_, err := adapter.SendPrompt(context.Background(), SessionRequest{
		ProjectPath: "/workspace/alpha",
		ThreadTS:    "1713686400.000100",
		Prompt:      " ",
	})
	if err == nil {
		t.Fatalf("SendPrompt() error = nil, want non-nil")
	}
}

func TestCLIAdapterPropagatesRunnerErrors(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{results: []runnerResult{{err: errors.New("boom")}}}
	adapter := NewCLIAdapter("acpx", runner)

	_, err := adapter.Status(context.Background(), "1713686400.000100")
	if err == nil {
		t.Fatalf("Status() error = nil, want non-nil")
	}
}

func TestNewCLIAdapterUsesACPXBinaryEnvOverride(t *testing.T) {
	t.Setenv("SPEXUS_AGENT_ACPX_BIN", "/opt/acpx/bin/acpx")

	adapter := NewCLIAdapter("", &fakeRunner{})
	if adapter.binary != "/opt/acpx/bin/acpx" {
		t.Fatalf("adapter binary = %q, want /opt/acpx/bin/acpx", adapter.binary)
	}
}

func TestCLIAdapterPropagatesStreamingWaitErrors(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{
		results: []runnerResult{
			{output: "ensured"},
		},
		startResults: []startRunnerResult{{
			stdout:  `{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`,
			stderr:  "queue owner unavailable",
			waitErr: errors.New("exit status 1"),
		}},
	}
	adapter := NewCLIAdapter("acpx", runner)

	stream, err := adapter.StartPrompt(context.Background(), SessionRequest{
		ProjectPath: "/workspace/alpha",
		ThreadTS:    "1713686400.000100",
		Prompt:      "recover session",
	})
	if err != nil {
		t.Fatalf("StartPrompt() error = %v", err)
	}
	defer func() {
		_ = stream.Close()
	}()

	_, err = CollectPromptStream(stream)
	if err == nil {
		t.Fatalf("CollectPromptStream() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "queue owner unavailable") {
		t.Fatalf("CollectPromptStream() error = %v, want stderr details", err)
	}
}
