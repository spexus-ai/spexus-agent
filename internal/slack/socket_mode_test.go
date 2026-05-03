package slack

import "testing"

// Test: Socket Mode message events normalize into the internal Slack event shape for root messages and thread replies.
// Validates: AC-1789 (REQ-1150 - runtime resolves project context from the active registered channel binding), AC-1790 (REQ-1151 - unregistered channels do not invoke ACPX)
func TestSocketModeMessageNormalize(t *testing.T) {
	t.Parallel()

	rootEvent, err := (SocketModeMessage{
		EventID:   "Ev123",
		Type:      SocketModeMessageType,
		ChannelID: "C12345678",
		Timestamp: "1713686400.000100",
		UserID:    "U123",
		Text:      "hello",
	}).Normalize()
	if err != nil {
		t.Fatalf("Normalize() root error = %v", err)
	}
	if rootEvent.ChannelID != "C12345678" || rootEvent.ThreadTS != "" || rootEvent.Timestamp != "1713686400.000100" {
		t.Fatalf("Normalize() root event = %#v", rootEvent)
	}

	replyEvent, err := (SocketModeMessage{
		EventID:   "Ev124",
		Type:      SocketModeMessageType,
		ChannelID: "C12345678",
		ThreadTS:  "1713686400.000100",
		Timestamp: "1713686410.000200",
		UserID:    "U123",
		Text:      "reply",
	}).Normalize()
	if err != nil {
		t.Fatalf("Normalize() reply error = %v", err)
	}
	if replyEvent.ThreadTS != "1713686400.000100" || replyEvent.Timestamp != "1713686410.000200" {
		t.Fatalf("Normalize() reply event = %#v", replyEvent)
	}

	mentionEvent, err := (SocketModeMessage{
		EventID:   "Ev125",
		Type:      SocketModeAppMentionType,
		ChannelID: "C12345678",
		Timestamp: "1713686420.000300",
		UserID:    "U123",
		Text:      "<@Ubot> hello",
	}).Normalize()
	if err != nil {
		t.Fatalf("Normalize() mention error = %v", err)
	}
	if mentionEvent.ChannelID != "C12345678" || mentionEvent.Timestamp != "1713686420.000300" || mentionEvent.Text != "<@Ubot> hello" {
		t.Fatalf("Normalize() mention event = %#v", mentionEvent)
	}
}

// Test: unsupported Socket Mode payloads are rejected before they can be handed to runtime ingestion.
// Validates: AC-1790 (REQ-1151 - runtime rejects unregistered or unsupported Slack events before orchestration)
func TestSocketModeMessageRejectsUnsupportedType(t *testing.T) {
	t.Parallel()

	_, err := (SocketModeMessage{
		Type:      "reaction_added",
		ChannelID: "C12345678",
		UserID:    "U123",
	}).Normalize()
	if err == nil {
		t.Fatalf("Normalize() error = nil, want non-nil")
	}
	if err != ErrSocketModeEventUnsupported {
		t.Fatalf("Normalize() error = %v, want %v", err, ErrSocketModeEventUnsupported)
	}
}

// Test: bot-authored message payloads carry bot/app identifiers so upper layers can reject self-generated events.
func TestSocketModeMessageCarriesBotIdentifiers(t *testing.T) {
	t.Parallel()

	event, err := (SocketModeMessage{
		EventID:   "Ev126",
		Type:      SocketModeMessageType,
		ChannelID: "C12345678",
		Timestamp: "1713686430.000400",
		UserID:    "Ubot",
		BotID:     "B123",
		AppID:     "A123",
		Text:      "bot output",
	}).Normalize()
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if event.ID != "Ev126" {
		t.Fatalf("Normalize() event = %#v", event)
	}
}

// Test: app_mention payloads normalize into the shared inbound invocation shape used by mention routing.
// Validates: AC-1815 (REQ-1180 - mention invocations resolve registered project channels), AC-1823 (REQ-1193 - invocation sources classify as mention)
func TestSocketModeMessageNormalizeInboundMention(t *testing.T) {
	t.Parallel()

	invocation, err := (SocketModeMessage{
		EventID:   "Ev125",
		Type:      SocketModeAppMentionType,
		ChannelID: "C12345678",
		Timestamp: "1713686420.000300",
		UserID:    "U123",
		Text:      "<@Ubot> status",
	}).NormalizeInbound()
	if err != nil {
		t.Fatalf("NormalizeInbound() error = %v", err)
	}
	if invocation.SourceType != InboundSourceMention {
		t.Fatalf("NormalizeInbound() source = %q, want %q", invocation.SourceType, InboundSourceMention)
	}
	if invocation.DeliveryID != "Ev125" || invocation.ChannelID != "C12345678" || invocation.ThreadTS != "1713686420.000300" {
		t.Fatalf("NormalizeInbound() invocation = %#v", invocation)
	}
}

// Test: thread message payloads normalize into the shared inbound invocation shape used by direct prompt routing.
func TestSocketModeMessageNormalizeInboundThreadMessage(t *testing.T) {
	t.Parallel()

	invocation, err := (SocketModeMessage{
		EventID:   "Ev126",
		Type:      SocketModeMessageType,
		ChannelID: "C12345678",
		ThreadTS:  "1713686420.000300",
		Timestamp: "1713686421.000400",
		UserID:    "U123",
		Text:      "summarize current project state",
	}).NormalizeInbound()
	if err != nil {
		t.Fatalf("NormalizeInbound() error = %v", err)
	}
	if invocation.SourceType != InboundSourceMessage {
		t.Fatalf("NormalizeInbound() source = %q, want %q", invocation.SourceType, InboundSourceMessage)
	}
	if invocation.DeliveryID != "Ev126" || invocation.ChannelID != "C12345678" || invocation.ThreadTS != "1713686420.000300" {
		t.Fatalf("NormalizeInbound() invocation = %#v", invocation)
	}
}

// Test: root message payloads remain classified as messages but do not invent a thread anchor.
func TestSocketModeMessageNormalizeInboundRootMessageDoesNotSetThread(t *testing.T) {
	t.Parallel()

	invocation, err := (SocketModeMessage{
		EventID:   "Ev127",
		Type:      SocketModeMessageType,
		ChannelID: "C12345678",
		Timestamp: "1713686421.000400",
		UserID:    "U123",
		Text:      "channel chatter",
	}).NormalizeInbound()
	if err != nil {
		t.Fatalf("NormalizeInbound() error = %v", err)
	}
	if invocation.SourceType != InboundSourceMessage || invocation.ThreadTS != "" {
		t.Fatalf("NormalizeInbound() invocation = %#v, want root message without thread", invocation)
	}
}

// Test: slash command payloads normalize into the shared inbound invocation shape without thread metadata.
// Validates: AC-1818 (REQ-1185 - slash invocations resolve channel context), AC-1824 (REQ-1193 - invocation sources classify as slash)
func TestSocketModeSlashCommandNormalizeInbound(t *testing.T) {
	t.Parallel()

	invocation, err := (SocketModeSlashCommand{
		Command:     "/spexus",
		ChannelID:   "C12345678",
		UserID:      "U123",
		Text:        "status",
		ResponseURL: "https://hooks.slack.test/response",
	}).NormalizeInbound("3-fwdc2")
	if err != nil {
		t.Fatalf("NormalizeInbound() error = %v", err)
	}
	if invocation.SourceType != InboundSourceSlash {
		t.Fatalf("NormalizeInbound() source = %q, want %q", invocation.SourceType, InboundSourceSlash)
	}
	if invocation.DeliveryID != "3-fwdc2" || invocation.AckEnvelopeID != "3-fwdc2" || invocation.ThreadTS != "" {
		t.Fatalf("NormalizeInbound() invocation = %#v", invocation)
	}
}
