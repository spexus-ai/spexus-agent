package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/registry"
	"github.com/spexus-ai/spexus-agent/internal/slack"
)

type fakeExecutionStore struct {
	mu            sync.Mutex
	dedupe        map[string]EventDedupe
	executions    map[string]ExecutionRequest
	threadStates  map[string]ThreadState
	threadLocks   map[string]ThreadLock
	saveEventErr  error
	saveStateErr  error
	saveLockErr   error
	deleteLockErr error
}

func newFakeExecutionStore() *fakeExecutionStore {
	return &fakeExecutionStore{
		dedupe:       make(map[string]EventDedupe),
		executions:   make(map[string]ExecutionRequest),
		threadStates: make(map[string]ThreadState),
		threadLocks:  make(map[string]ThreadLock),
	}
}

func (f *fakeExecutionStore) CreateExecution(_ context.Context, request ExecutionRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.executions[request.ExecutionID] = request
	return nil
}

func (f *fakeExecutionStore) LoadExecution(_ context.Context, executionID string) (ExecutionRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	request, ok := f.executions[executionID]
	if !ok {
		return ExecutionRequest{}, ErrNotFound
	}
	return request, nil
}

func (f *fakeExecutionStore) ListExecutions(_ context.Context, statuses []ExecutionLifecycleState) ([]ExecutionRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	filtered := make([]ExecutionRequest, 0, len(f.executions))
	for _, request := range f.executions {
		if len(statuses) == 0 {
			filtered = append(filtered, request)
			continue
		}

		for _, status := range statuses {
			if request.Status == status {
				filtered = append(filtered, request)
				break
			}
		}
	}

	return filtered, nil
}

func (f *fakeExecutionStore) UpdateExecutionState(_ context.Context, state ExecutionState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	request, ok := f.executions[state.ExecutionID]
	if !ok {
		return ErrNotFound
	}
	request.Status = state.Status
	request.DiagnosticContext = state.DiagnosticContext
	if state.StartedAt != nil {
		request.StartedAt = state.StartedAt
	}
	request.UpdatedAt = state.UpdatedAt
	if state.CompletedAt != nil {
		request.CompletedAt = state.CompletedAt
	}
	f.executions[state.ExecutionID] = request
	return nil
}

func (f *fakeExecutionStore) SaveThreadState(_ context.Context, state ThreadState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveStateErr != nil {
		return f.saveStateErr
	}
	f.threadStates[state.ThreadTS] = state
	return nil
}

func (f *fakeExecutionStore) LoadThreadState(_ context.Context, threadTS string) (ThreadState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	state, ok := f.threadStates[threadTS]
	if !ok {
		return ThreadState{}, ErrNotFound
	}
	return state, nil
}

func (f *fakeExecutionStore) SaveEventDedupe(_ context.Context, dedupe EventDedupe) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveEventErr != nil {
		return f.saveEventErr
	}
	f.dedupe[dedupe.SourceType+":"+dedupe.DeliveryID] = dedupe
	return nil
}

func (f *fakeExecutionStore) LoadEventDedupe(_ context.Context, sourceType, deliveryID string) (EventDedupe, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	dedupe, ok := f.dedupe[sourceType+":"+deliveryID]
	if !ok {
		return EventDedupe{}, ErrNotFound
	}
	return dedupe, nil
}

func (f *fakeExecutionStore) SaveThreadLock(_ context.Context, lock ThreadLock) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveLockErr != nil {
		return f.saveLockErr
	}
	f.threadLocks[lock.ThreadTS] = lock
	return nil
}

func (f *fakeExecutionStore) LoadThreadLock(_ context.Context, threadTS string) (ThreadLock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lock, ok := f.threadLocks[threadTS]
	if !ok {
		return ThreadLock{}, ErrNotFound
	}
	return lock, nil
}

func (f *fakeExecutionStore) DeleteThreadLock(_ context.Context, threadTS string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteLockErr != nil {
		return f.deleteLockErr
	}
	delete(f.threadLocks, threadTS)
	return nil
}

// Test: duplicate Slack event ids are skipped so Slack retries cannot re-run the same prompt.
// Validates: AC-1792 (REQ-1153 - Slack retries are deduplicated and do not execute the same prompt more than once), AC-1791 (REQ-1152 - runtime persistence stores dedupe metadata)
func TestSlackTurnCoordinatorSkipsDuplicateSlackEvent(t *testing.T) {
	t.Parallel()

	store := newFakeExecutionStore()
	store.dedupe["mention:Ev123"] = EventDedupe{
		SourceType: "mention",
		DeliveryID: "Ev123",
		ReceivedAt: time.Now().UTC(),
		Status:     "processed",
	}

	coordinator := NewSlackTurnCoordinator(store, "runtime-1")
	prepared := PreparedSlackEvent{
		SourceType: "mention",
		DeliveryID: "Ev123",
		Event:      slack.Event{ID: "Ev123"},
		ThreadTS:   "1713686400.000100",
		ThreadState: ThreadState{
			ThreadTS:    "1713686400.000100",
			ChannelID:   "C12345678",
			ProjectName: "alpha",
			SessionName: "slack-1713686400.000100",
		},
	}

	called := false
	result, err := coordinator.Execute(context.Background(), prepared, func(context.Context, PreparedSlackEvent) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.Duplicate {
		t.Fatalf("Execute() result = %#v, want duplicate", result)
	}
	if called {
		t.Fatalf("Execute() callback was called for duplicate event")
	}
}

// Test: accepted Slack invocations are persisted as queued execution requests before async execution starts.
// Validates: AC-2107 (REQ-1572 - accepted invocation persists an execution request with a unique execution identifier), AC-2108 (REQ-1574 - invalid or duplicate invocations do not create a second execution request)
func TestSlackTurnCoordinatorAcceptPersistsQueuedExecutionRequest(t *testing.T) {
	t.Parallel()

	store := newFakeExecutionStore()
	coordinator := NewSlackTurnCoordinator(store, "runtime-1")
	coordinator.SetExecutionIDGenerator(func() string { return "exec-1" })
	coordinator.now = func() time.Time { return time.Unix(1713686400, 0).UTC() }

	prepared := PreparedSlackEvent{
		SourceType: "mention",
		DeliveryID: "Ev-accept",
		Event: slack.Event{
			ID:        "Ev-accept",
			ChannelID: "C12345678",
			Text:      "status",
		},
		Project: registry.Project{
			Name: "alpha",
		},
		ThreadTS:    "1713686400.000100",
		SessionName: "slack-1713686400.000100",
		ThreadState: ThreadState{
			ThreadTS:    "1713686400.000100",
			ChannelID:   "C12345678",
			ProjectName: "alpha",
			SessionName: "slack-1713686400.000100",
		},
	}

	accepted, result, err := coordinator.Accept(context.Background(), prepared)
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
	if !result.Executed || result.Duplicate {
		t.Fatalf("Accept() result = %#v, want accepted and not duplicate", result)
	}
	if accepted.Execution.ExecutionID != "exec-1" {
		t.Fatalf("Accept() execution id = %q, want exec-1", accepted.Execution.ExecutionID)
	}
	if got := store.executions["exec-1"]; got.Status != ExecutionStateQueued || got.CommandText != "status" {
		t.Fatalf("persisted execution = %#v, want queued status and command text", got)
	}
	if got := store.dedupe["mention:Ev-accept"].Status; got != "queued" {
		t.Fatalf("dedupe status after Accept() = %q, want queued", got)
	}
	if got := store.threadStates["1713686400.000100"].LastStatus; got != "queued" {
		t.Fatalf("thread status after Accept() = %q, want queued", got)
	}
	if _, ok := store.threadLocks["1713686400.000100"]; !ok {
		t.Fatalf("thread lock missing after Accept()")
	}
}

// Test: duplicate accepted invocations are rejected before a second execution request can be persisted.
// Validates: REQ-1574 (dedupe rejection does not create an execution request)
func TestSlackTurnCoordinatorAcceptSkipsDuplicateWithoutCreatingExecution(t *testing.T) {
	t.Parallel()

	store := newFakeExecutionStore()
	store.dedupe["mention:Ev-duplicate"] = EventDedupe{
		SourceType:  "mention",
		DeliveryID:  "Ev-duplicate",
		ReceivedAt:  time.Unix(1713686400, 0).UTC(),
		ProcessedAt: timePtr(time.Unix(1713686405, 0).UTC()),
		Status:      "processed",
	}

	coordinator := NewSlackTurnCoordinator(store, "runtime-1")
	coordinator.SetExecutionIDGenerator(func() string { return "exec-duplicate" })

	accepted, result, err := coordinator.Accept(context.Background(), PreparedSlackEvent{
		SourceType: "mention",
		DeliveryID: "Ev-duplicate",
		Event: slack.Event{
			ID:        "Ev-duplicate",
			ChannelID: "C12345678",
			Text:      "status",
		},
		Project: registry.Project{
			Name: "alpha",
		},
		ThreadTS:    "1713686400.000100",
		SessionName: "slack-1713686400.000100",
		ThreadState: ThreadState{
			ThreadTS:    "1713686400.000100",
			ChannelID:   "C12345678",
			ProjectName: "alpha",
			SessionName: "slack-1713686400.000100",
		},
	})
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
	if !result.Duplicate || result.Executed {
		t.Fatalf("Accept() result = %#v, want duplicate without execution", result)
	}
	if accepted.Execution.ExecutionID != "" {
		t.Fatalf("Accept() execution = %#v, want empty accepted execution for duplicate", accepted.Execution)
	}
	if got := len(store.executions); got != 0 {
		t.Fatalf("execution count after duplicate Accept() = %d, want 0", got)
	}
}

// Test: accepted executions transition to running and terminal success when the async worker completes.
// Validates: AC-2113 (REQ-1583 - lifecycle transitions persist timestamps for async execution requests)
func TestSlackTurnCoordinatorExecuteAcceptedPersistsTerminalState(t *testing.T) {
	t.Parallel()

	store := newFakeExecutionStore()
	coordinator := NewSlackTurnCoordinator(store, "runtime-1")
	coordinator.SetExecutionIDGenerator(func() string { return "exec-2" })
	now := time.Unix(1713686400, 0).UTC()
	coordinator.now = func() time.Time {
		now = now.Add(time.Second)
		return now
	}

	prepared := PreparedSlackEvent{
		SourceType: "message",
		DeliveryID: "Ev-run",
		Event: slack.Event{
			ID:        "Ev-run",
			ChannelID: "C12345678",
			Text:      "summarize current state",
		},
		Project: registry.Project{
			Name: "alpha",
		},
		ThreadTS:    "1713686400.000100",
		SessionName: "slack-1713686400.000100",
		ThreadState: ThreadState{
			ThreadTS:    "1713686400.000100",
			ChannelID:   "C12345678",
			ProjectName: "alpha",
			SessionName: "slack-1713686400.000100",
		},
	}

	accepted, _, err := coordinator.Accept(context.Background(), prepared)
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}

	called := false
	result, err := coordinator.ExecuteAccepted(context.Background(), accepted, func(context.Context, PreparedSlackEvent) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("ExecuteAccepted() error = %v", err)
	}
	if !called {
		t.Fatalf("ExecuteAccepted() callback was not called")
	}
	if result.CompletedAt.IsZero() {
		t.Fatalf("ExecuteAccepted() completed_at is zero")
	}

	execution := store.executions["exec-2"]
	if execution.Status != ExecutionStateSucceeded {
		t.Fatalf("execution status after ExecuteAccepted() = %q, want succeeded", execution.Status)
	}
	if execution.StartedAt == nil || execution.CompletedAt == nil {
		t.Fatalf("execution timestamps after ExecuteAccepted() = %#v, want started and completed timestamps", execution)
	}
	if got := store.dedupe["message:Ev-run"].Status; got != "processed" {
		t.Fatalf("dedupe status after ExecuteAccepted() = %q, want processed", got)
	}
	if got := store.threadStates["1713686400.000100"].LastStatus; got != "processed" {
		t.Fatalf("thread status after ExecuteAccepted() = %q, want processed", got)
	}
	if len(store.threadLocks) != 0 {
		t.Fatalf("thread locks left behind after ExecuteAccepted(): %#v", store.threadLocks)
	}
}

// Test: thread-scoped execution is serialized so a second prompt in the same thread cannot run in parallel.
// Validates: AC-1794 (REQ-1154 - runtime prevents parallel prompt execution within a thread), AC-1791 (REQ-1152 - thread runtime metadata is persisted)
func TestSlackTurnCoordinatorSerializesThreadExecution(t *testing.T) {
	t.Parallel()

	store := newFakeExecutionStore()
	coordinator := NewSlackTurnCoordinator(store, "runtime-1")

	firstStarted := make(chan struct{})
	firstRelease := make(chan struct{})
	secondStarted := make(chan struct{})
	events := make(chan string, 4)

	firstPrepared := PreparedSlackEvent{
		SourceType: "mention",
		DeliveryID: "Ev1",
		Event:      slack.Event{ID: "Ev1"},
		ThreadTS:   "1713686400.000100",
		ThreadState: ThreadState{
			ThreadTS:    "1713686400.000100",
			ChannelID:   "C12345678",
			ProjectName: "alpha",
			SessionName: "slack-1713686400.000100",
		},
	}
	secondPrepared := PreparedSlackEvent{
		SourceType: "mention",
		DeliveryID: "Ev2",
		Event:      slack.Event{ID: "Ev2"},
		ThreadTS:   "1713686400.000100",
		ThreadState: ThreadState{
			ThreadTS:    "1713686400.000100",
			ChannelID:   "C12345678",
			ProjectName: "alpha",
			SessionName: "slack-1713686400.000100",
		},
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, err := coordinator.Execute(context.Background(), firstPrepared, func(context.Context, PreparedSlackEvent) error {
			events <- "first-start"
			close(firstStarted)
			<-firstRelease
			events <- "first-end"
			return nil
		})
		if err != nil {
			t.Errorf("first Execute() error = %v", err)
		}
	}()

	<-firstStarted

	go func() {
		defer wg.Done()
		_, err := coordinator.Execute(context.Background(), secondPrepared, func(context.Context, PreparedSlackEvent) error {
			events <- "second-start"
			close(secondStarted)
			return nil
		})
		if err != nil {
			t.Errorf("second Execute() error = %v", err)
		}
	}()

	select {
	case <-secondStarted:
		t.Fatalf("second execution started before the first thread execution completed")
	case <-time.After(100 * time.Millisecond):
	}

	close(firstRelease)
	wg.Wait()

	close(events)
	ordered := make([]string, 0, len(events))
	for event := range events {
		ordered = append(ordered, event)
	}

	want := []string{"first-start", "first-end", "second-start"}
	if len(ordered) != len(want) {
		t.Fatalf("execution order = %#v, want %#v", ordered, want)
	}
	for i := range want {
		if ordered[i] != want[i] {
			t.Fatalf("execution order = %#v, want %#v", ordered, want)
		}
	}
	if got := store.dedupe["mention:Ev1"].Status; got != "processed" {
		t.Fatalf("dedupe status for first execution = %q, want processed", got)
	}
	if got := store.threadStates["1713686400.000100"].LastStatus; got != "processed" {
		t.Fatalf("thread state status = %q, want processed", got)
	}
	if len(store.threadLocks) != 0 {
		t.Fatalf("thread locks left behind after execution: %#v", store.threadLocks)
	}
}

// Test: persistence failures stop the runtime before callback execution so unsafe continuation is impossible.
// Validates: AC-1793 (REQ-1155 - persistence failures fail the operation in a controlled manner and do not continue unsafely)
func TestSlackTurnCoordinatorFailsClosedOnPersistenceError(t *testing.T) {
	t.Parallel()

	store := newFakeExecutionStore()
	store.saveEventErr = errors.New("disk full")

	coordinator := NewSlackTurnCoordinator(store, "runtime-1")
	prepared := PreparedSlackEvent{
		SourceType: "mention",
		DeliveryID: "Ev123",
		Event:      slack.Event{ID: "Ev123"},
		ThreadTS:   "1713686400.000100",
		ThreadState: ThreadState{
			ThreadTS:    "1713686400.000100",
			ChannelID:   "C12345678",
			ProjectName: "alpha",
			SessionName: "slack-1713686400.000100",
		},
	}

	called := false
	_, err := coordinator.Execute(context.Background(), prepared, func(context.Context, PreparedSlackEvent) error {
		called = true
		return nil
	})
	if err == nil {
		t.Fatalf("Execute() error = nil, want persistence failure")
	}
	if called {
		t.Fatalf("Execute() callback was called after persistence failure")
	}
	if len(store.threadStates) != 0 {
		t.Fatalf("thread states persisted unexpectedly: %#v", store.threadStates)
	}
	if len(store.threadLocks) != 0 {
		t.Fatalf("thread locks persisted unexpectedly: %#v", store.threadLocks)
	}
}
