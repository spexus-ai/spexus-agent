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

type SlackInvocationRejection struct {
	SourceType  string `json:"sourceType"`
	ChannelID   string `json:"channelId"`
	ThreadTS    string `json:"threadTs,omitempty"`
	ResponseURL string `json:"responseUrl,omitempty"`
	Message     string `json:"message"`
	Ephemeral   bool   `json:"ephemeral,omitempty"`
}

type RejectedSlackInvocationError struct {
	Rejection SlackInvocationRejection
	cause     error
}

type PreparedSlackEvent struct {
	SourceType    string           `json:"sourceType"`
	DeliveryID    string           `json:"deliveryId"`
	Event         slack.Event      `json:"event"`
	Project       registry.Project `json:"project"`
	ThreadTS      string           `json:"threadTs"`
	SessionName   string           `json:"sessionName"`
	IsThreadReply bool             `json:"isThreadReply"`
	ThreadState   ThreadState      `json:"threadState"`
}

type PreparedSlackInvocation struct {
	Invocation    slack.InboundInvocation `json:"invocation"`
	Project       registry.Project        `json:"project"`
	ThreadTS      string                  `json:"threadTs,omitempty"`
	SessionName   string                  `json:"sessionName,omitempty"`
	IsThreadReply bool                    `json:"isThreadReply"`
	ThreadState   ThreadState             `json:"threadState"`
}

func (e *RejectedSlackInvocationError) Error() string {
	if e == nil {
		return ""
	}
	if e.cause == nil {
		return e.Rejection.Message
	}
	return e.cause.Error()
}

func (e *RejectedSlackInvocationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func PrepareSlackInvocation(ctx context.Context, resolver ProjectContextResolver, invocation slack.InboundInvocation) (PreparedSlackInvocation, error) {
	if err := ctx.Err(); err != nil {
		return PreparedSlackInvocation{}, err
	}
	if resolver == nil {
		return PreparedSlackInvocation{}, errors.New("project context resolver is required")
	}
	if strings.TrimSpace(invocation.ChannelID) == "" {
		return PreparedSlackInvocation{}, fmt.Errorf("slack invocation channel id is required")
	}

	project, err := resolver.GetByChannelID(ctx, invocation.ChannelID)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return PreparedSlackInvocation{}, &RejectedSlackInvocationError{
				Rejection: buildUnregisteredSlackInvocationRejection(invocation),
				cause:     fmt.Errorf("%w: %s", ErrUnregisteredSlackChannel, invocation.ChannelID),
			}
		}
		return PreparedSlackInvocation{}, fmt.Errorf("resolve project context for channel %q: %w", invocation.ChannelID, err)
	}

	prepared := PreparedSlackInvocation{
		Invocation: invocation,
		Project:    project,
	}

	if strings.TrimSpace(invocation.SourceType) != slack.InboundSourceSlash {
		threadTS := strings.TrimSpace(invocation.ThreadTS)
		if threadTS == "" {
			return PreparedSlackInvocation{}, ErrSlackEventThreadContextMissing
		}

		sessionName := acpxadapter.SessionName(threadTS)
		prepared.ThreadTS = threadTS
		prepared.SessionName = sessionName
		prepared.ThreadState = ThreadState{
			ThreadTS:    threadTS,
			ChannelID:   invocation.ChannelID,
			ProjectName: project.Name,
			SessionName: sessionName,
		}
	}

	return prepared, nil
}

func PrepareSlackEvent(ctx context.Context, resolver ProjectContextResolver, event slack.Event) (PreparedSlackEvent, error) {
	prepared, err := PrepareSlackInvocation(ctx, resolver, slack.InboundInvocation{
		SourceType:  slack.InboundSourceMention,
		DeliveryID:  strings.TrimSpace(event.ID),
		ChannelID:   strings.TrimSpace(event.ChannelID),
		UserID:      strings.TrimSpace(event.UserID),
		CommandText: event.Text,
		ThreadTS:    event.ThreadTimestamp(),
	})
	if err != nil {
		return PreparedSlackEvent{}, err
	}

	return PreparedSlackEvent{
		SourceType:    slack.InboundSourceMention,
		DeliveryID:    strings.TrimSpace(event.ID),
		Event:         event,
		Project:       prepared.Project,
		ThreadTS:      prepared.ThreadTS,
		SessionName:   prepared.SessionName,
		IsThreadReply: event.IsThreadReply(),
		ThreadState:   prepared.ThreadState,
	}, nil
}

func PrepareSlackMentionEvent(ctx context.Context, resolver ProjectContextResolver, invocation slack.InboundInvocation) (PreparedSlackEvent, error) {
	invocation.SourceType = slack.InboundSourceMention
	invocation.CommandText = extractMentionCommandText(invocation.CommandText)

	prepared, err := PrepareSlackInvocation(ctx, resolver, invocation)
	if err != nil {
		return PreparedSlackEvent{}, err
	}

	return PreparedSlackEvent{
		SourceType: prepared.Invocation.SourceType,
		DeliveryID: strings.TrimSpace(invocation.DeliveryID),
		Event: slack.Event{
			ID:        strings.TrimSpace(invocation.DeliveryID),
			ChannelID: strings.TrimSpace(invocation.ChannelID),
			ThreadTS:  prepared.ThreadTS,
			Timestamp: prepared.ThreadTS,
			UserID:    strings.TrimSpace(invocation.UserID),
			Text:      prepared.Invocation.CommandText,
		},
		Project:       prepared.Project,
		ThreadTS:      prepared.ThreadTS,
		SessionName:   prepared.SessionName,
		IsThreadReply: false,
		ThreadState:   prepared.ThreadState,
	}, nil
}

func PrepareSlackMessageEvent(ctx context.Context, resolver ProjectContextResolver, invocation slack.InboundInvocation) (PreparedSlackEvent, error) {
	invocation.SourceType = slack.InboundSourceMessage
	invocation.CommandText = strings.TrimSpace(invocation.CommandText)

	prepared, err := PrepareSlackInvocation(ctx, resolver, invocation)
	if err != nil {
		return PreparedSlackEvent{}, err
	}

	return PreparedSlackEvent{
		SourceType: prepared.Invocation.SourceType,
		DeliveryID: strings.TrimSpace(invocation.DeliveryID),
		Event: slack.Event{
			ID:        strings.TrimSpace(invocation.DeliveryID),
			ChannelID: strings.TrimSpace(invocation.ChannelID),
			ThreadTS:  prepared.ThreadTS,
			Timestamp: prepared.ThreadTS,
			UserID:    strings.TrimSpace(invocation.UserID),
			Text:      prepared.Invocation.CommandText,
		},
		Project:       prepared.Project,
		ThreadTS:      prepared.ThreadTS,
		SessionName:   prepared.SessionName,
		IsThreadReply: true,
		ThreadState:   prepared.ThreadState,
	}, nil
}

func buildUnregisteredSlackInvocationRejection(invocation slack.InboundInvocation) SlackInvocationRejection {
	rejection := SlackInvocationRejection{
		SourceType: strings.TrimSpace(invocation.SourceType),
		ChannelID:  strings.TrimSpace(invocation.ChannelID),
		Message:    "This Slack channel is not registered in the project registry.",
	}

	if rejection.SourceType == slack.InboundSourceSlash {
		rejection.Ephemeral = true
		rejection.ResponseURL = strings.TrimSpace(invocation.ResponseURL)
		return rejection
	}

	rejection.ThreadTS = strings.TrimSpace(invocation.ThreadTS)
	return rejection
}

func extractMentionCommandText(raw string) string {
	commandText := strings.TrimSpace(raw)
	if commandText == "" {
		return ""
	}

	if strings.HasPrefix(commandText, "<@") {
		if end := strings.Index(commandText, ">"); end >= 0 {
			commandText = strings.TrimSpace(commandText[end+1:])
		}
	}

	return commandText
}
