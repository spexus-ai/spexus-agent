package slack

import "testing"

func TestEventFromSocketModeEnvelopeNormalizesMessageEvent(t *testing.T) {
	envelope := socketModeEnvelope{
		Type: "events_api",
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
	envelope := socketModeEnvelope{
		Type: "events_api",
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
	envelope := socketModeEnvelope{
		Type: "events_api",
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
	envelope := socketModeEnvelope{
		Type:    "events_api",
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
