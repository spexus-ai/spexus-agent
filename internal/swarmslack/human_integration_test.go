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

// Test: a missed stop from paged Slack history wins over a later historical
// continue. Socket redelivery of that source cannot apply stop twice.
// Validates: AC-432/465 (REQ-350/392 - reconnect catchup and stop priority).
func TestHumanCatchupMissedStopAndDuplicateSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner := swarm.NewID()
	profile := swarm.TextProfile{ID: "owner", Model: "fixture/test", Reasoning: "minimal", Prompt: "Return JSON", Tools: []string{}, Extensions: []string{}}
	profileBytes, _ := json.Marshal(profile)
	cfg := swarm.Config{TenantID: swarm.NewID(), ProjectID: swarm.NewID(), WireVersion: 2, Human: &swarm.HumanConfig{BaseURL: "https://example.invalid", TokenFile: filepath.Join(t.TempDir(), "token.json"), EpicID: swarm.NewID(), WriterID: swarm.NewID(), WorkspaceID: "W"}, Agents: []swarm.AgentConfig{{AgentID: owner, Role: "owner", CredentialSHA256: swarm.Digest([]byte("secret")), ProfileID: "owner"}}, Profiles: []swarm.ProfileSnapshot{{Bytes: profileBytes}}}
	for i, id := range []string{swarm.NewID(), swarm.NewID()} {
		cfg.Agents = append(cfg.Agents, swarm.AgentConfig{AgentID: id, Role: "worker", CredentialSHA256: swarm.Digest([]byte{byte(i + 1)}), ProfileID: "owner"})
	}
	f := swarm.Feature{FeatureID: swarm.NewID(), TenantID: cfg.TenantID, ProjectID: cfg.ProjectID, OwnerAgentID: owner, ChannelID: "C", ThreadTS: "1.000001", AllowedActorIDs: []string{"U"}}
	cfg.Features = []swarm.Feature{f}
	store, err := swarm.Open(ctx, filepath.Join(t.TempDir(), "swarm.db"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/conversations.replies" {
			pages++
			if r.URL.Query().Get("cursor") == "" {
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "has_more": true, "messages": []any{map[string]string{"ts": "9999999999.000001", "thread_ts": f.ThreadTS, "user": "U", "text": "!stop"}}, "response_metadata": map[string]string{"next_cursor": "page-2"}})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": []any{map[string]string{"ts": "9999999999.000002", "thread_ts": f.ThreadTS, "user": "U", "text": "!continue"}}})
			}
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "9999999999.000003"})
	}))
	defer server.Close()
	api := NewAPI("token")
	api.BaseURL, api.Client = server.URL+"/", server.Client()
	bridge := &Bridge{Store: store, Features: cfg.Features, API: api}
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
	stops := 0
	for _, a := range history.Audit {
		if a.Event == "feature_stopped" {
			stops++
		}
	}
	if stops != 1 {
		t.Fatalf("stop applied %d times", stops)
	}
	if err = source.Send(ctx, slack.Event{ID: "socket-copy", ChannelID: "C", ThreadTS: f.ThreadTS, Timestamp: "9999999999.000001", UserID: "U", Text: "!stop"}); err != nil {
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
