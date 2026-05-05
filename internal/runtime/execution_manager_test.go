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

type recordingExecutionLifecycleRecorder struct {
	mu      sync.Mutex
	queued  []string
	running []string
	failed  []string
	runErr  error
}

func (r *recordingExecutionLifecycleRecorder) RecordQueued(_ context.Context, request ExecutionRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queued = append(r.queued, request.ExecutionID())
	return nil
}

func (r *recordingExecutionLifecycleRecorder) RecordRunning(_ context.Context, request ExecutionRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.running = append(r.running, request.ExecutionID())
	return r.runErr
}

func (r *recordingExecutionLifecycleRecorder) RecordFailed(_ context.Context, request ExecutionRequest, err error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed = append(r.failed, request.ExecutionID()+":"+err.Error())
	return nil
}

// Test: different session keys can run in parallel, but the execution manager does not start more workers than the configured global limit.
// Validates: AC-1976 (REQ-1420 - different session keys run in parallel), AC-1976 (REQ-1422 - global concurrency limits worker starts)
func TestExecutionManagerAppliesGlobalConcurrencyAcrossSessions(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan string, 3)
	releaseBySession := map[string]chan struct{}{
		"slack-1": make(chan struct{}),
		"slack-2": make(chan struct{}),
		"slack-3": make(chan struct{}),
	}

	manager := NewExecutionManager(ctx, ExecutionManagerConfig{
		GlobalConcurrency: 2,
	}, func(_ context.Context, request ExecutionRequest) {
		started <- request.SessionKey()
		<-releaseBySession[request.SessionKey()]
	})

	for _, request := range []ExecutionRequest{
		newExecutionManagerRequest("Ev-1", "1713686400.000100", "slack-1"),
		newExecutionManagerRequest("Ev-2", "1713686400.000200", "slack-2"),
		newExecutionManagerRequest("Ev-3", "1713686400.000300", "slack-3"),
	} {
		if err := manager.Enqueue(ctx, request); err != nil {
			t.Fatalf("Enqueue(%s) error = %v", request.DeliveryID, err)
		}
	}

	first := waitForExecutionSession(t, started)
	second := waitForExecutionSession(t, started)
	if first == second {
		t.Fatalf("started sessions = %q, %q, want two distinct sessions", first, second)
	}

	select {
	case session := <-started:
		t.Fatalf("session %s started before a global worker slot was released", session)
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseBySession[first])

	third := waitForExecutionSession(t, started)
	if third != "slack-3" {
		t.Fatalf("third started session = %q, want slack-3 after a worker slot freed", third)
	}

	close(releaseBySession[second])
	close(releaseBySession[third])
	cancel()
	manager.Wait()
}

// Test: the same session key is serialized while another session can continue in parallel without project-wide locking.
// Validates: AC-1977 (REQ-1421 - the same session key is serialized), AC-1976 (REQ-1420 - a different session key can run concurrently), AC-1977 (REQ-1423 - runtime does not serialize the whole project)
func TestExecutionManagerSerializesOneSessionWithoutBlockingOthers(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan string, 3)
	releaseByDelivery := map[string]chan struct{}{
		"Ev-1": make(chan struct{}),
		"Ev-2": make(chan struct{}),
		"Ev-3": make(chan struct{}),
	}

	var mu sync.Mutex
	var order []string

	manager := NewExecutionManager(ctx, ExecutionManagerConfig{
		GlobalConcurrency: 2,
	}, func(_ context.Context, request ExecutionRequest) {
		started <- request.DeliveryID

		mu.Lock()
		order = append(order, request.DeliveryID+":start")
		mu.Unlock()

		<-releaseByDelivery[request.DeliveryID]

		mu.Lock()
		order = append(order, request.DeliveryID+":end")
		mu.Unlock()
	})

	for _, request := range []ExecutionRequest{
		newExecutionManagerRequest("Ev-1", "1713686400.000100", "slack-shared"),
		newExecutionManagerRequest("Ev-2", "1713686400.000100", "slack-shared"),
		newExecutionManagerRequest("Ev-3", "1713686400.000200", "slack-other"),
	} {
		if err := manager.Enqueue(ctx, request); err != nil {
			t.Fatalf("Enqueue(%s) error = %v", request.DeliveryID, err)
		}
	}

	seen := map[string]bool{
		waitForExecutionSession(t, started): true,
		waitForExecutionSession(t, started): true,
	}
	if !seen["Ev-1"] || !seen["Ev-3"] {
		t.Fatalf("started deliveries = %#v, want Ev-1 and Ev-3 before Ev-2", seen)
	}

	select {
	case deliveryID := <-started:
		t.Fatalf("delivery %s started before the prior shared-session execution completed", deliveryID)
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseByDelivery["Ev-1"])

	if deliveryID := waitForExecutionSession(t, started); deliveryID != "Ev-2" {
		t.Fatalf("delivery started after shared-session release = %q, want Ev-2", deliveryID)
	}

	close(releaseByDelivery["Ev-3"])
	close(releaseByDelivery["Ev-2"])
	cancel()
	manager.Wait()

	mu.Lock()
	defer mu.Unlock()

	if len(order) < 4 {
		t.Fatalf("execution order = %#v, want at least four lifecycle entries", order)
	}

	indexByEvent := make(map[string]int, len(order))
	for index, event := range order {
		if _, exists := indexByEvent[event]; !exists {
			indexByEvent[event] = index
		}
	}

	if indexByEvent["Ev-2:start"] < indexByEvent["Ev-1:end"] {
		t.Fatalf("execution order = %#v, want Ev-2:start after Ev-1:end", order)
	}
	if indexByEvent["Ev-1:start"] > indexByEvent["Ev-2:start"] {
		t.Fatalf("execution order = %#v, want Ev-1:start before Ev-2:start", order)
	}
	if indexByEvent["Ev-3:start"] > indexByEvent["Ev-2:start"] {
		t.Fatalf("execution order = %#v, want Ev-3:start before Ev-2:start", order)
	}
}

// Test: the execution manager records queued and running lifecycle transitions before the worker handler starts.
// Validates: AC-1981 (REQ-1429 - queued/running lifecycle transitions are persisted by the execution manager)
func TestExecutionManagerRecordsQueuedAndRunningLifecycleTransitions(t *testing.T) {
	t.Parallel()

	recorder := &recordingExecutionLifecycleRecorder{}
	started := make(chan struct{}, 1)
	release := make(chan struct{})

	manager := NewExecutionManager(context.Background(), ExecutionManagerConfig{
		LifecycleRecorder: recorder,
	}, func(_ context.Context, request ExecutionRequest) {
		if request.ExecutionID() != "mention:Ev-1" {
			t.Errorf("ExecutionID() = %q, want mention:Ev-1", request.ExecutionID())
		}
		started <- struct{}{}
		<-release
	})

	request := newExecutionManagerRequest("Ev-1", "1713686400.000100", "slack-1")
	if err := manager.Enqueue(context.Background(), request); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}

	close(release)
	manager.Wait()

	recorder.mu.Lock()
	defer recorder.mu.Unlock()

	if len(recorder.queued) != 1 || recorder.queued[0] != request.ExecutionID() {
		t.Fatalf("queued lifecycle = %#v, want [%q]", recorder.queued, request.ExecutionID())
	}
	if len(recorder.running) != 1 || recorder.running[0] != request.ExecutionID() {
		t.Fatalf("running lifecycle = %#v, want [%q]", recorder.running, request.ExecutionID())
	}
	if len(recorder.failed) != 0 {
		t.Fatalf("failed lifecycle = %#v, want none", recorder.failed)
	}
}

// Test: if the running transition cannot be persisted, the execution manager records a failed lifecycle and does not start the worker.
// Validates: AC-1981 (REQ-1430 - lifecycle persistence captures terminal errors when startup transitions fail)
func TestExecutionManagerMarksFailedWhenRunningTransitionPersistenceFails(t *testing.T) {
	t.Parallel()

	recorder := &recordingExecutionLifecycleRecorder{runErr: errors.New("write failed")}
	started := make(chan struct{}, 1)

	manager := NewExecutionManager(context.Background(), ExecutionManagerConfig{
		LifecycleRecorder: recorder,
	}, func(context.Context, ExecutionRequest) {
		started <- struct{}{}
	})

	request := newExecutionManagerRequest("Ev-2", "1713686400.000200", "slack-2")
	if err := manager.Enqueue(context.Background(), request); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	manager.Wait()

	select {
	case <-started:
		t.Fatal("handler started despite running lifecycle persistence failure")
	default:
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()

	if len(recorder.failed) != 1 || recorder.failed[0] != request.ExecutionID()+":write failed" {
		t.Fatalf("failed lifecycle = %#v, want [%q]", recorder.failed, request.ExecutionID()+":write failed")
	}
}

func newExecutionManagerRequest(deliveryID, threadTS, sessionName string) ExecutionRequest {
	return ExecutionRequest{
		SourceType:  slack.InboundSourceMention,
		DeliveryID:  deliveryID,
		ChannelID:   "C123",
		UserID:      "U123",
		CommandText: "status",
		Project: registry.Project{
			Name:           "alpha",
			LocalPath:      "/workspace/alpha",
			SlackChannelID: "C123",
		},
		ThreadTS:    threadTS,
		SessionName: sessionName,
	}
}

func waitForExecutionSession(t *testing.T, started <-chan string) string {
	t.Helper()

	select {
	case value := <-started:
		return value
	case <-time.After(time.Second):
		t.Fatal("execution manager did not start the next worker in time")
		return ""
	}
}
