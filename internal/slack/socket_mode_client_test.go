package slack

import "testing"

// Test: app_mention Socket Mode envelopes map into the shared inbound invocation model for mention routing.
// Validates: AC-1815 (REQ-1180 - mention invocations resolve by channel_id), AC-1823 (REQ-1193 - mention source classification)
func TestInvocationFromSocketModeEnvelopeNormalizesAppMention(t *testing.T) {
	t.Parallel()

	envelope := socketModeEnvelope{
		Type: SocketModeEnvelopeEventsAPI,
		Payload: []byte(`{
			"event_id": "Ev123",
			"type": "event_callback",
			"event": {
				"type": "app_mention",
				"channel": "C123",
				"thread_ts": "1713686400.000100",
				"ts": "1713686400.000200",
				"user": "U123",
				"text": "<@Ubot> status"
			}
		}`),
	}

	invocation, ok, err := invocationFromSocketModeEnvelope(envelope)
	if err != nil {
		t.Fatalf("invocationFromSocketModeEnvelope() error = %v", err)
	}
	if !ok {
		t.Fatalf("invocationFromSocketModeEnvelope() ok = false, want true")
	}
	if invocation.SourceType != InboundSourceMention || invocation.DeliveryID != "Ev123" || invocation.ThreadTS != "1713686400.000100" {
		t.Fatalf("invocation = %#v, want normalized mention inbound invocation", invocation)
	}
}

func TestInvocationFromSocketModeEnvelopeNormalizesThreadMessage(t *testing.T) {
	t.Parallel()

	envelope := socketModeEnvelope{
		Type: SocketModeEnvelopeEventsAPI,
		Payload: []byte(`{
			"event_id": "Ev-message",
			"type": "event_callback",
			"event": {
				"type": "message",
				"channel": "C123",
				"thread_ts": "1713686400.000100",
				"ts": "1713686400.000200",
				"user": "U123",
				"text": "summarize current project state"
			}
		}`),
	}

	invocation, ok, err := invocationFromSocketModeEnvelope(envelope)
	if err != nil {
		t.Fatalf("invocationFromSocketModeEnvelope() error = %v", err)
	}
	if !ok {
		t.Fatalf("invocationFromSocketModeEnvelope() ok = false, want true")
	}
	if invocation.SourceType != InboundSourceMessage || invocation.DeliveryID != "Ev-message" || invocation.ThreadTS != "1713686400.000100" {
		t.Fatalf("invocation = %#v, want normalized message inbound invocation", invocation)
	}
}

// Test: slash_commands Socket Mode envelopes map into the shared inbound invocation model and preserve the ack envelope id.
// Validates: AC-1818 (REQ-1186 - slash invocations ack within the Slack window), AC-1824 (REQ-1193 - slash source classification)
func TestInvocationFromSocketModeEnvelopeNormalizesSlashCommand(t *testing.T) {
	t.Parallel()

	envelope := socketModeEnvelope{
		EnvelopeID: "3-fwdc2",
		Type:       SocketModeEnvelopeSlashCommands,
		Payload: []byte(`{
			"command": "/spexus",
			"channel_id": "C456",
			"user_id": "U456",
			"text": "status",
			"response_url": "https://hooks.slack.test/response"
		}`),
	}

	invocation, ok, err := invocationFromSocketModeEnvelope(envelope)
	if err != nil {
		t.Fatalf("invocationFromSocketModeEnvelope() error = %v", err)
	}
	if !ok {
		t.Fatalf("invocationFromSocketModeEnvelope() ok = false, want true")
	}
	if invocation.SourceType != InboundSourceSlash || invocation.DeliveryID != "3-fwdc2" || invocation.AckEnvelopeID != "3-fwdc2" || !invocation.Acked {
		t.Fatalf("invocation = %#v, want normalized slash inbound invocation", invocation)
	}
}

func TestEventFromSocketModeEnvelopeNormalizesMessageEvent(t *testing.T) {
	t.Parallel()

	envelope := socketModeEnvelope{
		Type: SocketModeEnvelopeEventsAPI,
		Payload: []byte(`{
			"event_id": "Ev123",
			"type": "event_callback",
			"event": {
				"type": "message",
				"channel": "C123",
				"thread_ts": "1713686400.000100",
				"ts": "1713686400.000200",
				"user": "U123",
				"text": "hello from slack"
			}
		}`),
	}

	event, ok, err := eventFromSocketModeEnvelope(envelope)
	if err != nil {
		t.Fatalf("eventFromSocketModeEnvelope() error = %v", err)
	}
	if !ok {
		t.Fatalf("eventFromSocketModeEnvelope() ok = false, want true")
	}
	if event.ID != "Ev123" || event.ChannelID != "C123" || event.ThreadTS != "1713686400.000100" || event.Timestamp != "1713686400.000200" {
		t.Fatalf("event = %#v, want normalized Slack event", event)
	}
}

func TestEventFromSocketModeEnvelopeIgnoresSubtypeMessages(t *testing.T) {
	t.Parallel()

	envelope := socketModeEnvelope{
		Type: SocketModeEnvelopeEventsAPI,
		Payload: []byte(`{
			"event_id": "Ev123",
			"type": "event_callback",
			"event": {
				"type": "message",
				"subtype": "bot_message",
				"channel": "C123",
				"ts": "1713686400.000200",
				"user": "U123",
				"text": "bot message"
			}
		}`),
	}

	_, ok, err := eventFromSocketModeEnvelope(envelope)
	if err != nil {
		t.Fatalf("eventFromSocketModeEnvelope() error = %v", err)
	}
	if ok {
		t.Fatalf("eventFromSocketModeEnvelope() ok = true, want false for subtype event")
	}
}

func TestEventFromSocketModeEnvelopeIgnoresBotMessages(t *testing.T) {
	t.Parallel()

	envelope := socketModeEnvelope{
		Type: SocketModeEnvelopeEventsAPI,
		Payload: []byte(`{
			"event_id": "Ev126",
			"type": "event_callback",
			"event": {
				"type": "message",
				"channel": "C123",
				"thread_ts": "1713686400.000100",
				"ts": "1713686401.000200",
				"user": "Ubot",
				"bot_id": "B123",
				"app_id": "A123",
				"text": "Session error..."
			}
		}`),
	}

	_, ok, err := eventFromSocketModeEnvelope(envelope)
	if err != nil {
		t.Fatalf("eventFromSocketModeEnvelope() error = %v", err)
	}
	if ok {
		t.Fatalf("eventFromSocketModeEnvelope() ok = true, want false for bot message")
	}
}

func TestEventFromSocketModeEnvelopeRejectsUnexpectedPayloadType(t *testing.T) {
	t.Parallel()

	envelope := socketModeEnvelope{
		Type:    SocketModeEnvelopeEventsAPI,
		Payload: []byte(`{"event_id":"Ev123","type":"url_verification"}`),
	}

	_, ok, err := eventFromSocketModeEnvelope(envelope)
	if err != nil {
		t.Fatalf("eventFromSocketModeEnvelope() error = %v", err)
	}
	if ok {
		t.Fatalf("eventFromSocketModeEnvelope() ok = true, want false")
	}
}

func TestEventFromSocketModeEnvelopeIgnoresNonEventEnvelopes(t *testing.T) {
	t.Parallel()

	for _, envelopeType := range []string{"hello", "disconnect"} {
		_, ok, err := eventFromSocketModeEnvelope(socketModeEnvelope{Type: envelopeType})
		if err != nil {
			t.Fatalf("eventFromSocketModeEnvelope(%q) error = %v", envelopeType, err)
		}
		if ok {
			t.Fatalf("eventFromSocketModeEnvelope(%q) ok = true, want false", envelopeType)
		}
	}
}
