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
		if strings.Contains(post["text"].(string), requestID) {
			t.Fatalf("question displayed technical request ID: %#v", post)
		}
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
				if !strings.HasPrefix(id, slack.HumanAnswerActionID+":") && !strings.HasPrefix(id, slack.HumanControlActionID+":") || seen[id] {
					t.Fatalf("invalid or duplicate action ID %q", id)
				}
				seen[id] = true
				buttons++
			}
		}
		want := 4
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
		if i >= 2 && (strings.Contains(post["text"].(string), "Выберите вариант кнопкой") || strings.Contains(post["text"].(string), "Напишите в этом треде")) {
			t.Fatalf("closed question still invites an answer: %#v", post)
		}
	}
}

func TestFreeTextQuestionInvitesNaturalReplyWithoutUUID(t *testing.T) {
	var posted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "9.000001"})
	}))
	defer server.Close()
	a := NewAPI("test")
	a.BaseURL, a.Client = server.URL+"/", server.Client()
	id := "123e4567-e89b-42d3-a456-426614174000"
	d := swarm.SlackDelivery{ID: id, ChannelID: "C", ThreadTS: "1.000001", Text: "Решение человека требуется.\nЗапрос: " + id + "\nВопрос: Что делать?\nОтвет: !answer " + id + " text <ответ>", Question: &swarm.HumanQuestion{Options: []swarm.HumanOption{}}, ShortSelector: 2}
	if _, err := a.Post(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	text := posted["text"].(string)
	if strings.Contains(text, id) || strings.Contains(text, "!answer") || strings.Contains(text, "Вопрос #2") || strings.Contains(text, "Ответ #2:") || !strings.Contains(text, "Ответьте своими словами") {
		t.Fatalf("free-text question is not human-readable: %q", text)
	}
	for _, raw := range posted["blocks"].([]any) {
		if raw.(map[string]any)["type"] != "actions" {
			continue
		}
		elements := raw.(map[string]any)["elements"].([]any)
		if len(elements) != 2 || elements[0].(map[string]any)["action_id"] != slack.HumanControlActionID+":details" || elements[1].(map[string]any)["action_id"] != slack.HumanControlActionID+":stop" {
			t.Fatalf("free-text question controls: %#v", elements)
		}
	}
}

func TestHumanQuestionShowsCompactContextWithoutInternalIdentifiers(t *testing.T) {
	id := "123e4567-e89b-42d3-a456-426614174000"
	message := "Решение человека требуется для работы.\nЗапрос: " + id +
		"\nПричина: " + strings.Repeat("Нужны данные. ", 30) +
		"\nКонтекст: " + strings.Repeat("Исполнитель ждёт. ", 30) +
		"\nBlocked work: internal detail\nВопрос: Какой формат выбрать?\nРекомендация: Короткий текст.\nОжидает: подготовка результата (job " + id + ", attempt " + id + ")\n• short — Коротко\nВыберите вариант кнопкой ниже."
	got := humanReadableQuestionText(message, []swarm.HumanOption{{ID: "short", Label: "Коротко"}}, 0, true)
	if !strings.HasPrefix(got, "Нужен ваш ответ: Какой формат выбрать?") ||
		!strings.Contains(got, "Почему спрашиваю:") || strings.Contains(got, "Контекст:") ||
		!strings.Contains(got, "Рекомендация:") || !strings.Contains(got, "Ждёт ответа:") ||
		!strings.Contains(got, "Выберите вариант или ответьте своими словами") ||
		strings.Contains(got, id) || strings.Contains(got, "Blocked work:") ||
		strings.Contains(got, "!answer") || len([]rune(got)) > 700 {
		t.Fatalf("question not compact and actionable: %q", got)
	}
	closed := humanReadableQuestionText(message, []swarm.HumanOption{{ID: "short", Label: "Коротко"}}, 0, false)
	if strings.Contains(closed, "Выберите кнопку") || strings.Contains(closed, "Ответьте своими словами") {
		t.Fatalf("closed question still invites an answer: %q", closed)
	}
}

func TestHumanQuestionPendingAndAttentionAreInPlaceWithoutFalseSuccess(t *testing.T) {
	var posts []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		posts = append(posts, payload)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer server.Close()
	a := NewAPI("test")
	a.BaseURL, a.Client = server.URL+"/", server.Client()
	d := swarm.SlackDelivery{ID: swarm.NewID(), ChannelID: "C", ThreadTS: "1.000001", SlackTS: "9.000001", Status: "sent", ShortSelector: 2, Text: "Вопрос: Как поступить?\nОтвет: !answer 00000000-0000-0000-0000-000000000000 text <ответ>", Question: &swarm.HumanQuestion{}}
	for _, state := range []string{"pending", "attention"} {
		if err := a.UpdateHumanQuestion(context.Background(), d, nil, state, ""); err != nil {
			t.Fatal(err)
		}
	}
	if len(posts) != 2 {
		t.Fatalf("updates=%d", len(posts))
	}
	for _, post := range posts {
		if post["ts"] != d.SlackTS || strings.Contains(post["text"].(string), "Решение записано") || strings.Contains(post["text"].(string), "Напишите в этом треде") {
			t.Fatalf("false terminal or wrong message: %#v", post)
		}
	}
	if !strings.Contains(posts[0]["text"].(string), "решение пока не подтверждено") || !strings.Contains(posts[1]["text"].(string), "не удалось подтвердить") {
		t.Fatalf("pending/error stages unclear: %#v", posts)
	}
}

// Test: the visible choice is derived only from the canonical provider view,
// never from the clicked button value before Spexus records its terminal.
// Validates: AC-464 (REQ-391 - authoritative decision readback).
func TestCanonicalDecisionSummary(t *testing.T) {
	options := []swarm.HumanOption{{ID: "short", Label: "Коротко"}}
	p := swarm.HumanProjection{BackendState: "answered", View: json.RawMessage(`{"terminal":{"kind":"answer","response":{"kind":"answer","option_id":"short","text":""}}}`)}
	if got := canonicalDecisionSummary(p, options); got != "Выбран вариант: Коротко." {
		t.Fatalf("summary=%q", got)
	}
	p.BackendState = "open"
	if got := canonicalDecisionSummary(p, options); got != "" {
		t.Fatalf("open request summary=%q", got)
	}
	p.BackendState = "answered"
	p.View = json.RawMessage(`{"terminal":{"kind":"answer","response":{"option_id":"forged"}}}`)
	if got := canonicalDecisionSummary(p, options); got != "" {
		t.Fatalf("unknown option summary=%q", got)
	}
	p.View = json.RawMessage(`{"terminal":{"kind":"answer","response":{"option_id":null,"text":"Подробный ответ"}}}`)
	if got := canonicalDecisionSummary(p, nil); got != "Ответ свободным текстом записан." {
		t.Fatalf("free-text summary=%q", got)
	}
	p.BackendState = "denied"
	p.View = json.RawMessage(`{"terminal":{"kind":"deny","response":{"option_id":null,"text":"Нет полномочий"}}}`)
	if got := canonicalDecisionSummary(p, nil); got != "Отказ с причиной записан." {
		t.Fatalf("deny summary=%q", got)
	}
}
