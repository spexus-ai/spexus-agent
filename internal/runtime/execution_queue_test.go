package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/registry"
	"github.com/spexus-ai/spexus-agent/internal/slack"
)

// Test: accepted mention invocations are converted into an execution request that preserves normalized thread and session context.
// Validates: AC-1975 (REQ-1418 - accepted invocations create an execution request before async processing)
func TestNewMentionExecutionRequestRoundTripsPreparedEvent(t *testing.T) {
	t.Parallel()

	prepared := PreparedSlackEvent{
		SourceType:  slack.InboundSourceMention,
		DeliveryID:  "Ev123",
		ThreadTS:    "1713686400.000100",
		SessionName: "slack-1713686400.000100",
		Event: slack.Event{
			ID:        "Ev123",
			ChannelID: "C123",
			ThreadTS:  "1713686400.000100",
			Timestamp: "1713686400.000100",
			UserID:    "U123",
			Text:      "ask summarize project status",
		},
		Project: registry.Project{
			Name:           "alpha",
			LocalPath:      "/workspace/alpha",
			SlackChannelID: "C123",
		},
		ThreadState: ThreadState{
			ThreadTS:    "1713686400.000100",
			ChannelID:   "C123",
			ProjectName: "alpha",
			SessionName: "slack-1713686400.000100",
		},
	}

	request := NewMentionExecutionRequest(prepared)
	if request.CommandText != "ask summarize project status" {
		t.Fatalf("NewMentionExecutionRequest() command text = %q, want mention command text", request.CommandText)
	}

	roundTrip, err := request.PreparedEvent()
	if err != nil {
		t.Fatalf("PreparedEvent() error = %v", err)
	}
	if roundTrip.ThreadTS != prepared.ThreadTS || roundTrip.SessionName != prepared.SessionName {
		t.Fatalf("PreparedEvent() thread/session = (%q, %q), want (%q, %q)", roundTrip.ThreadTS, roundTrip.SessionName, prepared.ThreadTS, prepared.SessionName)
	}
	if roundTrip.Project.Name != "alpha" || roundTrip.Event.Text != "ask summarize project status" {
		t.Fatalf("PreparedEvent() = %#v, want preserved project and command text", roundTrip)
	}
}

// Test: slash execution requests can be enqueued before a thread exists and acquire thread/session context later.
// Validates: AC-1975 (REQ-1418 - execution requests support async queueing before worker-owned thread creation)
func TestSlashExecutionRequestWithThreadBuildsPreparedEvent(t *testing.T) {
	t.Parallel()

	request := NewSlashExecutionRequest(PreparedSlackInvocation{
		Invocation: slack.InboundInvocation{
			SourceType:  slack.InboundSourceSlash,
			DeliveryID:  "3-fwdc2",
			ChannelID:   "C123",
			UserID:      "U123",
			CommandText: "status",
		},
		Project: registry.Project{
			Name:           "alpha",
			LocalPath:      "/workspace/alpha",
			SlackChannelID: "C123",
		},
	})
	if request.ThreadTS != "" || request.SessionName != "" {
		t.Fatalf("NewSlashExecutionRequest() thread/session = (%q, %q), want empty before worker thread creation", request.ThreadTS, request.SessionName)
	}

	prepared, err := request.WithThread("1713686400.000100").PreparedEvent()
	if err != nil {
		t.Fatalf("PreparedEvent() error = %v", err)
	}
	if prepared.ThreadTS != "1713686400.000100" {
		t.Fatalf("PreparedEvent() thread ts = %q, want worker thread timestamp", prepared.ThreadTS)
	}
	if prepared.SessionName != "slack-1713686400.000100" {
		t.Fatalf("PreparedEvent() session name = %q, want derived session name", prepared.SessionName)
	}
}

// Test: enqueue returns before the async handler completes so ingress can continue without waiting for execution.
// Validates: AC-1975 (REQ-1417 - ingress does not wait for ACPX completion), AC-1975 (REQ-1419 - ingress returns control to the intake loop immediately)
func TestAsyncExecutionQueueEnqueueReturnsBeforeHandlerCompletes(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	queue := NewAsyncExecutionQueue(func(context.Context, ExecutionRequest) {
		close(started)
		<-release
	})

	request := ExecutionRequest{
		SourceType:  slack.InboundSourceMention,
		DeliveryID:  "Ev123",
		ChannelID:   "C123",
		CommandText: "status",
		Project: registry.Project{
			Name:           "alpha",
			LocalPath:      "/workspace/alpha",
			SlackChannelID: "C123",
		},
		ThreadTS:    "1713686400.000100",
		SessionName: "slack-1713686400.000100",
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- queue.Enqueue(context.Background(), request)
	}()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Enqueue() error = %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Enqueue() blocked on async handler completion")
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("async handler did not start")
	}

	close(release)
}
