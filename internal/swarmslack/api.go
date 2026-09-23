// Package swarmslack connects the existing Socket Mode ingress to the durable swarm runtime.
package swarmslack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spexus-ai/spexus-agent/internal/slack"
	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

type RateLimitError struct{ After time.Duration }

func (e *RateLimitError) Error() string { return "slack thread history rate limited" }

// VerifyWorkspace binds the bot token used for history to the configured
// workspace. History messages do not carry a team ID themselves.
func (a *API) VerifyWorkspace(ctx context.Context, configured string) error {
	if configured == "" {
		return errors.New("Slack workspace is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.BaseURL+"auth.test", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	res, err := a.Client.Do(req)
	if err != nil {
		return errors.New("Slack bot workspace verification unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("Slack bot workspace verification HTTP %d", res.StatusCode)
	}
	var identity struct {
		OK     bool   `json:"ok"`
		TeamID string `json:"team_id"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 16*1024)).Decode(&identity); err != nil || !identity.OK || identity.TeamID != configured {
		return errors.New("Slack bot workspace does not match configured workspace")
	}
	return nil
}

// ScanThread reads every page and passes only original human messages to the
// caller. A partial scan is never evidence that history has been caught up.
func (a *API) ScanThread(ctx context.Context, channel, thread, oldest string, consume func(string, string, string, string, bool) error) (string, error) {
	if channel == "" || !validTimestamp(thread) || !validTimestamp(oldest) {
		return "", errors.New("invalid Slack history scope")
	}
	cursor := ""
	seen := map[string]bool{}
	upper := ""
	for pages := 0; pages < 1000; pages++ {
		query := url.Values{"channel": {channel}, "ts": {thread}, "limit": {"100"}, "oldest": {oldest}, "inclusive": {"true"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.BaseURL+"conversations.replies?"+query.Encode(), nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+a.Token)
		res, err := a.Client.Do(req)
		if err != nil {
			return "", errors.New("slack history unavailable")
		}
		if res.StatusCode == http.StatusTooManyRequests {
			seconds, parseErr := strconv.Atoi(strings.TrimSpace(res.Header.Get("Retry-After")))
			res.Body.Close()
			if parseErr != nil || seconds < 1 {
				return "", errors.New("slack history rate limited without valid Retry-After")
			}
			return "", &RateLimitError{After: time.Duration(seconds) * time.Second}
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, 2*1024*1024+1))
		res.Body.Close()
		if err != nil || len(body) > 2*1024*1024 || res.StatusCode != http.StatusOK {
			return "", errors.New("slack history response unavailable or too large")
		}
		var page struct {
			OK       bool   `json:"ok"`
			HasMore  bool   `json:"has_more"`
			Error    string `json:"error"`
			Messages []struct {
				TS       string          `json:"ts"`
				ThreadTS string          `json:"thread_ts"`
				User     string          `json:"user"`
				Text     string          `json:"text"`
				BotID    string          `json:"bot_id"`
				AppID    string          `json:"app_id"`
				Subtype  string          `json:"subtype"`
				Edited   json.RawMessage `json:"edited"`
			} `json:"messages"`
			Metadata struct {
				Cursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if json.Unmarshal(body, &page) != nil || !page.OK {
			return "", errors.New("slack history response invalid")
		}
		for _, message := range page.Messages {
			if !validTimestamp(message.TS) {
				return "", errors.New("slack history message has invalid timestamp")
			}
			if upper == "" || timestampAfter(message.TS, upper) {
				upper = message.TS
			}
			if message.TS == thread || message.BotID != "" || message.AppID != "" || message.Subtype != "" || message.User == "" {
				continue
			}
			if message.ThreadTS != "" && message.ThreadTS != thread {
				return "", errors.New("slack history thread mismatch")
			}
			edited := len(message.Edited) > 0 && string(message.Edited) != "null"
			if err := consume(message.TS, message.User, message.Text, thread, edited); err != nil {
				return "", err
			}
		}
		next := strings.TrimSpace(page.Metadata.Cursor)
		if next == "" {
			if page.HasMore {
				return "", errors.New("incomplete slack history")
			}
			return upper, nil
		}
		if seen[next] || next == cursor {
			return "", errors.New("repeated slack history cursor")
		}
		seen[next] = true
		cursor = next
	}
	return "", errors.New("slack history page limit")
}

func timestampAfter(a, b string) bool {
	// Slack timestamps are decimal strings; comparing the integer part avoids
	// floating point precision loss in source provenance.
	x := strings.SplitN(a, ".", 2)
	y := strings.SplitN(b, ".", 2)
	if len(x) != 2 || len(y) != 2 || len(x[1]) == 0 || len(y[1]) == 0 || len(x[1]) > 6 || len(y[1]) > 6 {
		return false
	}
	as, ae := strconv.ParseInt(x[0], 10, 64)
	bs, be := strconv.ParseInt(y[0], 10, 64)
	if ae != nil || be != nil {
		return false
	}
	return as > bs || as == bs && x[1]+strings.Repeat("0", 6-len(x[1])) > y[1]+strings.Repeat("0", 6-len(y[1]))
}

func validTimestamp(ts string) bool {
	parts := strings.Split(ts, ".")
	if len(parts) != 2 || len(parts[0]) == 0 || len(parts[1]) == 0 || len(parts[1]) > 6 {
		return false
	}
	for _, r := range ts {
		if r != '.' && (r < '0' || r > '9') {
			return false
		}
	}
	_, err := strconv.ParseInt(parts[0], 10, 64)
	return err == nil
}

// API provides correlation-aware publication. A transport error has an unknown
// delivery outcome, so callers must inspect thread history before any retry.
type API struct {
	Token   string
	BaseURL string
	Client  *http.Client
}

func NewAPI(token string) *API {
	return &API{Token: token, BaseURL: "https://slack.com/api/", Client: &http.Client{Timeout: 30 * time.Second}}
}
func (a *API) call(ctx context.Context, method string, payload any, result any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.BaseURL+method, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	res, err := a.Client.Do(req)
	if err != nil {
		return errors.New("slack transport outcome unknown")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("slack HTTP %d", res.StatusCode)
	}
	if err = json.NewDecoder(io.LimitReader(res.Body, 2*1024*1024)).Decode(result); err != nil {
		return errors.New("slack response outcome unknown")
	}
	return nil
}

// SetThreadStatus shows transient progress in an existing Slack thread.
// An empty status clears the indicator; durable human state remains in Spexus.
func (a *API) SetThreadStatus(ctx context.Context, channelID, threadTS, status string) error {
	if len(channelID) < 2 || len(channelID) > 128 || (channelID[0] != 'C' && channelID[0] != 'G' && channelID[0] != 'D') || !validTimestamp(threadTS) || len(status) > 200 || !utf8.ValidString(status) || (status != "" && strings.TrimSpace(status) != status) {
		return errors.New("invalid Slack thread status")
	}
	for _, c := range channelID[1:] {
		if c < 'A' || c > 'Z' {
			if c < '0' || c > '9' {
				return errors.New("invalid Slack thread status")
			}
		}
	}
	for _, r := range status {
		if unicode.IsControl(r) {
			return errors.New("invalid Slack thread status")
		}
	}
	var result struct {
		OK bool `json:"ok"`
	}
	if err := a.call(ctx, "assistant.threads.setStatus", map[string]string{"channel_id": channelID, "thread_ts": threadTS, "status": status}, &result); err != nil {
		return err
	}
	if !result.OK {
		return errors.New("Slack thread status rejected")
	}
	return nil
}

func (a *API) Post(ctx context.Context, d swarm.SlackDelivery) (string, error) {
	var res struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		TS    string `json:"ts"`
	}
	payload := map[string]any{"channel": d.ChannelID, "thread_ts": d.ThreadTS, "text": d.Text, "client_msg_id": d.ID, "metadata": map[string]any{"event_type": "spexus_swarm_reply", "event_payload": map[string]string{"delivery_id": d.ID, "turn_id": d.TurnID}}}
	if d.Question != nil {
		message := humanReadableQuestionText(d.Text, d.Question.Options, d.ShortSelector, true)
		payload["text"] = message
		payload["blocks"] = humanQuestionBlocks(message, d.ID, d.Question.Options, false)
	}
	err := a.call(ctx, "chat.postMessage", payload, &res)
	if err != nil {
		return "", err
	}
	if !res.OK {
		return "", fmt.Errorf("slack API rejected: %s", res.Error)
	}
	if res.TS == "" {
		return "", errors.New("slack missing timestamp")
	}
	return res.TS, nil
}

// UpdateHumanQuestion is safe to repeat after an unknown chat.update outcome:
// it replaces the same bot message with deterministic blocks and text.
func (a *API) UpdateHumanQuestion(ctx context.Context, d swarm.SlackDelivery, options []swarm.HumanOption, state, result string) error {
	if d.Status != "sent" || !validTimestamp(d.SlackTS) {
		return errors.New("human question has no published Slack message")
	}
	message := humanReadableQuestionText(d.Text, options, d.ShortSelector, state == "open")
	switch state {
	case "open":
	case "pending":
		message += "\nОтвет получен. Проверяю запись в Spexus; решение пока не подтверждено."
	case "attention":
		message += "\nОтвет не удалось подтвердить в Spexus. Работа ждёт повторной проверки."
	case "stopped":
		message += "\nРабота остановлена. Ответы сейчас недоступны."
	case "cancelled":
		message += "\nЗапрос отменён. Ответы больше не принимаются."
	case "answered", "denied":
		message += "\nРешение записано в Spexus. Ответы больше не принимаются."
		if result != "" {
			message += " " + result
		}
	default:
		return errors.New("unknown human question state")
	}
	var res struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := a.call(ctx, "chat.update", map[string]any{"channel": d.ChannelID, "ts": d.SlackTS, "text": message, "blocks": humanQuestionBlocks(message, d.ID, options, state != "open")}, &res); err != nil {
		return err
	}
	if !res.OK {
		return fmt.Errorf("slack question update rejected: %s", res.Error)
	}
	return nil
}

func humanReadableQuestionText(message string, options []swarm.HumanOption, _ int, actionable bool) string {
	lines := strings.Split(message, "\n")
	if len(lines) > 0 && lines[0] == "Решение человека требуется для работы." {
		fields := map[string]string{}
		for _, line := range lines[1:] {
			for _, label := range []string{"Причина: ", "Контекст: ", "Вопрос: ", "Рекомендация: ", "Ожидает: "} {
				if strings.HasPrefix(line, label) {
					fields[label] = strings.TrimSpace(strings.TrimPrefix(line, label))
				}
			}
		}
		if fields["Вопрос: "] != "" {
			waiting := fields["Ожидает: "]
			if before, _, ok := strings.Cut(waiting, " (job "); ok {
				waiting = before
			}
			if before, _, ok := strings.Cut(waiting, " (step "); ok {
				waiting = before
			}
			readable := []string{"Нужен ваш ответ: " + compactHumanField(fields["Вопрос: "], 240)}
			for _, item := range []struct {
				label, value string
				limit        int
			}{
				{"Почему спрашиваю: ", fields["Причина: "], 130},
				{"Рекомендация: ", fields["Рекомендация: "], 130},
				{"Ждёт ответа: ", waiting, 100},
			} {
				if item.value != "" {
					readable = append(readable, item.label+compactHumanField(item.value, item.limit))
				}
			}
			if actionable {
				if len(options) > 0 {
					readable = append(readable, "Выберите вариант или ответьте своими словами в треде.")
				} else {
					readable = append(readable, "Ответьте своими словами в треде.")
				}
			}
			return strings.Join(readable, "\n")
		}
	}
	readable := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(line, "Запрос: ") {
			continue // Correlation stays in the button value and durable history.
		}
		if strings.HasPrefix(line, "Ответ: !answer ") || strings.HasPrefix(line, "Выберите вариант кнопкой ниже.") || strings.HasPrefix(line, "Ответьте через доступное действие в сообщении.") || strings.HasPrefix(line, "Напишите в этом треде: Ответ:") {
			if !actionable {
				continue
			}
			if len(options) == 0 {
				line = "Ответьте своими словами в этом треде. Можно также задать уточняющий вопрос или объяснить отказ."
			} else {
				line = "Выберите вариант кнопкой или ответьте своими словами в этом треде."
			}
		}
		readable = append(readable, line)
	}
	return strings.Join(readable, "\n")
}

func compactHumanField(value string, limit int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) > limit {
		return strings.TrimSpace(string(runes[:limit-1])) + "…"
	}
	return value
}

func humanQuestionBlocks(message, requestID string, options []swarm.HumanOption, closed bool) []map[string]any {
	blocks := make([]map[string]any, 0, 18)
	for len(message) > 0 {
		cut := 2900
		if len(message) < cut {
			cut = len(message)
		}
		for cut > 0 && cut < len(message) && !utf8.RuneStart(message[cut]) {
			cut--
		}
		blocks = append(blocks, map[string]any{"type": "section", "text": map[string]any{"type": "plain_text", "text": message[:cut], "emoji": false}})
		message = message[cut:]
	}
	if !closed {
		elements := make([]map[string]any, 0, len(options)+2)
		for _, option := range options {
			label := option.Label
			if len([]rune(label)) > 70 {
				label = string([]rune(label)[:69]) + "…"
			}
			value, _ := json.Marshal(map[string]string{"request_id": requestID, "option_id": option.ID})
			elements = append(elements, map[string]any{"type": "button", "text": map[string]any{"type": "plain_text", "text": label, "emoji": false}, "action_id": slack.HumanAnswerActionID + ":" + option.ID, "value": string(value), "accessibility_label": label})
		}
		for _, control := range []struct{ id, label string }{{"details", "Подробнее"}, {"stop", "Остановить"}} {
			value, _ := json.Marshal(map[string]string{"request_id": requestID, "control_id": control.id})
			elements = append(elements, map[string]any{"type": "button", "text": map[string]any{"type": "plain_text", "text": control.label, "emoji": false}, "action_id": slack.HumanControlActionID + ":" + control.id, "value": string(value), "accessibility_label": control.label})
		}
		blocks = append(blocks, map[string]any{"type": "actions", "elements": elements})
	}
	return blocks
}

// Find checks every page of the original thread. An incomplete/unavailable
// history cannot prove absence and therefore never authorizes resend.
func (a *API) Find(ctx context.Context, d swarm.SlackDelivery) (string, bool, error) {
	cursor := ""
	seen := map[string]bool{}
	for pages := 0; pages < 1000; pages++ {
		values := url.Values{"channel": {d.ChannelID}, "ts": {d.ThreadTS}, "limit": {"100"}, "include_all_metadata": {"true"}}
		if cursor != "" {
			values.Set("cursor", cursor)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.BaseURL+"conversations.replies?"+values.Encode(), nil)
		if err != nil {
			return "", false, err
		}
		req.Header.Set("Authorization", "Bearer "+a.Token)
		res, err := a.Client.Do(req)
		if err != nil {
			return "", false, errors.New("slack history unavailable")
		}
		var page struct {
			OK       bool   `json:"ok"`
			Error    string `json:"error"`
			HasMore  bool   `json:"has_more"`
			Messages []struct {
				TS       string `json:"ts"`
				ClientID string `json:"client_msg_id"`
				Metadata struct {
					Type    string `json:"event_type"`
					Payload struct {
						ID string `json:"delivery_id"`
					} `json:"event_payload"`
				} `json:"metadata"`
			} `json:"messages"`
			Metadata struct {
				Cursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		err = json.NewDecoder(io.LimitReader(res.Body, 2*1024*1024)).Decode(&page)
		res.Body.Close()
		if err != nil || res.StatusCode != 200 || !page.OK {
			return "", false, errors.New("slack history unavailable; operator reconciliation required")
		}
		for _, m := range page.Messages {
			if m.ClientID == d.ID || (m.Metadata.Type == "spexus_swarm_reply" && m.Metadata.Payload.ID == d.ID) {
				if m.TS == "" {
					return "", false, errors.New("correlated message missing timestamp")
				}
				return m.TS, true, nil
			}
		}
		cursor = strings.TrimSpace(page.Metadata.Cursor)
		if cursor == "" {
			if page.HasMore {
				return "", false, errors.New("incomplete slack history")
			}
			return "", false, nil
		}
		if seen[cursor] {
			return "", false, errors.New("repeated slack history cursor")
		}
		seen[cursor] = true
	}
	return "", false, errors.New("slack history page limit; operator reconciliation required")
}
