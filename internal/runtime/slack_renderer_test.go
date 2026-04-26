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

// Test: non-terminal progress events are batched into one Slack thread update while the completed assistant answer stays in the final message.
// Validates: AC-1979 (REQ-1426 - Slack progress publishing batches updates), AC-1980 (REQ-1427 - terminal success publishes the final answer)
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
	if client.messages[0].Text != "Progress:\n- Session started: slack-1713686400.000100\n- Thinking: analyzing\n- Tool started: grep - searching" {
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

func TestSlackThreadProgressPublisherFlushesBatchedProgressByCount(t *testing.T) {
	t.Parallel()

	client := &fakeThreadMessageClient{}
	renderer := SlackThreadRenderer{Client: client}
	publisher, err := renderer.NewProgressPublisher(SlackThreadRenderRequest{
		ChannelID:   "C12345678",
		ThreadTS:    "1713686400.000100",
		SessionName: "slack-1713686400.000100",
	}, SlackThreadProgressPublisherConfig{
		FlushEventCount: 2,
	})
	if err != nil {
		t.Fatalf("NewProgressPublisher() error = %v", err)
	}

	if err := publisher.Consume(context.Background(), ACPXTurnEvent{Kind: ACPXEventSessionStarted, Text: "slack-1713686400.000100"}); err != nil {
		t.Fatalf("Consume(session started) error = %v", err)
	}
	if publisher.ShouldFlushByCount() {
		t.Fatal("ShouldFlushByCount() = true after one event, want false")
	}
	if err := publisher.Consume(context.Background(), ACPXTurnEvent{Kind: ACPXEventToolStarted, ToolName: "grep", Text: "searching"}); err != nil {
		t.Fatalf("Consume(tool started) error = %v", err)
	}
	if !publisher.ShouldFlushByCount() {
		t.Fatal("ShouldFlushByCount() = false after two events, want true")
	}
	if err := publisher.Flush(context.Background()); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	if err := publisher.Consume(context.Background(), ACPXTurnEvent{Kind: ACPXEventAssistantMessageFinal, Text: "final answer"}); err != nil {
		t.Fatalf("Consume(final) error = %v", err)
	}
	if err := publisher.Finish(context.Background(), nil); err != nil {
		t.Fatalf("Finish() error = %v", err)
	}

	if got, want := len(client.messages), 2; got != want {
		t.Fatalf("message count = %d, want %d", got, want)
	}
	if client.messages[0].Text != "Progress:\n- Session started: slack-1713686400.000100\n- Tool started: grep - searching" {
		t.Fatalf("progress message = %q", client.messages[0].Text)
	}
	if client.messages[1].Text != "final answer" {
		t.Fatalf("final message = %q, want final answer", client.messages[1].Text)
	}
}

// Test: progress stays buffered until a flush boundary is reached, then the final assistant output is posted separately.
// Validates: AC-1978 (REQ-1425 - intermediate ACPX events publish before completion), AC-1979 (REQ-1426 - Slack progress publishing batches updates), AC-1980 (REQ-1427 - terminal success publishes the final answer)
func TestSlackThreadProgressPublisherBuffersProgressUntilFinalOutput(t *testing.T) {
	t.Parallel()

	client := &fakeThreadMessageClient{}
	renderer := SlackThreadRenderer{Client: client}
	publisher, err := renderer.NewProgressPublisher(SlackThreadRenderRequest{
		ChannelID:   "C12345678",
		ThreadTS:    "1713686400.000100",
		SessionName: "slack-1713686400.000100",
	}, SlackThreadProgressPublisherConfig{
		FlushEventCount: 10,
	})
	if err != nil {
		t.Fatalf("NewProgressPublisher() error = %v", err)
	}

	if err := publisher.Consume(context.Background(), ACPXTurnEvent{Kind: ACPXEventSessionStarted, Text: "slack-1713686400.000100"}); err != nil {
		t.Fatalf("Consume(session started) error = %v", err)
	}
	if err := publisher.Consume(context.Background(), ACPXTurnEvent{Kind: ACPXEventToolStarted, ToolName: "grep", Text: "searching"}); err != nil {
		t.Fatalf("Consume(tool started) error = %v", err)
	}
	if got := len(client.messages); got != 0 {
		t.Fatalf("message count before flush boundary = %d, want 0", got)
	}

	if err := publisher.Consume(context.Background(), ACPXTurnEvent{Kind: ACPXEventAssistantMessageFinal, Text: "final answer"}); err != nil {
		t.Fatalf("Consume(final) error = %v", err)
	}
	if err := publisher.Flush(context.Background()); err != nil {
		t.Fatalf("Flush() after final error = %v", err)
	}
	if got, want := len(client.messages), 1; got != want {
		t.Fatalf("message count after final flush = %d, want %d", got, want)
	}
	if client.messages[0].Text != "Progress:\n- Session started: slack-1713686400.000100\n- Tool started: grep - searching" {
		t.Fatalf("progress message = %q", client.messages[0].Text)
	}

	if err := publisher.Finish(context.Background(), nil); err != nil {
		t.Fatalf("Finish() error = %v", err)
	}
	if got, want := len(client.messages), 2; got != want {
		t.Fatalf("message count after finish = %d, want %d", got, want)
	}
	if client.messages[1].Text != "final answer" {
		t.Fatalf("final message = %q, want final answer", client.messages[1].Text)
	}
}

func TestSlackThreadProgressPublisherFinishesWithTerminalError(t *testing.T) {
	t.Parallel()

	client := &fakeThreadMessageClient{}
	renderer := SlackThreadRenderer{Client: client}
	publisher, err := renderer.NewProgressPublisher(SlackThreadRenderRequest{
		ChannelID:   "C12345678",
		ThreadTS:    "1713686400.000100",
		SessionName: "slack-1713686400.000100",
	}, SlackThreadProgressPublisherConfig{})
	if err != nil {
		t.Fatalf("NewProgressPublisher() error = %v", err)
	}

	if err := publisher.Consume(context.Background(), ACPXTurnEvent{Kind: ACPXEventAssistantMessageChunk, Text: "working"}); err != nil {
		t.Fatalf("Consume(chunk) error = %v", err)
	}
	if err := publisher.Finish(context.Background(), errors.New("acpx crashed")); err != nil {
		t.Fatalf("Finish() error = %v", err)
	}

	if got, want := len(client.messages), 2; got != want {
		t.Fatalf("message count = %d, want %d", got, want)
	}
	if client.messages[0].Text != "Progress:\n- working" {
		t.Fatalf("progress message = %q", client.messages[0].Text)
	}
	if client.messages[1].Text != "Session error: acpx crashed" {
		t.Fatalf("terminal message = %q, want session error", client.messages[1].Text)
	}
}

// Test: terminal cancellations flush any buffered progress and publish a cancellation status in the same Slack thread.
// Validates: AC-1978 (REQ-1425 - buffered progress is published before terminal completion), REQ-1428 clause coverage (terminal cancellation publishes the final status)
func TestSlackThreadProgressPublisherFinishesWithTerminalCancellation(t *testing.T) {
	t.Parallel()

	client := &fakeThreadMessageClient{}
	renderer := SlackThreadRenderer{Client: client}
	publisher, err := renderer.NewProgressPublisher(SlackThreadRenderRequest{
		ChannelID:   "C12345678",
		ThreadTS:    "1713686400.000100",
		SessionName: "slack-1713686400.000100",
	}, SlackThreadProgressPublisherConfig{})
	if err != nil {
		t.Fatalf("NewProgressPublisher() error = %v", err)
	}

	if err := publisher.Consume(context.Background(), ACPXTurnEvent{Kind: ACPXEventAssistantMessageChunk, Text: "working"}); err != nil {
		t.Fatalf("Consume(chunk) error = %v", err)
	}
	if err := publisher.Consume(context.Background(), ACPXTurnEvent{Kind: ACPXEventSessionCancelled, Text: "cancelled by operator"}); err != nil {
		t.Fatalf("Consume(cancelled) error = %v", err)
	}
	if err := publisher.Finish(context.Background(), nil); err != nil {
		t.Fatalf("Finish() error = %v", err)
	}

	if got, want := len(client.messages), 2; got != want {
		t.Fatalf("message count = %d, want %d", got, want)
	}
	if client.messages[0].Text != "Progress:\n- working" {
		t.Fatalf("progress message = %q", client.messages[0].Text)
	}
	if client.messages[1].Text != "Session cancelled: cancelled by operator" {
		t.Fatalf("terminal message = %q, want cancellation status", client.messages[1].Text)
	}
}
