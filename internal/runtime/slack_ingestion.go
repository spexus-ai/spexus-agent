package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spexus-ai/spexus-agent/internal/acpxadapter"
	"github.com/spexus-ai/spexus-agent/internal/registry"
	"github.com/spexus-ai/spexus-agent/internal/slack"
)

var ErrUnregisteredSlackChannel = errors.New("slack event from unregistered channel")

var ErrSlackEventThreadContextMissing = errors.New("slack event missing thread context")

type ProjectContextResolver interface {
	GetByChannelID(context.Context, string) (registry.Project, error)
}

type PreparedSlackEvent struct {
	Event         slack.Event      `json:"event"`
	Project       registry.Project `json:"project"`
	ThreadTS      string           `json:"threadTs"`
	SessionName   string           `json:"sessionName"`
	IsThreadReply bool             `json:"isThreadReply"`
	ThreadState   ThreadState      `json:"threadState"`
}

func PrepareSlackEvent(ctx context.Context, resolver ProjectContextResolver, event slack.Event) (PreparedSlackEvent, error) {
	if err := ctx.Err(); err != nil {
		return PreparedSlackEvent{}, err
	}
	if resolver == nil {
		return PreparedSlackEvent{}, errors.New("project context resolver is required")
	}
	if strings.TrimSpace(event.ChannelID) == "" {
		return PreparedSlackEvent{}, fmt.Errorf("slack event channel id is required")
	}

	project, err := resolver.GetByChannelID(ctx, event.ChannelID)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return PreparedSlackEvent{}, fmt.Errorf("%w: %s", ErrUnregisteredSlackChannel, event.ChannelID)
		}
		return PreparedSlackEvent{}, fmt.Errorf("resolve project context for channel %q: %w", event.ChannelID, err)
	}

	threadTS := strings.TrimSpace(event.ThreadTimestamp())
	if threadTS == "" {
		return PreparedSlackEvent{}, ErrSlackEventThreadContextMissing
	}

	sessionName := acpxadapter.SessionName(threadTS)
	state := ThreadState{
		ThreadTS:    threadTS,
		ChannelID:   event.ChannelID,
		ProjectName: project.Name,
		SessionName: sessionName,
	}

	return PreparedSlackEvent{
		Event:         event,
		Project:       project,
		ThreadTS:      threadTS,
		SessionName:   sessionName,
		IsThreadReply: event.IsThreadReply(),
		ThreadState:   state,
	}, nil
}
