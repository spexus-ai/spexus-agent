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
	"strings"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

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
