package swarmslack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/slack"
	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

type durableFixture struct {
	mu     sync.Mutex
	handle func(context.Context, slack.Event) error
}

func (f *durableFixture) RunDurable(ctx context.Context, connected, disconnected func(context.Context) error, handle func(context.Context, slack.Event) error) error {
	f.mu.Lock()
	f.handle = handle
	f.mu.Unlock()
	if err := connected(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}
func (f *durableFixture) Close() error { return nil }
func (f *durableFixture) Send(ctx context.Context, event slack.Event) error {
	f.mu.Lock()
	handle := f.handle
	f.mu.Unlock()
	return handle(ctx, event)
}

func newHumanTransportStore(t *testing.T) (*swarm.Store, swarm.Feature) {
	t.Helper()
	owner := "orchestrator"
	cfg := swarm.Config{TenantID: swarm.NewID(), ProjectID: swarm.NewID(), WireVersion: 2, Human: &swarm.HumanConfig{BaseURL: "https://example.invalid", TokenFile: filepath.Join(t.TempDir(), "token.json"), EpicID: swarm.NewID(), WriterID: swarm.NewID(), WorkspaceID: "W"}, Agents: []swarm.AgentConfig{{AgentID: owner, Role: "owner", CredentialSHA256: swarm.Digest([]byte("secret")), ProfileID: owner}}}
	for i, id := range []string{"worker-a", "worker-b"} {
		cfg.Agents = append(cfg.Agents, swarm.AgentConfig{AgentID: id, Role: "worker", CredentialSHA256: swarm.Digest([]byte{byte(i + 1)}), ProfileID: id})
	}
	profileServiceFixture(t, &cfg, "fixture/test")
	f := swarm.Feature{FeatureID: swarm.NewID(), TenantID: cfg.TenantID, ProjectID: cfg.ProjectID, OwnerAgentID: owner, ChannelID: "C", ThreadTS: "1.000001", AllowedActorIDs: []string{"U"}}
	cfg.Features = []swarm.Feature{f}
	store, err := swarm.Open(context.Background(), filepath.Join(t.TempDir(), "swarm.db"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, f
}

// Test: a missed stop from paged Slack history wins over a later historical
// continue. Socket redelivery of that source cannot apply stop twice.
// Validates: AC-432/465 (REQ-350/392 - reconnect catchup and stop priority).
func TestHumanCatchupMissedStopAndDuplicateSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, f := newHumanTransportStore(t)
	var err error
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth.test" {
			if r.Method != http.MethodPost {
				t.Errorf("auth.test method=%s, want POST", r.Method)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "team_id": "W"})
			return
		}
		if r.URL.Path == "/conversations.replies" {
			pages++
			if r.URL.Query().Get("cursor") == "" {
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "has_more": true, "messages": []any{map[string]string{"ts": "9999999999.000001", "thread_ts": f.ThreadTS, "user": "U", "text": "!stop"}}, "response_metadata": map[string]string{"next_cursor": "page-2"}})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": []any{map[string]string{"ts": "9999999999.000002", "thread_ts": f.ThreadTS, "user": "U", "text": "!continue"}, map[string]any{"ts": "9999999999.000003", "thread_ts": f.ThreadTS, "user": "U", "text": "!answer 123e4567-e89b-42d3-a456-426614174000 text changed", "edited": map[string]string{"user": "U", "ts": "9999999999.000004"}}}})
			}
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "9999999999.000003"})
	}))
	defer server.Close()
	api := NewAPI("token")
	api.BaseURL, api.Client = server.URL+"/", server.Client()
	bridge := &Bridge{Store: store, Features: []swarm.Feature{f}, API: api}
	source := &durableFixture{}
	done := make(chan error, 1)
	go func() { done <- bridge.RunHuman(ctx, source, "W") }()
	var history swarm.History
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		history, err = store.History(ctx, f.FeatureID)
		if err == nil && history.Feature.Stopped && history.RecoveryBarrier == "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || !history.Feature.Stopped || history.RecoveryBarrier != "" {
		t.Fatalf("catchup status=%+v err=%v", history.Feature, err)
	}
	if pages != 2 {
		t.Fatalf("history pages=%d", pages)
	}
	if found, err := store.SlackSourceExists(ctx, "W", "C", "9999999999.000003"); err != nil || found {
		t.Fatalf("edited unseen answer became source: found=%t err=%v", found, err)
	}
	stops := 0
	for _, a := range history.Audit {
		if a.Event == "feature_stopped" {
			stops++
		}
	}
	if stops != 1 {
		t.Fatalf("stop applied %d times", stops)
	}
	if err = source.Send(ctx, slack.Event{ID: "socket-copy", WorkspaceID: "W", ChannelID: "C", ThreadTS: f.ThreadTS, Timestamp: "9999999999.000001", UserID: "U", Text: "!stop"}); err != nil {
		t.Fatal(err)
	}
	history, err = store.History(ctx, f.FeatureID)
	if err != nil {
		t.Fatal(err)
	}
	newStops := 0
	for _, a := range history.Audit {
		if a.Event == "feature_stopped" {
			newStops++
		}
	}
	if newStops != stops || !history.Feature.Stopped {
		t.Fatalf("duplicate stop changed state: %d to %d", stops, newStops)
	}
	if err := source.Send(ctx, slack.Event{ID: "foreign-workspace", WorkspaceID: "OTHER", ChannelID: "C", ThreadTS: f.ThreadTS, Timestamp: "9999999999.000005", UserID: "U", Text: "!answer 123e4567-e89b-42d3-a456-426614174000 text approved"}); err == nil {
		t.Fatal("foreign workspace was accepted")
	}
	if found, err := store.SlackSourceExists(ctx, "W", "C", "9999999999.000005"); err != nil || found {
		t.Fatalf("foreign workspace committed a source: found=%t err=%v", found, err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bridge did not stop")
	}
}

type stoppedCommitStore struct {
	*swarm.Store
	committed chan struct{}
	release   chan struct{}
}

func (s *stoppedCommitStore) CommitSlackSource(ctx context.Context, source swarm.SlackSource) (bool, error) {
	duplicate, err := s.Store.CommitSlackSource(ctx, source)
	if err == nil && source.Text == "!stop" {
		close(s.committed)
		<-s.release
	}
	return duplicate, err
}

type emptyHistory struct{}

func (emptyHistory) VerifyWorkspace(context.Context, string) error { return nil }
func (emptyHistory) ScanThread(context.Context, string, string, string, func(string, string, string, string, bool) error) (string, error) {
	return "", nil
}

// Test: a Socket !stop already committed to SQLite cannot wait behind the
// final history drain while catchup opens the execution barrier.
// Validates: AC-432/465 (REQ-350/392 - stop-before-resume race).
func TestHumanCatchupDoesNotOpenOverCommittedStop(t *testing.T) {
	ctx := context.Background()
	store, f := newHumanTransportStore(t)
	if _, err := store.SlackWatermark(ctx, f.FeatureID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRecoveryBarrier(ctx, f.FeatureID, "slack_catchup"); err != nil {
		t.Fatal(err)
	}
	wrapped := &stoppedCommitStore{Store: store, committed: make(chan struct{}), release: make(chan struct{})}
	h := &humanIngress{bridge: &Bridge{Features: []swarm.Feature{f}}, store: wrapped, history: emptyHistory{}, workspace: "W"}
	stopDone := make(chan error, 1)
	go func() {
		_, err := h.commit(ctx, f, slack.Event{ID: "stop-socket", WorkspaceID: "W", ChannelID: "C", ThreadTS: f.ThreadTS, Timestamp: "9999999999.000010", UserID: "U", Text: "!stop"})
		stopDone <- err
	}()
	select {
	case <-wrapped.committed:
	case <-time.After(2 * time.Second):
		t.Fatal("stop not committed")
	}
	catchupDone := make(chan error, 1)
	go func() { catchupDone <- h.scanAll(ctx) }()
	select {
	case err := <-catchupDone:
		t.Fatalf("catchup opened over pending stop: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	view, err := store.History(ctx, f.FeatureID)
	if err != nil || view.RecoveryBarrier == "" {
		t.Fatalf("barrier opened before stop latch: %+v %v", view, err)
	}
	close(wrapped.release)
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not settle")
	}
	select {
	case err := <-catchupDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("catchup did not settle")
	}
	view, err = store.History(ctx, f.FeatureID)
	if err != nil || !view.Feature.Stopped || view.RecoveryBarrier != "" {
		t.Fatalf("catchup settled without stop latch: %+v %v", view, err)
	}
}
