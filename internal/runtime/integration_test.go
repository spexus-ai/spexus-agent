package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/acpxadapter"
	"github.com/spexus-ai/spexus-agent/internal/registry"
	runtime "github.com/spexus-ai/spexus-agent/internal/runtime"
	"github.com/spexus-ai/spexus-agent/internal/slack"
	"github.com/spexus-ai/spexus-agent/internal/storage"
)

type recordingCommandRunner struct {
	mu           sync.Mutex
	calls        []recordedCommandCall
	outputs      []string
	startOutputs []string
}

type recordedCommandCall struct {
	binary string
	args   []string
	dir    string
}

func (r *recordingCommandRunner) Run(_ context.Context, binary string, args []string, dir string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.calls = append(r.calls, recordedCommandCall{
		binary: binary,
		args:   append([]string(nil), args...),
		dir:    dir,
	})

	if len(r.outputs) == 0 {
		return "", nil
	}

	output := r.outputs[0]
	r.outputs = r.outputs[1:]
	return output, nil
}

func (r *recordingCommandRunner) Start(_ context.Context, binary string, args []string, dir string) (acpxadapter.RunningCommand, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.calls = append(r.calls, recordedCommandCall{
		binary: binary,
		args:   append([]string(nil), args...),
		dir:    dir,
	})

	output := ""
	if len(r.startOutputs) > 0 {
		output = r.startOutputs[0]
		r.startOutputs = r.startOutputs[1:]
	}

	return acpxadapterPromptCommand{
		stdout: io.NopCloser(strings.NewReader(output)),
		stderr: io.NopCloser(strings.NewReader("")),
	}, nil
}

type acpxadapterPromptCommand struct {
	stdout io.ReadCloser
	stderr io.ReadCloser
}

func (c acpxadapterPromptCommand) Stdout() io.ReadCloser { return c.stdout }
func (c acpxadapterPromptCommand) Stderr() io.ReadCloser { return c.stderr }
func (c acpxadapterPromptCommand) Wait() error           { return nil }
func (c acpxadapterPromptCommand) Close() error          { return nil }

type recordingSlackClient struct {
	mu       sync.Mutex
	messages []slack.Message
	updates  []slack.MessageUpdate
}

func (c *recordingSlackClient) PostMessage(_ context.Context, message slack.Message) (slack.PostedMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.messages = append(c.messages, message)
	return slack.PostedMessage{
		ChannelID: message.ChannelID,
		Timestamp: "1713686400.000100",
	}, nil
}

func (c *recordingSlackClient) PostThreadMessage(_ context.Context, message slack.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.messages = append(c.messages, message)
	return nil
}

func (c *recordingSlackClient) UpdateMessage(_ context.Context, update slack.MessageUpdate) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.updates = append(c.updates, update)
	return nil
}

func (c *recordingSlackClient) CreateChannel(context.Context, slack.CreateChannelRequest) (slack.Channel, error) {
	return slack.Channel{}, fmt.Errorf("not implemented")
}

func (c *recordingSlackClient) FindChannelByName(context.Context, string) (slack.Channel, error) {
	return slack.Channel{}, fmt.Errorf("not implemented")
}

func (c *recordingSlackClient) Close() error { return nil }

type sqliteRuntimeStore struct {
	store *storage.Store
}

func (s sqliteRuntimeStore) SaveThreadState(ctx context.Context, state runtime.ThreadState) error {
	return s.store.Runtime().SaveThreadState(ctx, state)
}

func (s sqliteRuntimeStore) LoadThreadState(ctx context.Context, threadTS string) (runtime.ThreadState, error) {
	return s.store.Runtime().LoadThreadState(ctx, threadTS)
}

func (s sqliteRuntimeStore) SaveEventDedupe(ctx context.Context, dedupe runtime.EventDedupe) error {
	return s.store.Runtime().SaveEventDedupe(ctx, dedupe)
}

func (s sqliteRuntimeStore) LoadEventDedupe(ctx context.Context, sourceType, deliveryID string) (runtime.EventDedupe, error) {
	return s.store.Runtime().LoadEventDedupe(ctx, sourceType, deliveryID)
}

func (s sqliteRuntimeStore) SaveThreadLock(ctx context.Context, lock runtime.ThreadLock) error {
	return s.store.Runtime().SaveThreadLock(ctx, lock)
}

func (s sqliteRuntimeStore) LoadThreadLock(ctx context.Context, threadTS string) (runtime.ThreadLock, error) {
	return s.store.Runtime().LoadThreadLock(ctx, threadTS)
}

func (s sqliteRuntimeStore) DeleteThreadLock(ctx context.Context, threadTS string) error {
	return s.store.Runtime().DeleteThreadLock(ctx, threadTS)
}

// Test: a registered Slack event is resolved from SQLite, dispatched through ACPX, and rendered back to the thread using translated ACPX output.
// Validates: AC-1782 (REQ-1143 - runtime loads project registry from SQLite), AC-1787 (REQ-1148 - root Slack messages create or ensure a thread session), AC-1788 (REQ-1149 - thread replies continue the existing thread session), AC-1791 (REQ-1152 - runtime persists deduplication data and thread metadata in SQLite), AC-1792 (REQ-1153 - duplicate Slack events are deduplicated)
func TestSlackEventDispatchPersistsSQLiteStateAndRendersACPXOutput(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	workspacePath := filepath.Join(home, "workspace")
	if err := os.MkdirAll(filepath.Join(workspacePath, "alpha"), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	project := registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(workspacePath, "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C12345678",
	}
	if err := store.Projects().Upsert(ctx, project); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	rootPrepared, err := runtime.PrepareSlackEvent(ctx, store.Projects(), slack.Event{
		ID:        "Ev-root",
		ChannelID: project.SlackChannelID,
		Timestamp: "1713686400.000100",
		UserID:    "U123",
		Text:      "root prompt",
	})
	if err != nil {
		t.Fatalf("PrepareSlackEvent(root) error = %v", err)
	}

	replyPrepared, err := runtime.PrepareSlackEvent(ctx, store.Projects(), slack.Event{
		ID:        "Ev-reply",
		ChannelID: project.SlackChannelID,
		ThreadTS:  rootPrepared.ThreadTS,
		Timestamp: "1713686410.000200",
		UserID:    "U123",
		Text:      "thread reply",
	})
	if err != nil {
		t.Fatalf("PrepareSlackEvent(reply) error = %v", err)
	}

	if replyPrepared.SessionName != rootPrepared.SessionName {
		t.Fatalf("reply session name = %q, want %q", replyPrepared.SessionName, rootPrepared.SessionName)
	}
	if !replyPrepared.IsThreadReply {
		t.Fatalf("PrepareSlackEvent(reply) IsThreadReply = false, want true")
	}

	runner := &recordingCommandRunner{
		outputs: []string{
			"ensured",
			"ensured",
		},
		startOutputs: []string{
			`{"jsonrpc":"2.0","id":2,"method":"session/new","result":{"sessionId":"019db13d-f733-7ce0-8186-5aced7cdb2a7"}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"019db13d-f733-7ce0-8186-5aced7cdb2a7","update":{"sessionUpdate":"tool_call","toolCallId":"call_1","title":"Run grep","kind":"execute","status":"in_progress"}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"019db13d-f733-7ce0-8186-5aced7cdb2a7","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_1","status":"completed"}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"019db13d-f733-7ce0-8186-5aced7cdb2a7","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"root"}}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"019db13d-f733-7ce0-8186-5aced7cdb2a7","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":" final answer"}}}}
{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}`,
			`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"019db13d-f733-7ce0-8186-5aced7cdb2a7","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"reply final answer"}}}}
{"jsonrpc":"2.0","id":4,"result":{"stopReason":"end_turn"}}`,
		},
	}
	adapter := acpxadapter.NewCLIAdapter("acpx", runner)
	client := &recordingSlackClient{}
	renderer := runtime.SlackThreadRenderer{Client: client}
	coordinator := runtime.NewSlackTurnCoordinator(sqliteRuntimeStore{store: store}, "runtime-1")

	var dispatchedSessions []string
	execute := func(ctx context.Context, prepared runtime.PreparedSlackEvent) error {
		session, err := adapter.SendPrompt(ctx, acpxadapter.SessionRequest{
			ProjectPath: prepared.Project.LocalPath,
			ThreadTS:    prepared.ThreadTS,
			Prompt:      prepared.Event.Text,
		})
		if err != nil {
			return err
		}

		dispatchedSessions = append(dispatchedSessions, session.SessionName)
		return runtime.RenderACPXTurnOutput(ctx, renderer, runtime.SlackThreadRenderRequest{
			ChannelID:   prepared.Project.SlackChannelID,
			ThreadTS:    prepared.ThreadTS,
			SessionName: session.SessionName,
		}, session.Output)
	}

	rootResult, err := coordinator.Execute(ctx, rootPrepared, execute)
	if err != nil {
		t.Fatalf("Execute(root) error = %v", err)
	}
	if !rootResult.Executed || rootResult.Duplicate {
		t.Fatalf("Execute(root) result = %#v, want executed and not duplicate", rootResult)
	}
	if rootResult.CompletedAt.IsZero() {
		t.Fatalf("Execute(root) completed at is zero")
	}

	replyResult, err := coordinator.Execute(ctx, replyPrepared, execute)
	if err != nil {
		t.Fatalf("Execute(reply) error = %v", err)
	}
	if !replyResult.Executed || replyResult.Duplicate {
		t.Fatalf("Execute(reply) result = %#v, want executed and not duplicate", replyResult)
	}
	if replyResult.CompletedAt.IsZero() {
		t.Fatalf("Execute(reply) completed at is zero")
	}

	duplicateResult, err := coordinator.Execute(ctx, rootPrepared, func(context.Context, runtime.PreparedSlackEvent) error {
		t.Fatalf("duplicate root event should not reach the execution callback")
		return nil
	})
	if err != nil {
		t.Fatalf("Execute(duplicate root) error = %v", err)
	}
	if !duplicateResult.Duplicate || duplicateResult.Executed {
		t.Fatalf("Execute(duplicate root) result = %#v, want duplicate and not executed", duplicateResult)
	}

	if got, want := len(dispatchedSessions), 2; got != want {
		t.Fatalf("dispatched session count = %d, want %d", got, want)
	}
	if dispatchedSessions[0] != rootPrepared.SessionName || dispatchedSessions[1] != replyPrepared.SessionName {
		t.Fatalf("dispatched sessions = %#v, want reused thread session %q", dispatchedSessions, rootPrepared.SessionName)
	}

	runner.mu.Lock()
	gotCalls := append([]recordedCommandCall(nil), runner.calls...)
	runner.mu.Unlock()
	if got, want := len(gotCalls), 4; got != want {
		t.Fatalf("ACPX runner call count = %d, want %d", got, want)
	}
	if gotCalls[0].dir != project.LocalPath {
		t.Fatalf("first ACPX call dir = %q, want %q", gotCalls[0].dir, project.LocalPath)
	}
	if fmt.Sprint(gotCalls[0].args) != fmt.Sprint([]string{"--format", "json", "--json-strict", "--approve-all", "codex", "sessions", "ensure", "--name", rootPrepared.SessionName}) {
		t.Fatalf("first ACPX call args = %#v, want ensure session", gotCalls[0].args)
	}
	if fmt.Sprint(gotCalls[1].args) != fmt.Sprint([]string{"--format", "json", "--json-strict", "--approve-all", "codex", "prompt", "-s", rootPrepared.SessionName, "root prompt"}) {
		t.Fatalf("second ACPX call args = %#v, want root prompt", gotCalls[1].args)
	}
	if fmt.Sprint(gotCalls[2].args) != fmt.Sprint([]string{"--format", "json", "--json-strict", "--approve-all", "codex", "sessions", "ensure", "--name", rootPrepared.SessionName}) {
		t.Fatalf("third ACPX call args = %#v, want ensure session", gotCalls[2].args)
	}
	if fmt.Sprint(gotCalls[3].args) != fmt.Sprint([]string{"--format", "json", "--json-strict", "--approve-all", "codex", "prompt", "-s", rootPrepared.SessionName, "thread reply"}) {
		t.Fatalf("fourth ACPX call args = %#v, want reply prompt", gotCalls[3].args)
	}

	client.mu.Lock()
	gotMessages := append([]slack.Message(nil), client.messages...)
	client.mu.Unlock()
	if got, want := len(gotMessages), 3; got != want {
		t.Fatalf("Slack thread message count = %d, want %d", got, want)
	}
	if gotMessages[0].Text != "Progress:\n- Session started: 019db13d-f733-7ce0-8186-5aced7cdb2a7\n- Tool started: Run grep\n- Tool finished: Run grep" {
		t.Fatalf("first Slack message text = %q", gotMessages[0].Text)
	}
	if gotMessages[1].Text != "root final answer" {
		t.Fatalf("second Slack message text = %q, want root final answer", gotMessages[1].Text)
	}
	if gotMessages[2].Text != "reply final answer" {
		t.Fatalf("third Slack message text = %q, want reply final answer", gotMessages[2].Text)
	}
	for i, message := range gotMessages {
		if message.ThreadTS != rootPrepared.ThreadTS {
			t.Fatalf("Slack message %d thread ts = %q, want %q", i, message.ThreadTS, rootPrepared.ThreadTS)
		}
	}

	loadedDedupe, err := store.Runtime().LoadEventDedupe(ctx, "mention", "Ev-root")
	if err != nil {
		t.Fatalf("LoadEventDedupe(root) error = %v", err)
	}
	if loadedDedupe.Status != "processed" || loadedDedupe.ProcessedAt == nil {
		t.Fatalf("LoadEventDedupe(root) = %#v, want processed", loadedDedupe)
	}

	loadedReplyDedupe, err := store.Runtime().LoadEventDedupe(ctx, "mention", "Ev-reply")
	if err != nil {
		t.Fatalf("LoadEventDedupe(reply) error = %v", err)
	}
	if loadedReplyDedupe.Status != "processed" || loadedReplyDedupe.ProcessedAt == nil {
		t.Fatalf("LoadEventDedupe(reply) = %#v, want processed", loadedReplyDedupe)
	}

	loadedState, err := store.Runtime().LoadThreadState(ctx, rootPrepared.ThreadTS)
	if err != nil {
		t.Fatalf("LoadThreadState() error = %v", err)
	}
	if loadedState.LastStatus != "processed" || loadedState.LastRequestID != "Ev-reply" {
		t.Fatalf("LoadThreadState() = %#v, want processed reply state", loadedState)
	}
	if loadedState.SessionName != rootPrepared.SessionName {
		t.Fatalf("LoadThreadState() session name = %q, want %q", loadedState.SessionName, rootPrepared.SessionName)
	}

	if _, err := store.Runtime().LoadThreadLock(ctx, rootPrepared.ThreadTS); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("LoadThreadLock() error = %v, want not found", err)
	}
}

// Test: concurrent prompts in the same Slack thread are serialized by the runtime coordinator even when the store is backed by SQLite.
// Validates: AC-1791 (REQ-1152 - runtime persists thread metadata in SQLite), AC-1794 (REQ-1154 - runtime prevents parallel prompt execution within a thread)
func TestSlackTurnCoordinatorSerializesConcurrentSlackEventsWithSQLiteStore(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	workspacePath := filepath.Join(home, "workspace")
	if err := os.MkdirAll(filepath.Join(workspacePath, "alpha"), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	project := registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(workspacePath, "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C12345678",
	}
	if err := store.Projects().Upsert(ctx, project); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	firstPrepared, err := runtime.PrepareSlackEvent(ctx, store.Projects(), slack.Event{
		ID:        "Ev-1",
		ChannelID: project.SlackChannelID,
		Timestamp: "1713686400.000100",
		UserID:    "U123",
		Text:      "first prompt",
	})
	if err != nil {
		t.Fatalf("PrepareSlackEvent(first) error = %v", err)
	}
	secondPrepared, err := runtime.PrepareSlackEvent(ctx, store.Projects(), slack.Event{
		ID:        "Ev-2",
		ChannelID: project.SlackChannelID,
		ThreadTS:  firstPrepared.ThreadTS,
		Timestamp: "1713686410.000200",
		UserID:    "U123",
		Text:      "second prompt",
	})
	if err != nil {
		t.Fatalf("PrepareSlackEvent(second) error = %v", err)
	}

	coordinator := runtime.NewSlackTurnCoordinator(sqliteRuntimeStore{store: store}, "runtime-1")
	firstEntered := make(chan struct{})
	firstRelease := make(chan struct{})
	secondStarted := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, err := coordinator.Execute(ctx, firstPrepared, func(context.Context, runtime.PreparedSlackEvent) error {
			close(firstEntered)
			<-firstRelease
			return nil
		})
		if err != nil {
			t.Errorf("Execute(first) error = %v", err)
		}
	}()

	<-firstEntered

	go func() {
		defer wg.Done()
		_, err := coordinator.Execute(ctx, secondPrepared, func(context.Context, runtime.PreparedSlackEvent) error {
			close(secondStarted)
			return nil
		})
		if err != nil {
			t.Errorf("Execute(second) error = %v", err)
		}
	}()

	select {
	case <-secondStarted:
		t.Fatalf("second execution started before the first thread execution completed")
	case <-time.After(100 * time.Millisecond):
	}

	close(firstRelease)
	wg.Wait()

	select {
	case <-secondStarted:
	default:
		t.Fatalf("second execution never started after the first completed")
	}

	loadedState, err := store.Runtime().LoadThreadState(ctx, firstPrepared.ThreadTS)
	if err != nil {
		t.Fatalf("LoadThreadState() error = %v", err)
	}
	if loadedState.LastStatus != "processed" || loadedState.LastRequestID != "Ev-2" {
		t.Fatalf("LoadThreadState() = %#v, want final processed second-prompt state", loadedState)
	}

	firstDedupe, err := store.Runtime().LoadEventDedupe(ctx, "mention", "Ev-1")
	if err != nil {
		t.Fatalf("LoadEventDedupe(first) error = %v", err)
	}
	if firstDedupe.Status != "processed" {
		t.Fatalf("LoadEventDedupe(first) = %#v, want processed", firstDedupe)
	}

	secondDedupe, err := store.Runtime().LoadEventDedupe(ctx, "mention", "Ev-2")
	if err != nil {
		t.Fatalf("LoadEventDedupe(second) error = %v", err)
	}
	if secondDedupe.Status != "processed" {
		t.Fatalf("LoadEventDedupe(second) = %#v, want processed", secondDedupe)
	}

	if _, err := store.Runtime().LoadThreadLock(ctx, firstPrepared.ThreadTS); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("LoadThreadLock() error = %v, want not found", err)
	}
}

// Test: registered slash invocations reuse the same channel_id -> project resolution path as mention events without creating thread state yet.
// Validates: AC-1818 (REQ-1185 - slash invocations resolve the project by channel_id)
func TestPrepareSlackInvocationResolvesRegisteredSlashChannelFromSQLite(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	workspacePath := filepath.Join(home, "workspace")
	if err := os.MkdirAll(filepath.Join(workspacePath, "alpha"), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	project := registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(workspacePath, "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C12345678",
	}
	if err := store.Projects().Upsert(ctx, project); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	prepared, err := runtime.PrepareSlackInvocation(ctx, store.Projects(), slack.InboundInvocation{
		SourceType:    slack.InboundSourceSlash,
		DeliveryID:    "3-fwdc2",
		ChannelID:     project.SlackChannelID,
		UserID:        "U123",
		CommandText:   "status",
		ResponseURL:   "https://hooks.slack.test/response",
		AckEnvelopeID: "3-fwdc2",
	})
	if err != nil {
		t.Fatalf("PrepareSlackInvocation() error = %v", err)
	}

	if prepared.Project.Name != "alpha" || prepared.Project.LocalPath != project.LocalPath {
		t.Fatalf("PrepareSlackInvocation() project = %#v, want %#v", prepared.Project, project)
	}
	if prepared.ThreadTS != "" || prepared.SessionName != "" {
		t.Fatalf("PrepareSlackInvocation() thread/session = (%q, %q), want empty for slash", prepared.ThreadTS, prepared.SessionName)
	}
}

// Test: unregistered slash invocations are rejected before execution and return an ephemeral contract from the shared prepare layer.
// Validates: AC-1822 (REQ-1190 - unregistered channels reject before execution starts), AC-1822 (REQ-1192 - slash rejections are ephemeral)
func TestPrepareSlackInvocationRejectsUnregisteredSlashChannelFromSQLite(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	_, err = runtime.PrepareSlackInvocation(ctx, store.Projects(), slack.InboundInvocation{
		SourceType:    slack.InboundSourceSlash,
		DeliveryID:    "3-fwdc2",
		ChannelID:     "C99999999",
		UserID:        "U123",
		CommandText:   "status",
		ResponseURL:   "https://hooks.slack.test/response",
		AckEnvelopeID: "3-fwdc2",
	})
	if !errors.Is(err, runtime.ErrUnregisteredSlackChannel) {
		t.Fatalf("PrepareSlackInvocation() error = %v, want ErrUnregisteredSlackChannel", err)
	}

	var rejectionErr *runtime.RejectedSlackInvocationError
	if !errors.As(err, &rejectionErr) {
		t.Fatalf("PrepareSlackInvocation() error = %v, want RejectedSlackInvocationError", err)
	}
	if !rejectionErr.Rejection.Ephemeral || rejectionErr.Rejection.ResponseURL != "https://hooks.slack.test/response" {
		t.Fatalf("slash rejection = %#v, want ephemeral response contract", rejectionErr.Rejection)
	}
}
