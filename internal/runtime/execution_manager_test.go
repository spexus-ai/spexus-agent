package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/registry"
	"github.com/spexus-ai/spexus-agent/internal/slack"
)

// Test: queued execution requests are started by ExecutionManager when admission allows immediate startup.
// Validates: AC-2109 (REQ-1575 - ExecutionManager decides whether queued execution starts now or remains queued)
func TestExecutionManagerStartsAcceptedExecution(t *testing.T) {
	t.Parallel()

	store := newFakeExecutionStore()
	coordinator := NewSlackTurnCoordinator(store, "runtime-1")
	now := time.Unix(1713686400, 0).UTC()
	coordinator.now = func() time.Time {
		now = now.Add(time.Second)
		return now
	}
	manager := NewExecutionManager(coordinator, nil)

	accepted := acceptExecutionForManagerTest(t, coordinator, "exec-manager-start", "Ev-manager-start", "1713686400.000100", "slack-1713686400.000100")

	executed := make(chan struct{}, 1)
	completed := make(chan SlackTurnExecutionResult, 1)
	failed := make(chan error, 1)
	result, err := manager.ScheduleAccepted(context.Background(), ManagedExecution{
		Accepted: accepted,
		Execute: func(context.Context, PreparedSlackEvent) error {
			executed <- struct{}{}
			return nil
		},
		Callbacks: ExecutionCallbacks{
			OnCompleted: func(result SlackTurnExecutionResult) {
				completed <- result
			},
			OnFailed: func(err error) {
				failed <- err
			},
		},
	})
	if err != nil {
		t.Fatalf("ScheduleAccepted() error = %v", err)
	}
	if !result.Started {
		t.Fatalf("ScheduleAccepted() result = %#v, want started", result)
	}

	select {
	case <-executed:
	case err := <-failed:
		t.Fatalf("OnFailed() error = %v", err)
	case <-time.After(time.Second):
		t.Fatal("execution callback was not started")
	}

	select {
	case callbackResult := <-completed:
		if callbackResult.CompletedAt.IsZero() {
			t.Fatalf("OnCompleted() result = %#v, want completed_at", callbackResult)
		}
	case err := <-failed:
		t.Fatalf("OnFailed() error = %v", err)
	case <-time.After(time.Second):
		t.Fatal("OnCompleted() callback was not called")
	}

	execution := store.executions["exec-manager-start"]
	if execution.Status != ExecutionStateSucceeded {
		t.Fatalf("execution status after ScheduleAccepted() = %q, want succeeded", execution.Status)
	}
}

// Test: queued execution requests remain queued when the global concurrency limit is full and start after capacity is released.
// Validates: AC-2109 (REQ-1576 - global concurrency keeps extra requests queued until capacity becomes available)
func TestExecutionManagerQueuesWhenGlobalConcurrencyLimitIsReached(t *testing.T) {
	t.Parallel()

	store := newFakeExecutionStore()
	coordinator := NewSlackTurnCoordinator(store, "runtime-1")
	coordinator.now = advancingExecutionManagerClock()
	manager := NewExecutionManager(coordinator, boundedExecutionAdmissionPolicy{globalLimit: 1})

	firstAccepted := acceptExecutionForManagerTest(t, coordinator, "exec-manager-limit-1", "Ev-manager-limit-1", "1713686400.000100", "slack-1713686400.000100")
	secondAccepted := acceptExecutionForManagerTest(t, coordinator, "exec-manager-limit-2", "Ev-manager-limit-2", "1713686400.000200", "slack-1713686400.000200")

	firstStarted := make(chan struct{}, 1)
	firstRelease := make(chan struct{})
	firstCompleted := make(chan SlackTurnExecutionResult, 1)
	firstFailed := make(chan error, 1)
	firstResult, err := manager.ScheduleAccepted(context.Background(), ManagedExecution{
		Accepted: firstAccepted,
		Execute: func(context.Context, PreparedSlackEvent) error {
			firstStarted <- struct{}{}
			<-firstRelease
			return nil
		},
		Callbacks: ExecutionCallbacks{
			OnCompleted: func(result SlackTurnExecutionResult) {
				firstCompleted <- result
			},
			OnFailed: func(err error) {
				firstFailed <- err
			},
		},
	})
	if err != nil {
		t.Fatalf("ScheduleAccepted() error = %v", err)
	}
	if !firstResult.Started {
		t.Fatalf("first ScheduleAccepted() result = %#v, want started", firstResult)
	}

	waitForExecutionSignal(t, firstStarted, firstFailed, "first execution start")

	secondStarted := make(chan struct{}, 1)
	secondCompleted := make(chan SlackTurnExecutionResult, 1)
	secondFailed := make(chan error, 1)
	secondResult, err := manager.ScheduleAccepted(context.Background(), ManagedExecution{
		Accepted: secondAccepted,
		Execute: func(context.Context, PreparedSlackEvent) error {
			secondStarted <- struct{}{}
			return nil
		},
		Callbacks: ExecutionCallbacks{
			OnCompleted: func(result SlackTurnExecutionResult) {
				secondCompleted <- result
			},
			OnFailed: func(err error) {
				secondFailed <- err
			},
		},
	})
	if err != nil {
		t.Fatalf("second ScheduleAccepted() error = %v", err)
	}
	if secondResult.Started {
		t.Fatalf("second ScheduleAccepted() result = %#v, want deferred start", secondResult)
	}
	if !strings.Contains(secondResult.Decision.Reason, "global concurrency limit reached") {
		t.Fatalf("second ScheduleAccepted() decision reason = %q, want global concurrency limit", secondResult.Decision.Reason)
	}

	select {
	case <-secondStarted:
		t.Fatal("second execution started even though the global limit was full")
	case <-time.After(50 * time.Millisecond):
	}

	if execution := store.executions["exec-manager-limit-2"]; execution.Status != ExecutionStateQueued {
		t.Fatalf("queued execution status = %q, want queued", execution.Status)
	}

	close(firstRelease)

	select {
	case <-firstCompleted:
	case err := <-firstFailed:
		t.Fatalf("first execution failed = %v", err)
	case <-time.After(time.Second):
		t.Fatal("first execution did not complete")
	}

	waitForExecutionSignal(t, secondStarted, secondFailed, "second execution start after capacity release")

	select {
	case <-secondCompleted:
	case err := <-secondFailed:
		t.Fatalf("second execution failed = %v", err)
	case <-time.After(time.Second):
		t.Fatal("second execution did not complete")
	}

	if execution := store.executions["exec-manager-limit-2"]; execution.Status != ExecutionStateSucceeded {
		t.Fatalf("deferred execution status after capacity release = %q, want succeeded", execution.Status)
	}
}

// Test: terminal failed executions from cancel-routing failures do not continue occupying admission capacity for the same session.
// Validates: AC-2116 (REQ-1587 - cancellation routing failure persists a terminal outcome), AC-2110 (REQ-1577 - session serialization only blocks while another execution is still running)
func TestExecutionManagerIgnoresTerminalCancellationRoutingFailureForAdmission(t *testing.T) {
	t.Parallel()

	store := newFakeExecutionStore()
	failedAt := time.Unix(1713686400, 0).UTC()
	store.executions["exec-manager-cancel-routing-failed"] = ExecutionRequest{
		ExecutionID:       "exec-manager-cancel-routing-failed",
		SourceType:        "message",
		DeliveryID:        "Ev-manager-cancel-routing-failed",
		ChannelID:         "C12345678",
		ProjectName:       "alpha",
		SessionKey:        "slack-1713686400.000100",
		ThreadTS:          "1713686400.000100",
		CommandText:       "cancel failed",
		Status:            ExecutionStateFailed,
		DiagnosticContext: "cancel routing failed: execution owner unavailable",
		CreatedAt:         failedAt.Add(-time.Minute),
		StartedAt:         timePtr(failedAt.Add(-30 * time.Second)),
		UpdatedAt:         failedAt,
		CompletedAt:       timePtr(failedAt),
	}

	coordinator := NewSlackTurnCoordinator(store, "runtime-1")
	coordinator.now = advancingExecutionManagerClock()
	manager := NewExecutionManager(coordinator, boundedExecutionAdmissionPolicy{globalLimit: 1})

	accepted := acceptExecutionForManagerTest(t, coordinator, "exec-manager-after-cancel-routing-failure", "Ev-manager-after-cancel-routing-failure", "1713686400.000200", "slack-1713686400.000100")

	executed := make(chan struct{}, 1)
	completed := make(chan SlackTurnExecutionResult, 1)
	failed := make(chan error, 1)
	result, err := manager.ScheduleAccepted(context.Background(), ManagedExecution{
		Accepted: accepted,
		Execute: func(context.Context, PreparedSlackEvent) error {
			executed <- struct{}{}
			return nil
		},
		Callbacks: ExecutionCallbacks{
			OnCompleted: func(result SlackTurnExecutionResult) {
				completed <- result
			},
			OnFailed: func(err error) {
				failed <- err
			},
		},
	})
	if err != nil {
		t.Fatalf("ScheduleAccepted() error = %v", err)
	}
	if !result.Started {
		t.Fatalf("ScheduleAccepted() result = %#v, want started", result)
	}

	select {
	case <-executed:
	case err := <-failed:
		t.Fatalf("OnFailed() error = %v", err)
	case <-time.After(time.Second):
		t.Fatal("execution callback was not started")
	}

	select {
	case <-completed:
	case err := <-failed:
		t.Fatalf("OnFailed() error = %v", err)
	case <-time.After(time.Second):
		t.Fatal("OnCompleted() callback was not called")
	}
}

// Test: cancel-routing failure releases live manager admission so the next queued same-session execution starts before the original worker exits.
// Validates: AC-2116 (REQ-1587 - cancellation routing failure persists a terminal outcome), AC-2110 (REQ-1577 - same-session startup can resume immediately after terminal failure)
func TestExecutionManagerReleasesLiveAdmissionAfterCancellationRoutingFailure(t *testing.T) {
	t.Parallel()

	store := newFakeExecutionStore()
	coordinator := NewSlackTurnCoordinator(store, "runtime-1")
	coordinator.now = advancingExecutionManagerClock()
	manager := NewExecutionManager(coordinator, boundedExecutionAdmissionPolicy{globalLimit: 1})

	firstAccepted := acceptExecutionForManagerTest(t, coordinator, "exec-manager-live-cancel-routing-1", "Ev-manager-live-cancel-routing-1", "1713686400.000100", "slack-1713686400.000100")
	secondAccepted := acceptExecutionForManagerTest(t, coordinator, "exec-manager-live-cancel-routing-2", "Ev-manager-live-cancel-routing-2", "1713686400.000200", "slack-1713686400.000100")

	firstStarted := make(chan struct{}, 1)
	firstRelease := make(chan struct{})
	firstCompleted := make(chan SlackTurnExecutionResult, 1)
	firstFailed := make(chan error, 1)
	firstResult, err := manager.ScheduleAccepted(context.Background(), ManagedExecution{
		Accepted: firstAccepted,
		Execute: func(context.Context, PreparedSlackEvent) error {
			firstStarted <- struct{}{}
			<-firstRelease
			return nil
		},
		Callbacks: ExecutionCallbacks{
			OnCompleted: func(result SlackTurnExecutionResult) {
				firstCompleted <- result
			},
			OnFailed: func(err error) {
				firstFailed <- err
			},
		},
	})
	if err != nil {
		t.Fatalf("first ScheduleAccepted() error = %v", err)
	}
	if !firstResult.Started {
		t.Fatalf("first ScheduleAccepted() result = %#v, want started", firstResult)
	}

	waitForExecutionSignal(t, firstStarted, firstFailed, "first execution start")

	secondStarted := make(chan struct{}, 1)
	secondCompleted := make(chan SlackTurnExecutionResult, 1)
	secondFailed := make(chan error, 1)
	secondResult, err := manager.ScheduleAccepted(context.Background(), ManagedExecution{
		Accepted: secondAccepted,
		Execute: func(context.Context, PreparedSlackEvent) error {
			secondStarted <- struct{}{}
			return nil
		},
		Callbacks: ExecutionCallbacks{
			OnCompleted: func(result SlackTurnExecutionResult) {
				secondCompleted <- result
			},
			OnFailed: func(err error) {
				secondFailed <- err
			},
		},
	})
	if err != nil {
		t.Fatalf("second ScheduleAccepted() error = %v", err)
	}
	if secondResult.Started {
		t.Fatalf("second ScheduleAccepted() result = %#v, want deferred start", secondResult)
	}

	failedAt := time.Unix(1713686460, 0).UTC()
	if err := store.UpdateExecutionState(context.Background(), ExecutionState{
		ExecutionID:       firstAccepted.Execution.ExecutionID,
		Status:            ExecutionStateFailed,
		DiagnosticContext: "cancel routing failed: execution owner unavailable",
		StartedAt:         store.executions[firstAccepted.Execution.ExecutionID].StartedAt,
		UpdatedAt:         failedAt,
		CompletedAt:       timePtr(failedAt),
	}); err != nil {
		t.Fatalf("UpdateExecutionState(failed) error = %v", err)
	}
	if err := ReleaseExecutionAdmission(context.Background(), firstAccepted.Execution.ExecutionID); err != nil {
		t.Fatalf("ReleaseExecutionAdmission() error = %v", err)
	}

	waitForExecutionSignal(t, secondStarted, secondFailed, "second execution start after cancellation routing failure")

	select {
	case <-secondCompleted:
	case err := <-secondFailed:
		t.Fatalf("second execution failed = %v", err)
	case <-time.After(time.Second):
		t.Fatal("second execution did not complete")
	}

	select {
	case <-firstCompleted:
		t.Fatal("first execution completed before explicit release")
	default:
	}

	close(firstRelease)

	select {
	case <-firstCompleted:
	case err := <-firstFailed:
		t.Fatalf("first execution failed = %v", err)
	case <-time.After(time.Second):
		t.Fatal("first execution did not complete after explicit release")
	}

	if execution := store.executions[firstAccepted.Execution.ExecutionID]; execution.Status != ExecutionStateFailed {
		t.Fatalf("first execution status after cancel-routing failure = %q, want failed", execution.Status)
	}
	if execution := store.executions[secondAccepted.Execution.ExecutionID]; execution.Status != ExecutionStateSucceeded {
		t.Fatalf("second execution status after admission release = %q, want succeeded", execution.Status)
	}
}

// Test: queued execution requests for the same session key stay serialized until the active execution reaches a terminal state.
// Validates: AC-2110 (REQ-1577 - only one execution for the same session key runs at a time)
func TestExecutionManagerSerializesSameSessionKey(t *testing.T) {
	t.Parallel()

	store := newFakeExecutionStore()
	coordinator := NewSlackTurnCoordinator(store, "runtime-1")
	coordinator.now = advancingExecutionManagerClock()
	manager := NewExecutionManager(coordinator, boundedExecutionAdmissionPolicy{globalLimit: 2})

	firstAccepted := acceptExecutionForManagerTest(t, coordinator, "exec-manager-session-1", "Ev-manager-session-1", "1713686400.000100", "slack-shared-session")
	secondAccepted := acceptExecutionForManagerTest(t, coordinator, "exec-manager-session-2", "Ev-manager-session-2", "1713686400.000100", "slack-shared-session")

	firstStarted := make(chan struct{}, 1)
	firstRelease := make(chan struct{})
	firstCompleted := make(chan SlackTurnExecutionResult, 1)
	firstFailed := make(chan error, 1)
	firstResult, err := manager.ScheduleAccepted(context.Background(), ManagedExecution{
		Accepted: firstAccepted,
		Execute: func(context.Context, PreparedSlackEvent) error {
			firstStarted <- struct{}{}
			<-firstRelease
			return nil
		},
		Callbacks: ExecutionCallbacks{
			OnCompleted: func(result SlackTurnExecutionResult) {
				firstCompleted <- result
			},
			OnFailed: func(err error) {
				firstFailed <- err
			},
		},
	})
	if err != nil {
		t.Fatalf("first ScheduleAccepted() error = %v", err)
	}
	if !firstResult.Started {
		t.Fatalf("first ScheduleAccepted() result = %#v, want started", firstResult)
	}

	waitForExecutionSignal(t, firstStarted, firstFailed, "first execution start")

	secondStarted := make(chan struct{}, 1)
	secondCompleted := make(chan SlackTurnExecutionResult, 1)
	secondFailed := make(chan error, 1)
	secondResult, err := manager.ScheduleAccepted(context.Background(), ManagedExecution{
		Accepted: secondAccepted,
		Execute: func(context.Context, PreparedSlackEvent) error {
			secondStarted <- struct{}{}
			return nil
		},
		Callbacks: ExecutionCallbacks{
			OnCompleted: func(result SlackTurnExecutionResult) {
				secondCompleted <- result
			},
			OnFailed: func(err error) {
				secondFailed <- err
			},
		},
	})
	if err != nil {
		t.Fatalf("second ScheduleAccepted() error = %v", err)
	}
	if secondResult.Started {
		t.Fatalf("second ScheduleAccepted() result = %#v, want deferred start", secondResult)
	}
	if !strings.Contains(secondResult.Decision.Reason, `session key "slack-shared-session" already running`) {
		t.Fatalf("second ScheduleAccepted() decision reason = %q, want same-session serialization", secondResult.Decision.Reason)
	}

	select {
	case <-secondStarted:
		t.Fatal("second execution started while the same session key was still running")
	case <-time.After(50 * time.Millisecond):
	}

	if execution := store.executions["exec-manager-session-2"]; execution.Status != ExecutionStateQueued {
		t.Fatalf("same-session queued execution status = %q, want queued", execution.Status)
	}

	close(firstRelease)

	select {
	case <-firstCompleted:
	case err := <-firstFailed:
		t.Fatalf("first execution failed = %v", err)
	case <-time.After(time.Second):
		t.Fatal("first execution did not complete")
	}

	waitForExecutionSignal(t, secondStarted, secondFailed, "second execution start after same-session release")

	select {
	case <-secondCompleted:
	case err := <-secondFailed:
		t.Fatalf("second execution failed = %v", err)
	case <-time.After(time.Second):
		t.Fatal("second execution did not complete")
	}

	if execution := store.executions["exec-manager-session-2"]; execution.Status != ExecutionStateSucceeded {
		t.Fatalf("same-session deferred execution status after release = %q, want succeeded", execution.Status)
	}
}

// Test: different session keys can run in parallel when the global limit has spare capacity.
// Validates: AC-2110 (REQ-1578 - different session keys may execute concurrently subject to the global limit)
func TestExecutionManagerAllowsConcurrentDifferentSessionKeys(t *testing.T) {
	t.Parallel()

	store := newFakeExecutionStore()
	coordinator := NewSlackTurnCoordinator(store, "runtime-1")
	coordinator.now = advancingExecutionManagerClock()
	manager := NewExecutionManager(coordinator, boundedExecutionAdmissionPolicy{globalLimit: 2})

	firstAccepted := acceptExecutionForManagerTest(t, coordinator, "exec-manager-parallel-1", "Ev-manager-parallel-1", "1713686400.000100", "slack-session-1")
	secondAccepted := acceptExecutionForManagerTest(t, coordinator, "exec-manager-parallel-2", "Ev-manager-parallel-2", "1713686400.000200", "slack-session-2")

	firstStarted := make(chan struct{}, 1)
	firstRelease := make(chan struct{})
	firstCompleted := make(chan SlackTurnExecutionResult, 1)
	firstFailed := make(chan error, 1)
	firstResult, err := manager.ScheduleAccepted(context.Background(), ManagedExecution{
		Accepted: firstAccepted,
		Execute: func(context.Context, PreparedSlackEvent) error {
			firstStarted <- struct{}{}
			<-firstRelease
			return nil
		},
		Callbacks: ExecutionCallbacks{
			OnCompleted: func(result SlackTurnExecutionResult) {
				firstCompleted <- result
			},
			OnFailed: func(err error) {
				firstFailed <- err
			},
		},
	})
	if err != nil {
		t.Fatalf("first ScheduleAccepted() error = %v", err)
	}
	if !firstResult.Started {
		t.Fatalf("first ScheduleAccepted() result = %#v, want started", firstResult)
	}

	secondStarted := make(chan struct{}, 1)
	secondRelease := make(chan struct{})
	secondCompleted := make(chan SlackTurnExecutionResult, 1)
	secondFailed := make(chan error, 1)
	secondResult, err := manager.ScheduleAccepted(context.Background(), ManagedExecution{
		Accepted: secondAccepted,
		Execute: func(context.Context, PreparedSlackEvent) error {
			secondStarted <- struct{}{}
			<-secondRelease
			return nil
		},
		Callbacks: ExecutionCallbacks{
			OnCompleted: func(result SlackTurnExecutionResult) {
				secondCompleted <- result
			},
			OnFailed: func(err error) {
				secondFailed <- err
			},
		},
	})
	if err != nil {
		t.Fatalf("second ScheduleAccepted() error = %v", err)
	}
	if !secondResult.Started {
		t.Fatalf("second ScheduleAccepted() result = %#v, want started", secondResult)
	}

	waitForExecutionSignal(t, firstStarted, firstFailed, "first concurrent execution start")
	waitForExecutionSignal(t, secondStarted, secondFailed, "second concurrent execution start")

	if execution := store.executions["exec-manager-parallel-1"]; execution.Status != ExecutionStateRunning {
		t.Fatalf("first concurrent execution status = %q, want running", execution.Status)
	}
	if execution := store.executions["exec-manager-parallel-2"]; execution.Status != ExecutionStateRunning {
		t.Fatalf("second concurrent execution status = %q, want running", execution.Status)
	}

	close(firstRelease)
	close(secondRelease)

	select {
	case <-firstCompleted:
	case err := <-firstFailed:
		t.Fatalf("first concurrent execution failed = %v", err)
	case <-time.After(time.Second):
		t.Fatal("first concurrent execution did not complete")
	}

	select {
	case <-secondCompleted:
	case err := <-secondFailed:
		t.Fatalf("second concurrent execution failed = %v", err)
	case <-time.After(time.Second):
		t.Fatal("second concurrent execution did not complete")
	}
}

// Test: startup failures before the execution reaches running are persisted as terminal failed outcomes by the ExecutionManager path.
// Validates: AC-2114 (REQ-1584 - startup failures persist failed state with diagnostic context before running)
func TestExecutionManagerPersistsStartupFailureBeforeRunning(t *testing.T) {
	t.Parallel()

	store := newFakeExecutionStore()
	coordinator := NewSlackTurnCoordinator(store, "runtime-1")
	coordinator.now = advancingExecutionManagerClock()
	manager := NewExecutionManager(coordinator, nil)

	accepted := acceptExecutionForManagerTest(t, coordinator, "exec-manager-startup-failed", "Ev-manager-startup-failed", "1713686400.000100", "slack-1713686400.000100")
	store.saveStateErr = errors.New("persist processing thread state failed")

	executed := make(chan struct{}, 1)
	completed := make(chan SlackTurnExecutionResult, 1)
	failed := make(chan error, 1)
	result, err := manager.ScheduleAccepted(context.Background(), ManagedExecution{
		Accepted: accepted,
		Execute: func(context.Context, PreparedSlackEvent) error {
			executed <- struct{}{}
			return nil
		},
		Callbacks: ExecutionCallbacks{
			OnCompleted: func(result SlackTurnExecutionResult) {
				completed <- result
			},
			OnFailed: func(err error) {
				failed <- err
			},
		},
	})
	if err != nil {
		t.Fatalf("ScheduleAccepted() error = %v", err)
	}
	if !result.Started {
		t.Fatalf("ScheduleAccepted() result = %#v, want started", result)
	}

	select {
	case err := <-failed:
		if !strings.Contains(err.Error(), "persist thread state") {
			t.Fatalf("OnFailed() error = %v, want startup persistence failure", err)
		}
	case <-completed:
		t.Fatal("OnCompleted() callback was called for startup failure")
	case <-time.After(time.Second):
		t.Fatal("OnFailed() callback was not called for startup failure")
	}

	select {
	case <-executed:
		t.Fatal("execution callback ran even though startup failed before running")
	default:
	}

	execution := store.executions["exec-manager-startup-failed"]
	if execution.Status != ExecutionStateFailed {
		t.Fatalf("execution status after startup failure = %q, want failed", execution.Status)
	}
	if execution.StartedAt != nil {
		t.Fatalf("execution started_at after startup failure = %v, want nil", execution.StartedAt)
	}
	if execution.CompletedAt == nil {
		t.Fatalf("execution completed_at after startup failure = nil, want terminal timestamp")
	}
	if !strings.Contains(execution.DiagnosticContext, "persist thread state") {
		t.Fatalf("execution diagnostic context = %q, want startup failure details", execution.DiagnosticContext)
	}
}

func acceptExecutionForManagerTest(t *testing.T, coordinator *SlackTurnCoordinator, executionID, deliveryID, threadTS, sessionKey string) AcceptedSlackExecution {
	t.Helper()

	coordinator.SetExecutionIDGenerator(func() string { return executionID })
	accepted, _, err := coordinator.Accept(context.Background(), PreparedSlackEvent{
		SourceType: "message",
		DeliveryID: deliveryID,
		Event: slack.Event{
			ID:        deliveryID,
			ChannelID: "C12345678",
			Text:      "summarize current state",
		},
		Project: registry.Project{
			Name: "alpha",
		},
		ThreadTS:    threadTS,
		SessionName: sessionKey,
		ThreadState: ThreadState{
			ThreadTS:    threadTS,
			ChannelID:   "C12345678",
			ProjectName: "alpha",
			SessionName: sessionKey,
		},
	})
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}

	return accepted
}

func advancingExecutionManagerClock() func() time.Time {
	now := time.Unix(1713686400, 0).UTC()
	return func() time.Time {
		now = now.Add(time.Second)
		return now
	}
}

func waitForExecutionSignal(t *testing.T, started <-chan struct{}, failed <-chan error, label string) {
	t.Helper()

	select {
	case <-started:
	case err := <-failed:
		t.Fatalf("%s failed = %v", label, err)
	case <-time.After(time.Second):
		t.Fatalf("%s timed out", label)
	}
}

// Test: slash deliveries acknowledged before queue creation can still be accepted into the persisted execution queue.
// Validates: AC-2111 (REQ-1581 - successful slash acknowledgement leads to execution request handoff)
func TestSlackTurnCoordinatorAcceptUsesAckedSlashClaim(t *testing.T) {
	t.Parallel()

	store := newFakeExecutionStore()
	store.dedupe["slash:Ev-acked"] = EventDedupe{
		SourceType: "slash",
		DeliveryID: "Ev-acked",
		ReceivedAt: time.Unix(1713686400, 0).UTC(),
		Status:     "acked",
	}

	coordinator := NewSlackTurnCoordinator(store, "runtime-1")
	coordinator.SetExecutionIDGenerator(func() string { return "exec-acked-slash" })
	coordinator.now = func() time.Time { return time.Unix(1713686401, 0).UTC() }

	accepted, result, err := coordinator.Accept(context.Background(), PreparedSlackEvent{
		SourceType: "slash",
		DeliveryID: "Ev-acked",
		Event: slack.Event{
			ID:        "Ev-acked",
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
	if !result.Executed || result.Duplicate {
		t.Fatalf("Accept() result = %#v, want accepted and not duplicate", result)
	}
	if accepted.Execution.ExecutionID != "exec-acked-slash" {
		t.Fatalf("Accept() execution id = %q, want exec-acked-slash", accepted.Execution.ExecutionID)
	}
	if got := store.dedupe["slash:Ev-acked"].Status; got != "queued" {
		t.Fatalf("dedupe status after Accept() = %q, want queued", got)
	}
	if got := store.executions["exec-acked-slash"].Status; got != ExecutionStateQueued {
		t.Fatalf("persisted execution status = %q, want queued", got)
	}
}
