package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const defaultAPIBaseURL = "https://slack.com/api/"
const MessageTextSoftLimit = 3900
const slackMessageTextSoftLimit = MessageTextSoftLimit

type HTTPClient struct {
	token   string
	baseURL string
	client  *http.Client
}

func NewHTTPClient(token string) *HTTPClient {
	return &HTTPClient{
		token:   token,
		baseURL: defaultAPIBaseURL,
		client:  &http.Client{},
	}
}

func (c *HTTPClient) PostMessage(ctx context.Context, message Message) (PostedMessage, error) {
	parts := splitSlackMessageText(message.Text)
	if len(parts) == 0 {
		parts = []string{message.Text}
	}

	first := message
	first.Text = parts[0]
	response, err := c.postMessage(ctx, first)
	if err != nil {
		return PostedMessage{}, err
	}
	threadTS := strings.TrimSpace(message.ThreadTS)
	if threadTS == "" {
		threadTS = response.Timestamp
	}
	for _, part := range parts[1:] {
		next := message
		next.ThreadTS = threadTS
		next.Text = part
		if _, err := c.postMessage(ctx, next); err != nil {
			return PostedMessage{}, err
		}
	}
	return response, nil
}

func (c *HTTPClient) PostThreadMessage(ctx context.Context, message Message) error {
	parts := splitSlackMessageText(message.Text)
	if len(parts) == 0 {
		parts = []string{message.Text}
	}
	for _, part := range parts {
		next := message
		next.Text = part
		if _, err := c.postMessage(ctx, next); err != nil {
			return err
		}
	}
	return nil
}

func (c *HTTPClient) UpdateMessage(ctx context.Context, update MessageUpdate) error {
	if c == nil {
		return fmt.Errorf("slack http client is nil")
	}
	if strings.TrimSpace(update.ChannelID) == "" {
		return fmt.Errorf("channel id is required")
	}
	if strings.TrimSpace(update.Timestamp) == "" {
		return fmt.Errorf("message timestamp is required")
	}
	if len([]rune(update.Text)) > MessageTextSoftLimit {
		return fmt.Errorf("message text exceeds slack soft limit")
	}

	payload := map[string]string{
		"channel": strings.TrimSpace(update.ChannelID),
		"ts":      strings.TrimSpace(update.Timestamp),
		"text":    update.Text,
	}

	var response slackAPIResponse
	if err := c.do(ctx, "chat.update", payload, &response); err != nil {
		return err
	}
	if !response.OK {
		return fmt.Errorf("slack chat.update request failed: %s", response.Error)
	}
	return nil
}

func (c *HTTPClient) PostResponseURLMessage(ctx context.Context, message ResponseURLMessage) error {
	if c == nil {
		return fmt.Errorf("slack http client is nil")
	}

	responseURL := strings.TrimSpace(message.ResponseURL)
	if responseURL == "" {
		return fmt.Errorf("response url is required")
	}
	if c.client == nil {
		c.client = &http.Client{}
	}

	if responseType := strings.TrimSpace(message.ResponseType); responseType != "" {
		message.ResponseType = responseType
	}

	parts := splitSlackMessageText(message.Text)
	if len(parts) == 0 {
		parts = []string{message.Text}
	}
	for _, part := range parts {
		payload := map[string]string{
			"text": part,
		}
		if message.ResponseType != "" {
			payload["response_type"] = message.ResponseType
		}

		body, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode slack response_url request: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, responseURL, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("create slack response_url request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json; charset=utf-8")

		res, err := c.client.Do(req)
		if err != nil {
			return fmt.Errorf("call slack response_url: %w", err)
		}

		if _, err := io.ReadAll(res.Body); err != nil {
			res.Body.Close()
			return fmt.Errorf("read slack response_url response: %w", err)
		}
		if closeErr := res.Body.Close(); closeErr != nil {
			return fmt.Errorf("close slack response_url response: %w", closeErr)
		}
		if res.StatusCode >= http.StatusBadRequest {
			return fmt.Errorf("slack response_url request failed with status %s", res.Status)
		}
	}

	return nil
}

func splitSlackMessageText(text string) []string {
	if len([]rune(text)) <= slackMessageTextSoftLimit {
		return []string{text}
	}

	remaining := []rune(text)
	parts := make([]string, 0, len(remaining)/slackMessageTextSoftLimit+1)
	for len(remaining) > slackMessageTextSoftLimit {
		cut := slackMessageTextSoftLimit
		for i := slackMessageTextSoftLimit; i > 0; i-- {
			if remaining[i-1] == '\n' {
				cut = i - 1
				break
			}
		}
		if cut == 0 {
			cut = slackMessageTextSoftLimit
		}
		part := strings.TrimRight(string(remaining[:cut]), "\n")
		if part != "" {
			parts = append(parts, part)
		}
		remaining = remaining[cut:]
		if len(remaining) > 0 && remaining[0] == '\n' {
			remaining = remaining[1:]
		}
	}
	if len(remaining) > 0 {
		parts = append(parts, string(remaining))
	}
	return parts
}

func (c *HTTPClient) postMessage(ctx context.Context, message Message) (PostedMessage, error) {
	if c == nil {
		return PostedMessage{}, fmt.Errorf("slack http client is nil")
	}
	if strings.TrimSpace(message.ChannelID) == "" {
		return PostedMessage{}, fmt.Errorf("channel id is required")
	}

	payload := map[string]string{
		"channel": message.ChannelID,
		"text":    message.Text,
	}
	if threadTS := strings.TrimSpace(message.ThreadTS); threadTS != "" {
		payload["thread_ts"] = threadTS
	}

	var response struct {
		slackAPIResponse
		Timestamp string `json:"ts"`
	}
	if err := c.do(ctx, "chat.postMessage", payload, &response); err != nil {
		return PostedMessage{}, err
	}
	if !response.OK {
		return PostedMessage{}, fmt.Errorf("slack chat.postMessage request failed: %s", response.Error)
	}
	if strings.TrimSpace(response.Timestamp) == "" {
		return PostedMessage{}, fmt.Errorf("slack chat.postMessage response is missing ts")
	}
	return PostedMessage{
		ChannelID: strings.TrimSpace(message.ChannelID),
		Timestamp: strings.TrimSpace(response.Timestamp),
	}, nil
}

func (c *HTTPClient) CreateChannel(ctx context.Context, request CreateChannelRequest) (Channel, error) {
	if c == nil {
		return Channel{}, fmt.Errorf("slack http client is nil")
	}
	name := strings.TrimSpace(request.Name)
	if name == "" {
		return Channel{}, fmt.Errorf("channel name is required")
	}

	payload := map[string]any{
		"name": name,
	}

	var response struct {
		slackAPIResponse
		Channel Channel `json:"channel"`
	}
	if err := c.do(ctx, "conversations.create", payload, &response); err != nil {
		return Channel{}, err
	}
	if !response.OK {
		return Channel{}, newSlackAPIError("conversations.create", response.Error)
	}
	if response.Channel.ID == "" {
		return Channel{}, fmt.Errorf("slack channel id is required")
	}
	if response.Channel.Name == "" {
		response.Channel.Name = name
	}
	return response.Channel, nil
}

func (c *HTTPClient) FindChannelByName(ctx context.Context, name string) (Channel, error) {
	if c == nil {
		return Channel{}, fmt.Errorf("slack http client is nil")
	}

	channelName := strings.TrimSpace(name)
	if channelName == "" {
		return Channel{}, fmt.Errorf("channel name is required")
	}

	cursor := ""
	for {
		payload := map[string]any{
			"exclude_archived": true,
			"limit":            1000,
			"types":            "public_channel,private_channel",
		}
		if cursor != "" {
			payload["cursor"] = cursor
		}

		var response struct {
			slackAPIResponse
			Channels []Channel `json:"channels"`
			Metadata struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := c.do(ctx, "conversations.list", payload, &response); err != nil {
			return Channel{}, err
		}
		if !response.OK {
			return Channel{}, newSlackAPIError("conversations.list", response.Error)
		}

		for _, channel := range response.Channels {
			if channel.Name == channelName {
				if channel.ID == "" {
					return Channel{}, fmt.Errorf("slack channel id is required")
				}
				return channel, nil
			}
		}

		cursor = strings.TrimSpace(response.Metadata.NextCursor)
		if cursor == "" {
			break
		}
	}

	return Channel{}, fmt.Errorf("%w: %s", ErrChannelNotFound, channelName)
}

func (c *HTTPClient) Close() error {
	return nil
}

type slackAPIResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

type slackAPIError struct {
	method string
	code   string
}

func (e *slackAPIError) Error() string {
	return fmt.Sprintf("slack %s request failed: %s", e.method, e.code)
}

func (e *slackAPIError) Unwrap() error {
	switch e.code {
	case "name_taken":
		return ErrChannelNameTaken
	default:
		return nil
	}
}

func newSlackAPIError(method, code string) error {
	if strings.TrimSpace(code) == "" {
		return fmt.Errorf("slack %s request failed", method)
	}
	return &slackAPIError{method: method, code: code}
}

func (c *HTTPClient) do(ctx context.Context, method string, payload any, response any) error {
	if c.token == "" {
		return fmt.Errorf("slack bot token is required")
	}
	if c.client == nil {
		c.client = &http.Client{}
	}
	if c.baseURL == "" {
		c.baseURL = defaultAPIBaseURL
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode slack %s request: %w", method, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+method, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create slack %s request: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	res, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("call slack %s: %w", method, err)
	}
	defer res.Body.Close()

	data, err := io.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("read slack %s response: %w", method, err)
	}
	if res.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("slack %s request failed with status %s", method, res.Status)
	}

	if err := json.Unmarshal(data, response); err != nil {
		return fmt.Errorf("decode slack %s response: %w", method, err)
	}
	return nil
}
