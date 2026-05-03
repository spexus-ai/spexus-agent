package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spexus-ai/spexus-agent/internal/slack"
)

type fakeThreadMessageClient struct {
	messages []slack.Message
	updates  []slack.MessageUpdate
	err      error
}

func (f *fakeThreadMessageClient) PostMessage(_ context.Context, message slack.Message) (slack.PostedMessage, error) {
	if f.err != nil {
		return slack.PostedMessage{}, f.err
	}
	f.messages = append(f.messages, message)
	return slack.PostedMessage{
		ChannelID: message.ChannelID,
		Timestamp: "1713686400.00010" + string(rune('0'+len(f.messages))),
	}, nil
}

func (f *fakeThreadMessageClient) PostThreadMessage(_ context.Context, message slack.Message) error {
	f.messages = append(f.messages, message)
	if f.err != nil {
		return f.err
	}
	return nil
}

func (f *fakeThreadMessageClient) UpdateMessage(_ context.Context, update slack.MessageUpdate) error {
	f.updates = append(f.updates, update)
	if f.err != nil {
		return f.err
	}
	return nil
}

func (f *fakeThreadMessageClient) CreateChannel(context.Context, slack.CreateChannelRequest) (slack.Channel, error) {
	return slack.Channel{}, errors.New("not implemented")
}

func (f *fakeThreadMessageClient) FindChannelByName(context.Context, string) (slack.Channel, error) {
	return slack.Channel{}, errors.New("not implemented")
}

func (f *fakeThreadMessageClient) Close() error { return nil }

// Test: user-visible progress events are batched into one Slack thread update and tool events are omitted.
// Validates: AC-1787 (REQ-1148 - root Slack messages create or ensure a thread session), AC-1788 (REQ-1149 - thread replies continue the existing thread session)
func TestSlackThreadRendererBatchesProgressFiltersToolsAndPostsFinal(t *testing.T) {
	t.Parallel()

	client := &fakeThreadMessageClient{}
	renderer := SlackThreadRenderer{Client: client}

	err := renderer.Render(context.Background(), SlackThreadRenderRequest{
		ChannelID:   "C12345678",
		ThreadTS:    "1713686400.000100",
		SessionName: "slack-1713686400.000100",
	}, []ACPXTurnEvent{
		{Kind: ACPXEventSessionStarted, Text: "slack-1713686400.000100"},
		{Kind: ACPXEventAssistantThinking, Text: "analyzing"},
		{Kind: ACPXEventToolStarted, ToolName: "grep", Text: "searching"},
		{Kind: ACPXEventToolFinished, ToolName: "grep", Text: "done"},
		{Kind: ACPXEventAssistantMessageChunk, Text: "partial answer"},
		{Kind: ACPXEventAssistantMessageFinal, Text: "final answer"},
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	if got, want := len(client.messages), 2; got != want {
		t.Fatalf("PostThreadMessage() calls = %d, want %d", got, want)
	}
	if client.messages[0].Text != "Progress:\n- Session started: slack-1713686400.000100\n- Thinking: analyzing\n- partial answer" {
		t.Fatalf("first message text = %q", client.messages[0].Text)
	}
	if client.messages[1].Text != "final answer" {
		t.Fatalf("final message text = %q, want final answer", client.messages[1].Text)
	}
	if client.messages[0].ThreadTS != "1713686400.000100" || client.messages[1].ThreadTS != "1713686400.000100" {
		t.Fatalf("thread timestamps = %#v %#v", client.messages[0], client.messages[1])
	}
}

// Test: tool-only progress is suppressed so noisy file-read updates cannot grow Slack messages past API limits.
func TestSlackThreadRendererSuppressesToolOnlyProgress(t *testing.T) {
	t.Parallel()

	client := &fakeThreadMessageClient{}
	renderer := SlackThreadRenderer{Client: client}

	err := renderer.Render(context.Background(), SlackThreadRenderRequest{
		ChannelID:   "C12345678",
		ThreadTS:    "1713686400.000100",
		SessionName: "slack-1713686400.000100",
	}, []ACPXTurnEvent{
		{Kind: ACPXEventToolStarted, ToolName: "Read search_service.go"},
		{Kind: ACPXEventToolFinished, ToolName: "Read search_service.go"},
		{Kind: ACPXEventSessionDone},
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	if got, want := len(client.messages), 1; got != want {
		t.Fatalf("PostThreadMessage() calls = %d, want %d", got, want)
	}
	if client.messages[0].Text != "Session complete." {
		t.Fatalf("final message text = %q, want session complete", client.messages[0].Text)
	}
}

// Test: terminal ACPX errors are rendered after any pending progress batch and use the same Slack thread context.
// Validates: AC-1795 (REQ-1156 - cancel requests cancellation without corrupting session mapping)
func TestSlackThreadRendererRendersTerminalError(t *testing.T) {
	t.Parallel()

	client := &fakeThreadMessageClient{}
	renderer := SlackThreadRenderer{Client: client}

	err := renderer.Render(context.Background(), SlackThreadRenderRequest{
		ChannelID:   "C12345678",
		ThreadTS:    "1713686400.000100",
		SessionName: "slack-1713686400.000100",
	}, []ACPXTurnEvent{
		{Kind: ACPXEventAssistantMessageChunk, Text: "working"},
		{Kind: ACPXEventSessionError, Text: "acpx crashed"},
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	if got, want := len(client.messages), 2; got != want {
		t.Fatalf("PostThreadMessage() calls = %d, want %d", got, want)
	}
	if client.messages[0].Text != "Progress:\n- working" {
		t.Fatalf("progress render text = %q", client.messages[0].Text)
	}
	if client.messages[1].Text != "Session error: acpx crashed" {
		t.Fatalf("terminal render text = %q", client.messages[1].Text)
	}
}

// Test: cancel rendering posts a terminal Slack update after progress is flushed without changing the thread/session context.
// Validates: AC-1795 (REQ-1156 - cancel requests cancellation without corrupting session mapping)
func TestSlackThreadRendererRendersCancel(t *testing.T) {
	t.Parallel()

	client := &fakeThreadMessageClient{}
	renderer := SlackThreadRenderer{Client: client}

	err := renderer.Render(context.Background(), SlackThreadRenderRequest{
		ChannelID:   "C12345678",
		ThreadTS:    "1713686400.000100",
		SessionName: "slack-1713686400.000100",
	}, []ACPXTurnEvent{
		{Kind: ACPXEventAssistantMessageChunk, Text: "working"},
		{Kind: ACPXEventSessionCancelled, Text: "cancelled by operator"},
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	if got, want := len(client.messages), 2; got != want {
		t.Fatalf("PostThreadMessage() calls = %d, want %d", got, want)
	}
	if client.messages[0].Text != "Progress:\n- working" {
		t.Fatalf("progress render text = %q", client.messages[0].Text)
	}
	if client.messages[1].Text != "Session cancelled: cancelled by operator" {
		t.Fatalf("cancel render text = %q", client.messages[1].Text)
	}
}

// Test: stream rendering posts append-only chunks on newline boundaries or chunk rollover.
func TestSlackThreadStreamPostsNewMessagesOnNewlineThenRollsToNextChunk(t *testing.T) {
	t.Parallel()

	client := &fakeThreadMessageClient{}
	renderer := SlackThreadRenderer{Client: client}
	stream, err := renderer.NewStream(context.Background(), SlackThreadRenderRequest{
		ChannelID:   "C12345678",
		ThreadTS:    "1713686400.000100",
		SessionName: "slack-1713686400.000100",
	})
	if err != nil {
		t.Fatalf("NewStream() error = %v", err)
	}

	first := strings.Repeat("a", 10)
	if err := stream.Render(context.Background(), []ACPXTurnEvent{{Kind: ACPXEventAssistantMessageFinal, Text: first}}); err != nil {
		t.Fatalf("Render(first) error = %v", err)
	}
	second := first + strings.Repeat("b", 10)
	if err := stream.Render(context.Background(), []ACPXTurnEvent{{Kind: ACPXEventAssistantMessageFinal, Text: second}}); err != nil {
		t.Fatalf("Render(second) error = %v", err)
	}
	withNewline := second + "\nnext line"
	if err := stream.Render(context.Background(), []ACPXTurnEvent{{Kind: ACPXEventAssistantMessageFinal, Text: withNewline}}); err != nil {
		t.Fatalf("Render(withNewline) error = %v", err)
	}
	large := withNewline + strings.Repeat("x", streamingSlackMessageSoftLimit) + "tail"
	if err := stream.Render(context.Background(), []ACPXTurnEvent{{Kind: ACPXEventAssistantMessageFinal, Text: large}}); err != nil {
		t.Fatalf("Render(large) error = %v", err)
	}

	if got, want := len(client.messages), 2; got != want {
		t.Fatalf("PostMessage() calls = %d, want %d", got, want)
	}
	if got := len(client.updates); got != 0 {
		t.Fatalf("UpdateMessage() calls = %d, want 0", got)
	}
	if client.messages[0].Text != second {
		t.Fatalf("first post text = %q, want newline-delimited partial", client.messages[0].Text)
	}
	if len([]rune(client.messages[1].Text)) != streamingSlackMessageSoftLimit {
		t.Fatalf("rolled second chunk length = %d, want soft limit", len([]rune(client.messages[1].Text)))
	}
	if strings.Contains(client.messages[1].Text, "tail") {
		t.Fatalf("second post included unsent tail: %q", client.messages[1].Text)
	}
}

func TestSlackThreadStreamPostsTerminalTailEvenWithoutNewline(t *testing.T) {
	t.Parallel()

	client := &fakeThreadMessageClient{}
	renderer := SlackThreadRenderer{Client: client}
	stream, err := renderer.NewStream(context.Background(), SlackThreadRenderRequest{
		ChannelID:   "C12345678",
		ThreadTS:    "1713686400.000100",
		SessionName: "slack-1713686400.000100",
	})
	if err != nil {
		t.Fatalf("NewStream() error = %v", err)
	}

	if err := stream.Render(context.Background(), []ACPXTurnEvent{{Kind: ACPXEventAssistantMessageFinal, Text: "partial"}}); err != nil {
		t.Fatalf("Render(partial) error = %v", err)
	}
	if err := stream.Render(context.Background(), []ACPXTurnEvent{
		{Kind: ACPXEventAssistantMessageFinal, Text: "partial final"},
		{Kind: ACPXEventSessionDone},
	}); err != nil {
		t.Fatalf("Render(final) error = %v", err)
	}

	if got, want := len(client.messages), 1; got != want {
		t.Fatalf("PostMessage() calls = %d, want %d", got, want)
	}
	if got := len(client.updates); got != 0 {
		t.Fatalf("UpdateMessage() calls = %d, want 0", got)
	}
	if client.messages[0].Text != "partial final" {
		t.Fatalf("terminal message text = %q, want final text", client.messages[0].Text)
	}
}
