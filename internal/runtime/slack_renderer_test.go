package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/spexus-ai/spexus-agent/internal/slack"
)

type fakeThreadMessageClient struct {
	messages []slack.Message
	err      error
}

func (f *fakeThreadMessageClient) PostMessage(_ context.Context, message slack.Message) (slack.PostedMessage, error) {
	if f.err != nil {
		return slack.PostedMessage{}, f.err
	}
	return slack.PostedMessage{ChannelID: message.ChannelID, Timestamp: "1713686400.000100"}, nil
}

func (f *fakeThreadMessageClient) PostThreadMessage(_ context.Context, message slack.Message) error {
	f.messages = append(f.messages, message)
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

// Test: progress events are batched into one Slack thread update and the final assistant response is posted separately.
// Validates: AC-1787 (REQ-1148 - root Slack messages create or ensure a thread session), AC-1788 (REQ-1149 - thread replies continue the existing thread session)
func TestSlackThreadRendererBatchesProgressAndFinalUpdates(t *testing.T) {
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
		{Kind: ACPXEventAssistantMessageChunk, Text: "partial answer"},
		{Kind: ACPXEventAssistantMessageFinal, Text: "final answer"},
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	if got, want := len(client.messages), 2; got != want {
		t.Fatalf("PostThreadMessage() calls = %d, want %d", got, want)
	}
	if client.messages[0].Text != "Progress:\n- Session started: slack-1713686400.000100\n- Thinking: analyzing\n- Tool started: grep - searching\n- partial answer" {
		t.Fatalf("first message text = %q", client.messages[0].Text)
	}
	if client.messages[1].Text != "final answer" {
		t.Fatalf("final message text = %q, want final answer", client.messages[1].Text)
	}
	if client.messages[0].ThreadTS != "1713686400.000100" || client.messages[1].ThreadTS != "1713686400.000100" {
		t.Fatalf("thread timestamps = %#v %#v", client.messages[0], client.messages[1])
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
