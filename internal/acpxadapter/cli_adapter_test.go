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

type fakeRunner struct {
	calls   []recordedCall
	outputs []string
	err     error
}

func (f *fakeRunner) Run(_ context.Context, binary string, args []string, dir string) (string, error) {
	call := recordedCall{
		binary: binary,
		args:   append([]string(nil), args...),
		dir:    dir,
	}
	f.calls = append(f.calls, call)
	if f.err != nil {
		return "", f.err
	}
	if len(f.outputs) == 0 {
		return "", nil
	}
	output := f.outputs[0]
	f.outputs = f.outputs[1:]
	return output, nil
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
		outputs: []string{"ensured", "prompted", "ensured-again", "prompted-again"},
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

	wantEnsure := []string{"--format", "json", "--json-strict", "codex", "sessions", "ensure", "--name", "slack-1713686400.000100"}
	wantPrompt := []string{"--format", "json", "--json-strict", "codex", "prompt", "-s", "slack-1713686400.000100", "root message"}
	if fmt.Sprint(runner.calls[0].args) != fmt.Sprint(wantEnsure) {
		t.Fatalf("first call args = %#v, want %#v", runner.calls[0].args, wantEnsure)
	}
	if runner.calls[0].dir != "/workspace/alpha" {
		t.Fatalf("first call dir = %q, want /workspace/alpha", runner.calls[0].dir)
	}
	if fmt.Sprint(runner.calls[1].args) != fmt.Sprint(wantPrompt) {
		t.Fatalf("second call args = %#v, want %#v", runner.calls[1].args, wantPrompt)
	}
	if runner.calls[1].dir != "/workspace/alpha" {
		t.Fatalf("second call dir = %q, want /workspace/alpha", runner.calls[1].dir)
	}
	if fmt.Sprint(runner.calls[2].args) != fmt.Sprint(wantEnsure) {
		t.Fatalf("third call args = %#v, want %#v", runner.calls[2].args, wantEnsure)
	}
	if fmt.Sprint(runner.calls[3].args) != fmt.Sprint([]string{"--format", "json", "--json-strict", "codex", "prompt", "-s", "slack-1713686400.000100", "thread reply"}) {
		t.Fatalf("fourth call args = %#v, want thread reply prompt", runner.calls[3].args)
	}
}

// Test: thread-scoped control operations resolve the same derived session name inside the adapter.
// Validates: AC-1795 (REQ-1156 - cancel requests cancellation without corrupting the thread/session mapping)
func TestCLIAdapterStatusAndCancelResolveThreadSession(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{
		outputs: []string{"ok"},
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

	if fmt.Sprint(runner.calls[0].args) != fmt.Sprint([]string{"--format", "json", "--json-strict", "codex", "status", "-s", "slack-1713686400.000100"}) {
		t.Fatalf("status call args = %#v, want status session args", runner.calls[0].args)
	}
	if fmt.Sprint(runner.calls[1].args) != fmt.Sprint([]string{"--format", "json", "--json-strict", "codex", "cancel", "-s", "slack-1713686400.000100"}) {
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

	runner := &fakeRunner{err: errors.New("boom")}
	adapter := NewCLIAdapter("acpx", runner)

	_, err := adapter.Status(context.Background(), "1713686400.000100")
	if err == nil {
		t.Fatalf("Status() error = nil, want non-nil")
	}
}
