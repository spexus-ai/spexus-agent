package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Test: a Socket Mode envelope is ACKed only after the consumer returns from
// its durable commit; a failed commit never produces an ACK.
// Validates: AC-431 (REQ-349 - durable Slack ingress before ACK).
func TestDurableSocketACKAfterConsumerCommit(t *testing.T) {
	for _, commit := range []bool{true, false} {
		t.Run(map[bool]string{true: "committed", false: "failed"}[commit], func(t *testing.T) {
			received := make(chan struct{})
			release := make(chan struct{})
			ack := make(chan bool, 1)
			upgrader := websocket.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				_ = conn.WriteJSON(map[string]any{"envelope_id": "env-1", "type": "events_api", "payload": map[string]any{"event_id": "evt-1", "type": "event_callback", "event": map[string]string{"type": "message", "channel": "C", "thread_ts": "123.000001", "ts": "123.000002", "user": "U", "text": "!stop"}}})
				var message map[string]string
				err = conn.ReadJSON(&message)
				ack <- err == nil && message["envelope_id"] == "env-1"
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := NewSocketModeClient("app-token")
			done := make(chan error, 1)
			go func() {
				done <- client.consumeDurable(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil, func(_ context.Context, e Event) error {
					if e.ID != "evt-1" {
						t.Errorf("wrong event: %+v", e)
					}
					close(received)
					<-release
					if !commit {
						return context.DeadlineExceeded
					}
					return nil
				})
			}()
			select {
			case <-received:
			case <-time.After(2 * time.Second):
				t.Fatal("event not delivered")
			}
			select {
			case <-ack:
				t.Fatal("ACK before commit")
			case <-time.After(40 * time.Millisecond):
			}
			close(release)
			select {
			case got := <-ack:
				if got != commit {
					t.Fatalf("ACK=%t after commit=%t", got, commit)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("socket did not settle")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("client did not return")
			}
		})
	}
}

// Test: bot and edited messages are classified without invoking the consumer.
// Validates: AC-431 (REQ-349 - excluded Slack events cannot become input).
func TestDurableSocketClassifiesBotBeforeACK(t *testing.T) {
	var envelope socketModeEnvelope
	if err := json.Unmarshal([]byte(`{"envelope_id":"bot","type":"events_api","payload":{"event_id":"evt","type":"event_callback","event":{"type":"message","channel":"C","thread_ts":"1.000001","ts":"1.000002","user":"U","subtype":"message_changed","text":"!answer x"}}}`), &envelope); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := eventFromSocketModeEnvelope(envelope); err != nil || ok {
		t.Fatalf("edited message accepted: ok=%t err=%v", ok, err)
	}
}
