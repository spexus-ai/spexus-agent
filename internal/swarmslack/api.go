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

	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

type RateLimitError struct{ After time.Duration }

func (e *RateLimitError) Error() string { return "slack thread history rate limited" }

// ScanThread reads every page and passes only original human messages to the
// caller. A partial scan is never evidence that history has been caught up.
func (a *API) ScanThread(ctx context.Context, channel, thread, oldest string, consume func(string, string, string, string) error) (string, error) {
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
				TS       string `json:"ts"`
				ThreadTS string `json:"thread_ts"`
				User     string `json:"user"`
				Text     string `json:"text"`
				BotID    string `json:"bot_id"`
				AppID    string `json:"app_id"`
				Subtype  string `json:"subtype"`
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
			if err := consume(message.TS, message.User, message.Text, thread); err != nil {
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
func (a *API) Post(ctx context.Context, d swarm.SlackDelivery) (string, error) {
	var res struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		TS    string `json:"ts"`
	}
	err := a.call(ctx, "chat.postMessage", map[string]any{"channel": d.ChannelID, "thread_ts": d.ThreadTS, "text": d.Text, "client_msg_id": d.ID, "metadata": map[string]any{"event_type": "spexus_swarm_reply", "event_payload": map[string]string{"delivery_id": d.ID, "turn_id": d.TurnID}}}, &res)
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
