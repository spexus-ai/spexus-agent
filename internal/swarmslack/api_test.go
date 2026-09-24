package swarmslack

import (
	"context"
	"encoding/json"
	"github.com/spexus-ai/spexus-agent/internal/swarm"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCorrelatedSlackPublishAndPagedLookup(t *testing.T) {
	d := swarm.SlackDelivery{ID: swarm.NewID(), TurnID: swarm.NewID(), ChannelID: "C", ThreadTS: "1.2", Text: "result"}
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test" {
			t.Error("missing auth")
		}
		if r.URL.Path == "/chat.postMessage" {
			var p struct {
				ID       string `json:"client_msg_id"`
				Thread   string `json:"thread_ts"`
				Metadata struct {
					Payload map[string]string `json:"event_payload"`
				} `json:"metadata"`
			}
			json.NewDecoder(r.Body).Decode(&p)
			if p.ID != d.ID || p.Thread != d.ThreadTS || p.Metadata.Payload["delivery_id"] != d.ID {
				t.Error("missing durable correlation")
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1.3"})
			return
		}
		pages++
		if r.URL.Query().Get("channel") != d.ChannelID || r.URL.Query().Get("ts") != d.ThreadTS {
			t.Error("wrong thread")
		}
		if pages == 1 {
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "has_more": true, "messages": []any{}, "response_metadata": map[string]string{"next_cursor": "next"}})
			return
		}
		if r.URL.Query().Get("cursor") != "next" {
			t.Error("missing pagination")
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": []any{map[string]any{"ts": "1.3", "metadata": map[string]any{"event_type": "spexus_swarm_reply", "event_payload": map[string]string{"delivery_id": d.ID}}}}})
	}))
	defer server.Close()
	a := NewAPI("test")
	a.BaseURL = server.URL + "/"
	a.Client = server.Client()
	if ts, err := a.Post(context.Background(), d); err != nil || ts != "1.3" {
		t.Fatalf("post %s %v", ts, err)
	}
	if ts, found, err := a.Find(context.Background(), d); err != nil || !found || ts != "1.3" || pages != 2 {
		t.Fatalf("lookup %s %t %v pages%d", ts, found, err, pages)
	}
}
func TestIncompleteSlackHistoryNeverProvesAbsent(t *testing.T) {
	for _, body := range []string{`{"ok":false,"error":"missing_scope"}`, `{"ok":true,"has_more":true,"messages":[]}`, `not json`} {
		t.Run(body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
			defer s.Close()
			a := NewAPI("test")
			a.BaseURL = s.URL + "/"
			a.Client = s.Client()
			_, found, err := a.Find(context.Background(), swarm.SlackDelivery{ID: swarm.NewID()})
			if err == nil || found {
				t.Fatal("unavailable/incomplete history incorrectly proved absence")
			}
		})
	}
}
