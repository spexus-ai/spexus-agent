package swarmslack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/spexus-ai/spexus-agent/internal/slack"
	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

func TestFeatureAnchorStopAndContinueSync(t *testing.T) {
	feature := swarm.Feature{FeatureID: swarm.NewID(), ChannelID: "C", ThreadTS: "1.000001"}
	marker := "P3-HR anchor fixture"
	value, _ := json.Marshal(map[string]string{"feature_id": feature.FeatureID})
	blocks := []map[string]any{
		{"type": "section", "block_id": marker, "text": map[string]string{"type": "mrkdwn", "text": "Test"}},
		{"type": "actions", "elements": []map[string]string{{"type": "button", "action_id": slack.FeatureControlActionID + ":stop", "value": string(value)}}},
	}
	var mu sync.Mutex
	updates := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test" {
			t.Error("wrong Slack token")
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/conversations.replies":
			if r.URL.Query().Get("channel") != feature.ChannelID || r.URL.Query().Get("ts") != feature.ThreadTS {
				t.Error("wrong feature anchor scope")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": []any{map[string]any{"ts": feature.ThreadTS, "blocks": blocks}}})
		case "/chat.update":
			var body struct {
				Channel string           `json:"channel"`
				TS      string           `json:"ts"`
				Blocks  []map[string]any `json:"blocks"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Channel != feature.ChannelID || body.TS != feature.ThreadTS {
				t.Error("wrong anchor update")
			}
			blocks = body.Blocks
			updates++
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			t.Errorf("unexpected Slack API %s", r.URL.Path)
		}
	}))
	defer server.Close()
	a := NewAPI("test")
	a.BaseURL, a.Client = server.URL+"/", server.Client()
	for _, stopped := range []bool{true, true, false, false} {
		if err := a.SyncFeatureAnchor(context.Background(), feature, stopped); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if updates != 2 || len(blocks) != 2 || blocks[0]["block_id"] != marker {
		t.Fatalf("anchor updates=%d blocks=%+v", updates, blocks)
	}
	elements, ok := blocks[1]["elements"].([]any)
	if !ok || len(elements) != 1 {
		t.Fatalf("stop button not restored: %+v", blocks)
	}
	button, ok := elements[0].(map[string]any)
	if !ok || button["action_id"] != slack.FeatureControlActionID+":stop" {
		t.Fatalf("stop button not restored: %+v", blocks)
	}
}
