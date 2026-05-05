package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const defaultSocketModeOpenURL = "https://slack.com/api/apps.connections.open"

type SocketModeClient struct {
	appToken   string
	openURL    string
	httpClient *http.Client
	dialer     *websocket.Dialer
	debugf     func(string, ...any)

	mu   sync.Mutex
	conn *websocket.Conn
}

func NewSocketModeClient(appToken string) *SocketModeClient {
	return &SocketModeClient{
		appToken:   strings.TrimSpace(appToken),
		openURL:    defaultSocketModeOpenURL,
		httpClient: &http.Client{},
		dialer:     websocket.DefaultDialer,
	}
}

func (c *SocketModeClient) SetDebugLogger(fn func(string, ...any)) {
	if c == nil {
		return
	}
	c.debugf = fn
}

func (c *SocketModeClient) Events(ctx context.Context) (<-chan Event, error) {
	if c == nil {
		return nil, fmt.Errorf("slack socket mode client is nil")
	}
	if strings.TrimSpace(c.appToken) == "" {
		return nil, fmt.Errorf("slack app token is required")
	}

	events := make(chan Event)
	go c.runEvents(ctx, events)
	return events, nil
}

func (c *SocketModeClient) InboundInvocations(ctx context.Context) (<-chan InboundInvocation, error) {
	if c == nil {
		return nil, fmt.Errorf("slack socket mode client is nil")
	}
	if strings.TrimSpace(c.appToken) == "" {
		return nil, fmt.Errorf("slack app token is required")
	}

	invocations := make(chan InboundInvocation)
	go c.runInvocations(ctx, invocations)
	return invocations, nil
}

func (c *SocketModeClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

func (c *SocketModeClient) runEvents(ctx context.Context, out chan<- Event) {
	defer close(out)

	backoff := time.Second
	for ctx.Err() == nil {
		socketURL, err := c.openConnection(ctx)
		if err != nil {
			if !sleepWithContext(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}

		if err := c.readEventLoop(ctx, socketURL, out); err != nil && ctx.Err() == nil {
			if !sleepWithContext(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}

		backoff = time.Second
	}
}

func (c *SocketModeClient) runInvocations(ctx context.Context, out chan<- InboundInvocation) {
	defer close(out)

	backoff := time.Second
	for ctx.Err() == nil {
		socketURL, err := c.openConnection(ctx)
		if err != nil {
			if !sleepWithContext(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}

		if err := c.readInvocationLoop(ctx, socketURL, out); err != nil && ctx.Err() == nil {
			if !sleepWithContext(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}

		backoff = time.Second
	}
}

func (c *SocketModeClient) openConnection(ctx context.Context) (string, error) {
	payload := bytes.NewReader([]byte("{}"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.socketModeOpenURL(), payload)
	if err != nil {
		return "", fmt.Errorf("create slack apps.connections.open request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.appToken)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	res, err := c.httpClientOrDefault().Do(req)
	if err != nil {
		return "", fmt.Errorf("call slack apps.connections.open: %w", err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", fmt.Errorf("read slack apps.connections.open response: %w", err)
	}
	if res.StatusCode >= http.StatusBadRequest {
		return "", fmt.Errorf("slack apps.connections.open request failed with status %s", res.Status)
	}

	var response struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
		URL   string `json:"url,omitempty"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", fmt.Errorf("decode slack apps.connections.open response: %w", err)
	}
	if !response.OK {
		return "", fmt.Errorf("slack apps.connections.open request failed: %s", response.Error)
	}
	if strings.TrimSpace(response.URL) == "" {
		return "", fmt.Errorf("slack apps.connections.open response is missing url")
	}
	return response.URL, nil
}

func (c *SocketModeClient) readEventLoop(ctx context.Context, socketURL string, out chan<- Event) error {
	conn, _, err := c.dialerOrDefault().DialContext(ctx, socketURL, nil)
	if err != nil {
		return fmt.Errorf("dial slack socket mode websocket: %w", err)
	}
	c.logf("runtime.socket: websocket connected url=%s", socketURL)
	c.setConn(conn)
	defer func() {
		_ = c.Close()
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read slack socket mode message: %w", err)
		}
		c.logf("runtime.socket: raw recv %s", strings.TrimSpace(string(data)))

		envelope, err := decodeSocketModeEnvelope(data)
		if err != nil {
			c.logf("runtime.socket: envelope decode failed: %v", err)
			continue
		}
		c.logf("runtime.socket: envelope type=%s envelope_id=%s", envelope.Type, envelope.EnvelopeID)

		if envelope.EnvelopeID != "" {
			if err := conn.WriteJSON(map[string]string{"envelope_id": envelope.EnvelopeID}); err != nil {
				return fmt.Errorf("ack slack socket mode envelope: %w", err)
			}
			c.logf("runtime.socket: ack envelope_id=%s", envelope.EnvelopeID)
		}

		event, ok, err := eventFromSocketModeEnvelope(envelope)
		if err != nil {
			c.logf("runtime.socket: event extraction failed: %v", err)
			if envelope.Type == "disconnect" {
				return nil
			}
			continue
		}
		if !ok {
			c.logf("runtime.socket: envelope ignored type=%s", envelope.Type)
			if envelope.Type == "disconnect" {
				return nil
			}
			continue
		}
		c.logf("runtime.socket: normalized event id=%s type=%s channel=%s thread=%s", event.ID, envelope.Type, event.ChannelID, event.ThreadTimestamp())

		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- event:
		}
	}
}

func (c *SocketModeClient) readInvocationLoop(ctx context.Context, socketURL string, out chan<- InboundInvocation) error {
	conn, _, err := c.dialerOrDefault().DialContext(ctx, socketURL, nil)
	if err != nil {
		return fmt.Errorf("dial slack socket mode websocket: %w", err)
	}
	c.logf("runtime.socket: websocket connected url=%s", socketURL)
	c.setConn(conn)
	defer func() {
		_ = c.Close()
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read slack socket mode message: %w", err)
		}
		c.logf("runtime.socket: raw recv %s", strings.TrimSpace(string(data)))

		envelope, err := decodeSocketModeEnvelope(data)
		if err != nil {
			c.logf("runtime.socket: envelope decode failed: %v", err)
			continue
		}
		c.logf("runtime.socket: envelope type=%s envelope_id=%s", envelope.Type, envelope.EnvelopeID)

		if envelope.EnvelopeID != "" {
			if err := conn.WriteJSON(map[string]string{"envelope_id": envelope.EnvelopeID}); err != nil {
				return fmt.Errorf("ack slack socket mode envelope: %w", err)
			}
			c.logf("runtime.socket: ack envelope_id=%s", envelope.EnvelopeID)
		}

		invocation, ok, err := invocationFromSocketModeEnvelope(envelope)
		if err != nil {
			c.logf("runtime.socket: inbound extraction failed: %v", err)
			if envelope.Type == "disconnect" {
				return nil
			}
			continue
		}
		if !ok {
			c.logf("runtime.socket: envelope ignored type=%s", envelope.Type)
			if envelope.Type == "disconnect" {
				return nil
			}
			continue
		}
		c.logf(
			"runtime.socket: normalized inbound delivery=%s source=%s channel=%s thread=%s",
			invocation.DeliveryID,
			invocation.SourceType,
			invocation.ChannelID,
			invocation.ThreadTS,
		)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- invocation:
		}
	}
}

type socketModeEnvelope struct {
	EnvelopeID string          `json:"envelope_id,omitempty"`
	Type       string          `json:"type,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

type socketModeEventsPayload struct {
	EventID string `json:"event_id,omitempty"`
	Type    string `json:"type,omitempty"`
	Event   struct {
		Type      string `json:"type,omitempty"`
		ChannelID string `json:"channel,omitempty"`
		ThreadTS  string `json:"thread_ts,omitempty"`
		Timestamp string `json:"ts,omitempty"`
		UserID    string `json:"user,omitempty"`
		BotID     string `json:"bot_id,omitempty"`
		AppID     string `json:"app_id,omitempty"`
		Text      string `json:"text,omitempty"`
		Subtype   string `json:"subtype,omitempty"`
	} `json:"event,omitempty"`
}

func decodeSocketModeEnvelope(data []byte) (socketModeEnvelope, error) {
	var envelope socketModeEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return socketModeEnvelope{}, err
	}
	return envelope, nil
}

func eventFromSocketModeEnvelope(envelope socketModeEnvelope) (Event, bool, error) {
	if strings.TrimSpace(envelope.Type) != SocketModeEnvelopeEventsAPI {
		return Event{}, false, nil
	}

	var payload socketModeEventsPayload
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		return Event{}, false, err
	}
	if payload.Type != "" && strings.TrimSpace(payload.Type) != "event_callback" {
		return Event{}, false, nil
	}

	message := SocketModeMessage{
		EventID:   payload.EventID,
		Type:      payload.Event.Type,
		ChannelID: payload.Event.ChannelID,
		ThreadTS:  payload.Event.ThreadTS,
		Timestamp: payload.Event.Timestamp,
		UserID:    payload.Event.UserID,
		BotID:     payload.Event.BotID,
		AppID:     payload.Event.AppID,
		Text:      payload.Event.Text,
		Subtype:   payload.Event.Subtype,
	}
	if strings.TrimSpace(message.Subtype) != "" || strings.TrimSpace(message.BotID) != "" || strings.TrimSpace(message.AppID) != "" {
		return Event{}, false, nil
	}

	event, err := message.Normalize()
	if err != nil {
		return Event{}, false, err
	}
	return event, true, nil
}

func invocationFromSocketModeEnvelope(envelope socketModeEnvelope) (InboundInvocation, bool, error) {
	switch strings.TrimSpace(envelope.Type) {
	case SocketModeEnvelopeEventsAPI:
		var payload socketModeEventsPayload
		if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
			return InboundInvocation{}, false, err
		}
		if payload.Type != "" && strings.TrimSpace(payload.Type) != "event_callback" {
			return InboundInvocation{}, false, nil
		}

		message := SocketModeMessage{
			EventID:   payload.EventID,
			Type:      payload.Event.Type,
			ChannelID: payload.Event.ChannelID,
			ThreadTS:  payload.Event.ThreadTS,
			Timestamp: payload.Event.Timestamp,
			UserID:    payload.Event.UserID,
			BotID:     payload.Event.BotID,
			AppID:     payload.Event.AppID,
			Text:      payload.Event.Text,
			Subtype:   payload.Event.Subtype,
		}
		if strings.TrimSpace(message.Subtype) != "" || strings.TrimSpace(message.BotID) != "" || strings.TrimSpace(message.AppID) != "" {
			return InboundInvocation{}, false, nil
		}

		invocation, err := message.NormalizeInbound()
		if err != nil {
			return InboundInvocation{}, false, err
		}
		return invocation, true, nil
	case SocketModeEnvelopeSlashCommands:
		var payload SocketModeSlashCommand
		if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
			return InboundInvocation{}, false, err
		}

		invocation, err := payload.NormalizeInbound(envelope.EnvelopeID)
		if err != nil {
			return InboundInvocation{}, false, err
		}
		invocation.Acked = true
		return invocation, true, nil
	default:
		return InboundInvocation{}, false, nil
	}
}

func (c *SocketModeClient) httpClientOrDefault() *http.Client {
	if c.httpClient == nil {
		c.httpClient = &http.Client{}
	}
	return c.httpClient
}

func (c *SocketModeClient) dialerOrDefault() *websocket.Dialer {
	if c.dialer == nil {
		c.dialer = websocket.DefaultDialer
	}
	return c.dialer
}

func (c *SocketModeClient) socketModeOpenURL() string {
	if strings.TrimSpace(c.openURL) == "" {
		return defaultSocketModeOpenURL
	}
	return c.openURL
}

func (c *SocketModeClient) setConn(conn *websocket.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conn = conn
}

func (c *SocketModeClient) logf(format string, args ...any) {
	if c != nil && c.debugf != nil {
		c.debugf(format, args...)
	}
}

func sleepWithContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextBackoff(current time.Duration) time.Duration {
	if current <= 0 {
		return time.Second
	}
	current *= 2
	if current > 30*time.Second {
		return 30 * time.Second
	}
	return current
}
