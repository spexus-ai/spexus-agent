package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/spexus-ai/spexus-agent/internal/registry"
	"github.com/spexus-ai/spexus-agent/internal/slack"
)

type fakeProjectContextResolver struct {
	project registry.Project
	err     error
	calls   int
}

func (f *fakeProjectContextResolver) GetByChannelID(context.Context, string) (registry.Project, error) {
	f.calls++
	if f.err != nil {
		return registry.Project{}, f.err
	}
	return f.project, nil
}

// Test: registered channel events resolve the project context and prepare thread/session metadata without invoking ACPX.
// Validates: AC-1789 (REQ-1150 - runtime resolves project context from the active registered channel binding), AC-1787 (REQ-1148 - root messages create or ensure a thread session)
func TestPrepareSlackEventResolvesRegisteredChannelContext(t *testing.T) {
	t.Parallel()

	resolver := &fakeProjectContextResolver{
		project: registry.Project{
			Name:             "alpha",
			LocalPath:        "/workspace/alpha",
			SlackChannelID:   "C12345678",
			SlackChannelName: "spexus-alpha",
		},
	}

	event, err := PrepareSlackEvent(context.Background(), resolver, slack.Event{
		ID:        "Ev123",
		ChannelID: "C12345678",
		Timestamp: "1713686400.000100",
		UserID:    "U123",
		Text:      "hello",
	})
	if err != nil {
		t.Fatalf("PrepareSlackEvent() error = %v", err)
	}

	if resolver.calls != 1 {
		t.Fatalf("GetByChannelID() calls = %d, want 1", resolver.calls)
	}
	if event.Project.Name != "alpha" {
		t.Fatalf("PrepareSlackEvent() project = %#v, want alpha", event.Project)
	}
	if event.ThreadTS != "1713686400.000100" {
		t.Fatalf("PrepareSlackEvent() thread ts = %q, want root timestamp", event.ThreadTS)
	}
	if event.SessionName != "slack-1713686400.000100" {
		t.Fatalf("PrepareSlackEvent() session name = %q, want derived slack session name", event.SessionName)
	}
	if event.IsThreadReply {
		t.Fatalf("PrepareSlackEvent() IsThreadReply = true, want false for root message")
	}
	if event.ThreadState.ThreadTS != event.ThreadTS || event.ThreadState.ProjectName != "alpha" {
		t.Fatalf("PrepareSlackEvent() thread state = %#v", event.ThreadState)
	}
}

// Test: thread replies preserve the same project context and session mapping so later orchestration can continue the thread.
// Validates: AC-1788 (REQ-1149 - replies continue the existing thread session), AC-1789 (REQ-1150 - runtime resolves project context from the channel binding)
func TestPrepareSlackEventResolvesThreadReplyContext(t *testing.T) {
	t.Parallel()

	resolver := &fakeProjectContextResolver{
		project: registry.Project{
			Name:             "alpha",
			LocalPath:        "/workspace/alpha",
			SlackChannelID:   "C12345678",
			SlackChannelName: "spexus-alpha",
		},
	}

	event, err := PrepareSlackEvent(context.Background(), resolver, slack.Event{
		ID:        "Ev124",
		ChannelID: "C12345678",
		ThreadTS:  "1713686400.000100",
		Timestamp: "1713686410.000200",
		UserID:    "U123",
		Text:      "reply",
	})
	if err != nil {
		t.Fatalf("PrepareSlackEvent() error = %v", err)
	}

	if !event.IsThreadReply {
		t.Fatalf("PrepareSlackEvent() IsThreadReply = false, want true for thread reply")
	}
	if event.ThreadTS != "1713686400.000100" {
		t.Fatalf("PrepareSlackEvent() thread ts = %q, want existing thread ts", event.ThreadTS)
	}
	if event.SessionName != "slack-1713686400.000100" {
		t.Fatalf("PrepareSlackEvent() session name = %q, want derived slack session name", event.SessionName)
	}
}

// Test: unregistered channels are rejected before the runtime can prepare any orchestration state.
// Validates: AC-1790 (REQ-1151 - runtime does not invoke ACPX for unregistered channels)
func TestPrepareSlackEventRejectsUnregisteredChannel(t *testing.T) {
	t.Parallel()

	resolver := &fakeProjectContextResolver{
		err: registry.ErrNotFound,
	}

	_, err := PrepareSlackEvent(context.Background(), resolver, slack.Event{
		ID:        "Ev125",
		ChannelID: "C99999999",
		Timestamp: "1713686400.000100",
		UserID:    "U123",
		Text:      "hello",
	})
	if !errors.Is(err, ErrUnregisteredSlackChannel) {
		t.Fatalf("PrepareSlackEvent() error = %v, want ErrUnregisteredSlackChannel", err)
	}
	if resolver.calls != 1 {
		t.Fatalf("GetByChannelID() calls = %d, want 1", resolver.calls)
	}
}

// Test: shared invocation preparation resolves registered slash commands by channel without requiring thread context.
// Validates: AC-1818 (REQ-1185 - slash invocations resolve the project by channel_id)
func TestPrepareSlackInvocationResolvesRegisteredSlashChannelContext(t *testing.T) {
	t.Parallel()

	resolver := &fakeProjectContextResolver{
		project: registry.Project{
			Name:             "alpha",
			LocalPath:        "/workspace/alpha",
			SlackChannelID:   "C12345678",
			SlackChannelName: "spexus-alpha",
		},
	}

	prepared, err := PrepareSlackInvocation(context.Background(), resolver, slack.InboundInvocation{
		SourceType:    slack.InboundSourceSlash,
		DeliveryID:    "3-fwdc2",
		ChannelID:     "C12345678",
		UserID:        "U123",
		CommandText:   "status",
		ResponseURL:   "https://hooks.slack.test/response",
		AckEnvelopeID: "3-fwdc2",
	})
	if err != nil {
		t.Fatalf("PrepareSlackInvocation() error = %v", err)
	}

	if resolver.calls != 1 {
		t.Fatalf("GetByChannelID() calls = %d, want 1", resolver.calls)
	}
	if prepared.Project.Name != "alpha" {
		t.Fatalf("PrepareSlackInvocation() project = %#v, want alpha", prepared.Project)
	}
	if prepared.ThreadTS != "" || prepared.SessionName != "" {
		t.Fatalf("PrepareSlackInvocation() thread/session = (%q, %q), want empty for slash", prepared.ThreadTS, prepared.SessionName)
	}
	if prepared.Invocation.SourceType != slack.InboundSourceSlash {
		t.Fatalf("PrepareSlackInvocation() source = %q, want slash", prepared.Invocation.SourceType)
	}
}

// Test: unregistered mention invocations produce a human-readable Slack message rejection before ACPX can start.
// Validates: AC-1821 (REQ-1190 - unregistered channels reject before execution starts), AC-1821 (REQ-1191 - mention rejections are regular Slack messages)
func TestPrepareSlackInvocationRejectsUnregisteredMentionChannel(t *testing.T) {
	t.Parallel()

	resolver := &fakeProjectContextResolver{err: registry.ErrNotFound}

	_, err := PrepareSlackInvocation(context.Background(), resolver, slack.InboundInvocation{
		SourceType:  slack.InboundSourceMention,
		DeliveryID:  "Ev126",
		ChannelID:   "C99999999",
		UserID:      "U123",
		CommandText: "<@Ubot> status",
		ThreadTS:    "1713686400.000100",
	})
	if !errors.Is(err, ErrUnregisteredSlackChannel) {
		t.Fatalf("PrepareSlackInvocation() error = %v, want ErrUnregisteredSlackChannel", err)
	}

	var rejectionErr *RejectedSlackInvocationError
	if !errors.As(err, &rejectionErr) {
		t.Fatalf("PrepareSlackInvocation() error = %v, want RejectedSlackInvocationError", err)
	}
	if rejectionErr.Rejection.Ephemeral {
		t.Fatalf("mention rejection = %#v, want non-ephemeral rejection", rejectionErr.Rejection)
	}
	if rejectionErr.Rejection.ThreadTS != "1713686400.000100" {
		t.Fatalf("mention rejection thread ts = %q, want invocation thread ts", rejectionErr.Rejection.ThreadTS)
	}
	if rejectionErr.Rejection.Message == "" {
		t.Fatalf("mention rejection message = %q, want human-readable message", rejectionErr.Rejection.Message)
	}
}

// Test: unregistered slash invocations produce an ephemeral rejection contract before ACPX can start.
// Validates: AC-1822 (REQ-1190 - unregistered channels reject before execution starts), AC-1822 (REQ-1192 - slash rejections are ephemeral)
func TestPrepareSlackInvocationRejectsUnregisteredSlashChannel(t *testing.T) {
	t.Parallel()

	resolver := &fakeProjectContextResolver{err: registry.ErrNotFound}

	_, err := PrepareSlackInvocation(context.Background(), resolver, slack.InboundInvocation{
		SourceType:    slack.InboundSourceSlash,
		DeliveryID:    "3-fwdc2",
		ChannelID:     "C99999999",
		UserID:        "U123",
		CommandText:   "status",
		ResponseURL:   "https://hooks.slack.test/response",
		AckEnvelopeID: "3-fwdc2",
	})
	if !errors.Is(err, ErrUnregisteredSlackChannel) {
		t.Fatalf("PrepareSlackInvocation() error = %v, want ErrUnregisteredSlackChannel", err)
	}

	var rejectionErr *RejectedSlackInvocationError
	if !errors.As(err, &rejectionErr) {
		t.Fatalf("PrepareSlackInvocation() error = %v, want RejectedSlackInvocationError", err)
	}
	if !rejectionErr.Rejection.Ephemeral {
		t.Fatalf("slash rejection = %#v, want ephemeral rejection", rejectionErr.Rejection)
	}
	if rejectionErr.Rejection.ResponseURL != "https://hooks.slack.test/response" {
		t.Fatalf("slash rejection response url = %q, want response url", rejectionErr.Rejection.ResponseURL)
	}
	if rejectionErr.Rejection.ThreadTS != "" {
		t.Fatalf("slash rejection thread ts = %q, want empty", rejectionErr.Rejection.ThreadTS)
	}
}

// Test: root app_mention invocations strip the leading mention token and keep the root message timestamp as the execution thread anchor.
// Validates: AC-1815 (REQ-1181 - root mentions start a new thread-oriented execution context), AC-1815 (REQ-1183 - mention command text is parsed from the payload)
func TestPrepareSlackMentionEventResolvesRootMentionCommand(t *testing.T) {
	t.Parallel()

	resolver := &fakeProjectContextResolver{
		project: registry.Project{
			Name:             "alpha",
			LocalPath:        "/workspace/alpha",
			SlackChannelID:   "C12345678",
			SlackChannelName: "spexus-alpha",
		},
	}

	prepared, err := PrepareSlackMentionEvent(context.Background(), resolver, slack.InboundInvocation{
		SourceType:  slack.InboundSourceMention,
		DeliveryID:  "Ev127",
		ChannelID:   "C12345678",
		UserID:      "U123",
		CommandText: "<@Ubot> status",
		ThreadTS:    "1713686400.000100",
	})
	if err != nil {
		t.Fatalf("PrepareSlackMentionEvent() error = %v", err)
	}

	if prepared.ThreadTS != "1713686400.000100" {
		t.Fatalf("PrepareSlackMentionEvent() thread ts = %q, want root anchor", prepared.ThreadTS)
	}
	if prepared.Event.Text != "status" {
		t.Fatalf("PrepareSlackMentionEvent() text = %q, want stripped command text", prepared.Event.Text)
	}
	if prepared.SessionName != "slack-1713686400.000100" {
		t.Fatalf("PrepareSlackMentionEvent() session name = %q, want derived slack session name", prepared.SessionName)
	}
}

// Test: threaded app_mention invocations reuse the existing thread anchor and parse the command body after the mention token.
// Validates: AC-1816 (REQ-1182 - threaded mentions continue the existing Slack thread), AC-1816 (REQ-1183 - mention command text is parsed from the payload)
func TestPrepareSlackMentionEventResolvesThreadedMentionCommand(t *testing.T) {
	t.Parallel()

	resolver := &fakeProjectContextResolver{
		project: registry.Project{
			Name:             "alpha",
			LocalPath:        "/workspace/alpha",
			SlackChannelID:   "C12345678",
			SlackChannelName: "spexus-alpha",
		},
	}

	prepared, err := PrepareSlackMentionEvent(context.Background(), resolver, slack.InboundInvocation{
		SourceType:  slack.InboundSourceMention,
		DeliveryID:  "Ev128",
		ChannelID:   "C12345678",
		UserID:      "U123",
		CommandText: "<@Ubot> ask summarize current project state",
		ThreadTS:    "1713686400.000100",
	})
	if err != nil {
		t.Fatalf("PrepareSlackMentionEvent() error = %v", err)
	}

	if prepared.ThreadTS != "1713686400.000100" {
		t.Fatalf("PrepareSlackMentionEvent() thread ts = %q, want existing thread anchor", prepared.ThreadTS)
	}
	if prepared.Event.Text != "ask summarize current project state" {
		t.Fatalf("PrepareSlackMentionEvent() text = %q, want stripped threaded command", prepared.Event.Text)
	}
}

// Test: empty app_mention invocations normalize to an empty command string so runtime can render usage without crashing.
// Validates: AC-1817 (REQ-1184 - empty mention invocations return usage-oriented guidance)
func TestPrepareSlackMentionEventLeavesEmptyCommandForUsageHandling(t *testing.T) {
	t.Parallel()

	resolver := &fakeProjectContextResolver{
		project: registry.Project{
			Name:             "alpha",
			LocalPath:        "/workspace/alpha",
			SlackChannelID:   "C12345678",
			SlackChannelName: "spexus-alpha",
		},
	}

	prepared, err := PrepareSlackMentionEvent(context.Background(), resolver, slack.InboundInvocation{
		SourceType:  slack.InboundSourceMention,
		DeliveryID:  "Ev129",
		ChannelID:   "C12345678",
		UserID:      "U123",
		CommandText: "<@Ubot>",
		ThreadTS:    "1713686400.000100",
	})
	if err != nil {
		t.Fatalf("PrepareSlackMentionEvent() error = %v", err)
	}

	if prepared.Event.Text != "" {
		t.Fatalf("PrepareSlackMentionEvent() text = %q, want empty command", prepared.Event.Text)
	}
	if prepared.ThreadTS != "1713686400.000100" {
		t.Fatalf("PrepareSlackMentionEvent() thread ts = %q, want existing thread anchor", prepared.ThreadTS)
	}
}
