package swarmslack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spexus-ai/spexus-agent/internal/slack"
	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

// Test: the original human question has a readable fallback and two distinct
// buttons; terminal chat.update removes the buttons from that same message.
// Validates: AC-464/465 (REQ-391/392 - usable single decision and no stale UI).
func TestHumanQuestionButtonsAndTerminalUpdate(t *testing.T) {
	var posts []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		payload["method"] = r.URL.Path
		posts = append(posts, payload)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "9.000001"})
	}))
	defer server.Close()
	a := NewAPI("test")
	a.BaseURL, a.Client = server.URL+"/", server.Client()
	requestID := "123e4567-e89b-42d3-a456-426614174000"
	options := []swarm.HumanOption{{ID: "short", Label: "Коротко"}, {ID: "detailed", Label: "Подробно"}}
	d := swarm.SlackDelivery{ID: requestID, ChannelID: "C", ThreadTS: "1.000001", Text: "Запрос: " + requestID + "\nВопрос: Какой вариант?\nОтвет: !answer " + requestID + " <option-id>", Question: &swarm.HumanQuestion{Options: options}}
	if _, err := a.Post(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	d.Status, d.SlackTS = "sent", "9.000001"
	if err := a.UpdateHumanQuestion(context.Background(), d, options, "open", ""); err != nil {
		t.Fatal(err)
	}
	if err := a.UpdateHumanQuestion(context.Background(), d, options, "stopped", ""); err != nil {
		t.Fatal(err)
	}
	if err := a.UpdateHumanQuestion(context.Background(), d, options, "answered", "Выбран вариант: Коротко."); err != nil {
		t.Fatal(err)
	}
	if len(posts) != 4 {
		t.Fatalf("calls=%d", len(posts))
	}
	for i, post := range posts {
		if i != 0 && (post["method"] != "/chat.update" || post["ts"] != d.SlackTS) {
			t.Fatalf("update did not target original question: %#v", post)
		}
		blocks := post["blocks"].([]any)
		buttons := 0
		seen := map[string]bool{}
		for _, raw := range blocks {
			block := raw.(map[string]any)
			if block["type"] != "actions" {
				continue
			}
			for _, element := range block["elements"].([]any) {
				button := element.(map[string]any)
				if button["type"] != "button" {
					t.Fatalf("not button: %#v", button)
				}
				id := button["action_id"].(string)
				if !strings.HasPrefix(id, slack.HumanAnswerActionID+":") || seen[id] {
					t.Fatalf("invalid or duplicate action ID %q", id)
				}
				seen[id] = true
				buttons++
			}
		}
		want := 2
		if i >= 2 {
			want = 0
		}
		if buttons != want {
			t.Fatalf("call %d buttons=%d want=%d", i, buttons, want)
		}
		if i > 0 && strings.Contains(post["text"].(string), "Ответ: !answer") {
			t.Fatal("legacy command still displayed")
		}
		if i == 2 && (strings.Contains(post["text"].(string), "Решение записано") || !strings.Contains(post["text"].(string), "Работа остановлена")) {
			t.Fatal("stop falsely claimed a decision")
		}
		if i == 3 && !strings.Contains(post["text"].(string), "Выбран вариант: Коротко.") {
			t.Fatal("canonical result not shown")
		}
	}
}

// Test: the visible choice is derived only from the canonical provider view,
// never from the clicked button value before Spexus records its terminal.
// Validates: AC-464 (REQ-391 - authoritative decision readback).
func TestCanonicalDecisionSummary(t *testing.T) {
	options := []swarm.HumanOption{{ID: "short", Label: "Коротко"}}
	p := swarm.HumanProjection{BackendState: "answered", View: json.RawMessage(`{"terminal":{"response":{"kind":"answer","option_id":"short","text":""}}}`)}
	if got := canonicalDecisionSummary(p, options); got != "Выбран вариант: Коротко." {
		t.Fatalf("summary=%q", got)
	}
	p.BackendState = "open"
	if got := canonicalDecisionSummary(p, options); got != "" {
		t.Fatalf("open request summary=%q", got)
	}
	p.BackendState = "answered"
	p.View = json.RawMessage(`{"terminal":{"response":{"option_id":"forged"}}}`)
	if got := canonicalDecisionSummary(p, options); got != "" {
		t.Fatalf("unknown option summary=%q", got)
	}
}
