package swarmslack

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/slack"
	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

func TestLegacyDurableRecoversMissedMessageWithoutDuplicate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner := "orchestrator"
	cfg := swarm.Config{TenantID: swarm.NewID(), ProjectID: swarm.NewID(), WireVersion: 1, Agents: []swarm.AgentConfig{{AgentID: owner, Role: "owner", CredentialSHA256: swarm.Digest([]byte("owner")), ProfileID: owner}}}
	for _, id := range []string{"worker-a", "worker-b"} {
		cfg.Agents = append(cfg.Agents, swarm.AgentConfig{AgentID: id, Role: "worker", CredentialSHA256: swarm.Digest([]byte(id)), ProfileID: id})
	}
	profileServiceFixture(t, &cfg, "fixture/test")
	f := swarm.Feature{FeatureID: swarm.NewID(), TenantID: cfg.TenantID, ProjectID: cfg.ProjectID, OwnerAgentID: owner, ChannelID: "C", ThreadTS: "1.000001", AllowedActorIDs: []string{"U"}}
	cfg.Features = []swarm.Feature{f}
	path := filepath.Join(t.TempDir(), "swarm.db")
	store, err := swarm.Open(ctx, path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.SlackWatermark(ctx, f.FeatureID); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/conversations.replies":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": []any{map[string]string{"ts": "9999999999.000001", "thread_ts": f.ThreadTS, "user": "U", "text": "ping"}}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer server.Close()
	api := NewAPI("token")
	api.BaseURL, api.Client = server.URL+"/", server.Client()
	bridge := &Bridge{Store: store, Features: cfg.Features, API: api}
	source := &durableFixture{}
	done := make(chan error, 1)
	go func() { done <- bridge.RunLegacyDurable(ctx, source) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		watermark, err := store.SlackWatermark(ctx, f.FeatureID)
		if err == nil && watermark == "9999999999.000001" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("history cursor was not advanced: %s, %v", watermark, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := source.Send(ctx, slack.Event{ID: "Ev-redelivery", ChannelID: f.ChannelID, ThreadTS: f.ThreadTS, Timestamp: "9999999999.000001", UserID: "U", Text: "ping"}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM ingress WHERE feature_id=?", f.FeatureID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("ingress count=%d, err=%v; want one recovered message", count, err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("durable Slack bridge did not shut down")
	}
}
