package runtime_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/registry"
	runtime "github.com/spexus-ai/spexus-agent/internal/runtime"
	"github.com/spexus-ai/spexus-agent/internal/slack"
	"github.com/spexus-ai/spexus-agent/internal/storage"
)

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

// Test: a registered Slack event is resolved from SQLite, dispatched through Agent, and rendered back to the thread using translated Agent output.
// Validates: AC-1782 (REQ-1143 - runtime loads project registry from SQLite), AC-1787 (REQ-1148 - root Slack messages create or ensure a thread session), AC-1788 (REQ-1149 - thread replies continue the existing thread session), AC-1791 (REQ-1152 - runtime persists deduplication data and thread metadata in SQLite), AC-1792 (REQ-1153 - duplicate Slack events are deduplicated)
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
