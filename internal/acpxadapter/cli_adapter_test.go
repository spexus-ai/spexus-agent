package acpxadapter

import (
	"context"
	"errors"
	"fmt"
	"testing"
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

type fakeRunner struct {
	calls   []recordedCall
	results []runnerResult
}

func (f *fakeRunner) Run(_ context.Context, binary string, args []string, dir string) (string, error) {
	call := recordedCall{
		binary: binary,
		args:   append([]string(nil), args...),
		dir:    dir,
	}
	f.calls = append(f.calls, call)
	if len(f.results) == 0 {
		return "", nil
	}
	result := f.results[0]
	f.results = f.results[1:]
	return result.output, result.err
}

func (f *fakeRunner) RunStream(ctx context.Context, binary string, args []string, dir string, onOutput PromptStreamFunc) (string, error) {
	output, err := f.Run(ctx, binary, args, dir)
	if onOutput != nil {
		if callbackErr := onOutput(output); callbackErr != nil {
			return output, callbackErr
		}
	}
	return output, err
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

	runner := &fakeRunner{
		results: []runnerResult{
			{err: errors.New("no session")},
			{output: "created"},
			{output: "prompted"},
			{output: "prompted-again"},
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
	if rootResult.Output != "prompted" {
		t.Fatalf("SendPrompt(root) output = %q, want prompted", rootResult.Output)
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
	if replyResult.Output != "prompted-again" {
		t.Fatalf("SendPrompt(reply) output = %q, want prompted-again", replyResult.Output)
	}

	if got, want := len(runner.calls), 4; got != want {
		t.Fatalf("runner calls = %d, want %d", got, want)
	}

	wantPrompt := []string{"--format", "json", "--json-strict", "--approve-all", "codex", "prompt", "-s", "slack-1713686400.000100", "root message"}
	wantCreate := []string{"--format", "json", "--json-strict", "--approve-all", "codex", "sessions", "new", "--name", "slack-1713686400.000100"}
	if fmt.Sprint(runner.calls[0].args) != fmt.Sprint(wantPrompt) {
		t.Fatalf("first call args = %#v, want %#v", runner.calls[0].args, wantPrompt)
	}
	if runner.calls[0].dir != "/workspace/alpha" {
		t.Fatalf("first call dir = %q, want /workspace/alpha", runner.calls[0].dir)
	}
	if fmt.Sprint(runner.calls[1].args) != fmt.Sprint(wantCreate) {
		t.Fatalf("second call args = %#v, want %#v", runner.calls[1].args, wantCreate)
	}
	if runner.calls[1].dir != "/workspace/alpha" {
		t.Fatalf("second call dir = %q, want /workspace/alpha", runner.calls[1].dir)
	}
	if fmt.Sprint(runner.calls[2].args) != fmt.Sprint(wantPrompt) {
		t.Fatalf("third call args = %#v, want %#v", runner.calls[2].args, wantPrompt)
	}
	if fmt.Sprint(runner.calls[3].args) != fmt.Sprint([]string{"--format", "json", "--json-strict", "--approve-all", "codex", "prompt", "-s", "slack-1713686400.000100", "thread reply"}) {
		t.Fatalf("fourth call args = %#v, want thread reply prompt", runner.calls[3].args)
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

func TestCLIAdapterRetriesPromptAfterQueueOwnerStartupFailure(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{
		results: []runnerResult{
			{err: errors.New("Session queue owner failed to start")},
			{output: "created"},
			{output: "prompted"},
		},
	}
	adapter := NewCLIAdapter("acpx", runner)

	result, err := adapter.SendPrompt(context.Background(), SessionRequest{
		ProjectPath: "/workspace/alpha",
		ThreadTS:    "1713686400.000100",
		Prompt:      "recover session",
	})
	if err != nil {
		t.Fatalf("SendPrompt() error = %v", err)
	}
	if result.Output != "prompted" {
		t.Fatalf("SendPrompt() output = %q, want prompted", result.Output)
	}
	if got, want := len(runner.calls), 3; got != want {
		t.Fatalf("runner calls = %d, want %d", got, want)
	}
	if fmt.Sprint(runner.calls[1].args) != fmt.Sprint([]string{"--format", "json", "--json-strict", "--approve-all", "codex", "sessions", "new", "--name", "slack-1713686400.000100"}) {
		t.Fatalf("recreate call args = %#v, want sessions new", runner.calls[1].args)
	}
}

func TestCLIAdapterStreamEnsuresSessionBeforePrompt(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{
		results: []runnerResult{
			{output: "ensured"},
			{output: `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello"}}}}`},
		},
	}
	adapter := NewCLIAdapter("acpx", runner)

	var streamed []string
	result, err := adapter.SendPromptStream(context.Background(), SessionRequest{
		ProjectPath: "/workspace/alpha",
		ThreadTS:    "1713686400.000100",
		Prompt:      "stream prompt",
	}, func(output string) error {
		streamed = append(streamed, output)
		return nil
	})
	if err != nil {
		t.Fatalf("SendPromptStream() error = %v", err)
	}
	if result.SessionName != "slack-1713686400.000100" {
		t.Fatalf("session name = %q, want slack session", result.SessionName)
	}
	if got, want := len(runner.calls), 2; got != want {
		t.Fatalf("runner calls = %d, want %d", got, want)
	}
	if fmt.Sprint(runner.calls[0].args) != fmt.Sprint([]string{"--format", "json", "--json-strict", "--approve-all", "codex", "sessions", "ensure", "--name", "slack-1713686400.000100"}) {
		t.Fatalf("ensure call args = %#v", runner.calls[0].args)
	}
	if fmt.Sprint(runner.calls[1].args) != fmt.Sprint([]string{"--format", "json", "--json-strict", "--approve-all", "codex", "prompt", "-s", "slack-1713686400.000100", "stream prompt"}) {
		t.Fatalf("prompt call args = %#v", runner.calls[1].args)
	}
	if got, want := len(streamed), 1; got != want {
		t.Fatalf("stream callback calls = %d, want %d", got, want)
	}
}

func TestCLIAdapterStreamSuppressesRecoverableNoSessionBeforeRetry(t *testing.T) {
	t.Parallel()

	noSessionOutput := `{"jsonrpc":"2.0","id":null,"error":{"code":-32002,"message":":warning: No acpx session found","data":{"acpxCode":"NO_SESSION"}}}`
	runner := &fakeRunner{
		results: []runnerResult{
			{output: "ensured"},
			{output: noSessionOutput, err: errors.New("exit status 4: " + noSessionOutput)},
			{output: "created"},
			{output: `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"recovered"}}}}`},
		},
	}
	adapter := NewCLIAdapter("acpx", runner)

	var streamed []string
	result, err := adapter.SendPromptStream(context.Background(), SessionRequest{
		ProjectPath: "/workspace/alpha",
		ThreadTS:    "1713686400.000100",
		Prompt:      "recover stream",
	}, func(output string) error {
		streamed = append(streamed, output)
		return nil
	})
	if err != nil {
		t.Fatalf("SendPromptStream() error = %v", err)
	}
	if result.Output == noSessionOutput {
		t.Fatalf("SendPromptStream() returned unrecovered no-session output")
	}
	if got, want := len(streamed), 1; got != want {
		t.Fatalf("stream callback calls = %d, want only recovered output", got)
	}
	if streamed[0] == noSessionOutput {
		t.Fatalf("recoverable no-session output was streamed")
	}
	if got, want := len(runner.calls), 4; got != want {
		t.Fatalf("runner calls = %d, want ensure, prompt, recreate, prompt", got)
	}
	if fmt.Sprint(runner.calls[2].args) != fmt.Sprint([]string{"--format", "json", "--json-strict", "--approve-all", "codex", "sessions", "new", "--name", "slack-1713686400.000100"}) {
		t.Fatalf("recreate call args = %#v", runner.calls[2].args)
	}
}

func TestCLIAdapterForceNewCreatesSessionBeforePrompt(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{
		results: []runnerResult{
			{output: "created"},
			{output: "prompted"},
		},
	}
	adapter := NewCLIAdapter("acpx", runner)

	result, err := adapter.SendPrompt(context.Background(), SessionRequest{
		ProjectPath: "/workspace/alpha",
		ThreadTS:    "1713686400.000100",
		Prompt:      "fresh ask",
		ForceNew:    true,
	})
	if err != nil {
		t.Fatalf("SendPrompt() error = %v", err)
	}
	if result.Output != "prompted" {
		t.Fatalf("SendPrompt() output = %q, want prompted", result.Output)
	}
	if got, want := len(runner.calls), 2; got != want {
		t.Fatalf("runner calls = %d, want %d", got, want)
	}
	if fmt.Sprint(runner.calls[0].args) != fmt.Sprint([]string{"--format", "json", "--json-strict", "--approve-all", "codex", "sessions", "new", "--name", "slack-1713686400.000100"}) {
		t.Fatalf("first call args = %#v, want sessions new", runner.calls[0].args)
	}
	if fmt.Sprint(runner.calls[1].args) != fmt.Sprint([]string{"--format", "json", "--json-strict", "--approve-all", "codex", "prompt", "-s", "slack-1713686400.000100", "fresh ask"}) {
		t.Fatalf("second call args = %#v, want prompt", runner.calls[1].args)
	}
}
