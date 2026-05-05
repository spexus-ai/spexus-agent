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
	executions    map[string]ExecutionState
	threadStates  map[string]ThreadState
	threadLocks   map[string]ThreadLock
	saveEventErr  error
	saveExecErr   error
	saveStateErr  error
	saveLockErr   error
	deleteLockErr error
}

func newFakeExecutionStore() *fakeExecutionStore {
	return &fakeExecutionStore{
		dedupe:       make(map[string]EventDedupe),
		executions:   make(map[string]ExecutionState),
		threadStates: make(map[string]ThreadState),
		threadLocks:  make(map[string]ThreadLock),
	}
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

func (f *fakeExecutionStore) SaveClaimedExecution(_ context.Context, dedupe EventDedupe, state ExecutionState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveEventErr != nil {
		return f.saveEventErr
	}
	if f.saveExecErr != nil {
		return f.saveExecErr
	}
	f.dedupe[dedupe.SourceType+":"+dedupe.DeliveryID] = dedupe
	f.executions[state.SourceType+":"+state.DeliveryID] = state
	return nil
}

func (f *fakeExecutionStore) SaveExecutionState(_ context.Context, state ExecutionState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveExecErr != nil {
		return f.saveExecErr
	}
	f.executions[state.SourceType+":"+state.DeliveryID] = state
	return nil
}

func (f *fakeExecutionStore) LoadExecutionState(_ context.Context, executionID string) (ExecutionState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, state := range f.executions {
		if state.ExecutionID == executionID {
			return state, nil
		}
	}
	return ExecutionState{}, ErrNotFound
}

func (f *fakeExecutionStore) LoadExecutionStateByDelivery(_ context.Context, sourceType, deliveryID string) (ExecutionState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	state, ok := f.executions[sourceType+":"+deliveryID]
	if !ok {
		return ExecutionState{}, ErrNotFound
	}
	return state, nil
}

func (f *fakeExecutionStore) LoadLatestExecutionByThread(_ context.Context, threadTS string) (ExecutionState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, state := range f.executions {
		if state.ThreadTS == threadTS {
			return state, nil
		}
	}
	return ExecutionState{}, ErrNotFound
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

// Test: accepting an invocation persists the dedupe claim together with a queued execution state before enqueue continues.
// Validates: AC-1981 (REQ-1429 - accepted deliveries persist an initial queued execution state at claim time), AC-1982 (REQ-1432 - startup recovery can observe accepted deliveries before worker start)
func TestSlackTurnCoordinatorClaimExecutionPersistsQueuedState(t *testing.T) {
	t.Parallel()

	store := newFakeExecutionStore()
	coordinator := NewSlackTurnCoordinator(store, "runtime-1")

	claimedAt := time.Date(2026, time.April, 26, 19, 0, 0, 0, time.UTC)
	coordinator.now = func() time.Time {
		return claimedAt
	}

	request := ExecutionRequest{
		SourceType:  "mention",
		DeliveryID:  "Ev-queued",
		ChannelID:   "C12345678",
		CommandText: "@agent status",
		Project: registry.Project{
			Name:           "alpha",
			SlackChannelID: "C12345678",
		},
		ThreadTS:    "1713686400.000100",
		SessionName: "slack-1713686400.000100",
	}

	result, err := coordinator.ClaimExecution(context.Background(), request)
	if err != nil {
		t.Fatalf("ClaimExecution() error = %v", err)
	}
	if !result.Executed || result.Duplicate {
		t.Fatalf("ClaimExecution() result = %#v, want executed non-duplicate claim", result)
	}

	dedupe, err := store.LoadEventDedupe(context.Background(), request.SourceType, request.DeliveryID)
	if err != nil {
		t.Fatalf("LoadEventDedupe() error = %v", err)
	}
	if dedupe.Status != "acked" || !dedupe.ReceivedAt.Equal(claimedAt) {
		t.Fatalf("LoadEventDedupe() = %#v, want acked at %s", dedupe, claimedAt)
	}

	state, err := store.LoadExecutionStateByDelivery(context.Background(), request.SourceType, request.DeliveryID)
	if err != nil {
		t.Fatalf("LoadExecutionStateByDelivery() error = %v", err)
	}
	if state.ExecutionID != request.ExecutionID() || state.Status != ExecutionStatusQueued {
		t.Fatalf("LoadExecutionStateByDelivery() = %#v, want queued execution state", state)
	}
	if !state.QueuedAt.Equal(claimedAt) || !state.UpdatedAt.Equal(claimedAt) {
		t.Fatalf("LoadExecutionStateByDelivery() timestamps = %#v, want queued/updated at claim time", state)
	}
	if state.ThreadTS != request.ThreadTS || state.SessionName != request.SessionName {
		t.Fatalf("LoadExecutionStateByDelivery() thread/session = (%q, %q), want request thread/session", state.ThreadTS, state.SessionName)
	}
}
