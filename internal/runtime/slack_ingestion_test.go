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
