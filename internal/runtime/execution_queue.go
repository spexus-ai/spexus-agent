package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/spexus-ai/spexus-agent/internal/acpxadapter"
	"github.com/spexus-ai/spexus-agent/internal/registry"
	"github.com/spexus-ai/spexus-agent/internal/slack"
)

type ExecutionRequest struct {
	SourceType    string           `json:"sourceType"`
	DeliveryID    string           `json:"deliveryId"`
	ChannelID     string           `json:"channelId"`
	UserID        string           `json:"userId"`
	CommandText   string           `json:"commandText"`
	ResponseURL   string           `json:"responseUrl,omitempty"`
	Project       registry.Project `json:"project"`
	ThreadTS      string           `json:"threadTs,omitempty"`
	SessionName   string           `json:"sessionName,omitempty"`
	IsThreadReply bool             `json:"isThreadReply"`
}

type ExecutionQueue interface {
	Enqueue(context.Context, ExecutionRequest) error
}

func NewMentionExecutionRequest(prepared PreparedSlackEvent) ExecutionRequest {
	return ExecutionRequest{
		SourceType:    prepared.SourceType,
		DeliveryID:    prepared.DeliveryID,
		ChannelID:     prepared.Project.SlackChannelID,
		UserID:        prepared.Event.UserID,
		CommandText:   prepared.Event.Text,
		Project:       prepared.Project,
		ThreadTS:      prepared.ThreadTS,
		SessionName:   prepared.SessionName,
		IsThreadReply: prepared.IsThreadReply,
	}
}

func NewSlashExecutionRequest(prepared PreparedSlackInvocation) ExecutionRequest {
	return ExecutionRequest{
		SourceType:  prepared.Invocation.SourceType,
		DeliveryID:  prepared.Invocation.DeliveryID,
		ChannelID:   prepared.Project.SlackChannelID,
		UserID:      prepared.Invocation.UserID,
		CommandText: prepared.Invocation.CommandText,
		ResponseURL: prepared.Invocation.ResponseURL,
		Project:     prepared.Project,
	}
}

func (r ExecutionRequest) WithThread(threadTS string) ExecutionRequest {
	r.ThreadTS = threadTS
	r.SessionName = acpxadapter.SessionName(threadTS)
	return r
}

func (r ExecutionRequest) SessionKey() string {
	if r.SessionName != "" {
		return r.SessionName
	}
	if r.ThreadTS != "" {
		return acpxadapter.SessionName(r.ThreadTS)
	}
	return fmt.Sprintf("%s/%s", r.SourceType, r.DeliveryID)
}

func (r ExecutionRequest) ExecutionID() string {
	return fmt.Sprintf("%s:%s", r.SourceType, r.DeliveryID)
}

func (r ExecutionRequest) PreparedEvent() (PreparedSlackEvent, error) {
	if err := r.validateForPreparedEvent(); err != nil {
		return PreparedSlackEvent{}, err
	}

	return PreparedSlackEvent{
		SourceType: r.SourceType,
		DeliveryID: r.DeliveryID,
		Event: slack.Event{
			ID:        r.DeliveryID,
			ChannelID: r.ChannelID,
			ThreadTS:  r.ThreadTS,
			Timestamp: r.ThreadTS,
			UserID:    r.UserID,
			Text:      r.CommandText,
		},
		Project:       r.Project,
		ThreadTS:      r.ThreadTS,
		SessionName:   r.SessionName,
		IsThreadReply: r.IsThreadReply,
		ThreadState: ThreadState{
			ThreadTS:    r.ThreadTS,
			ChannelID:   r.ChannelID,
			ProjectName: r.Project.Name,
			SessionName: r.SessionName,
		},
	}, nil
}

func (r ExecutionRequest) validateForEnqueue() error {
	if r.SourceType == "" {
		return errors.New("execution request source type is required")
	}
	if r.DeliveryID == "" {
		return errors.New("execution request delivery id is required")
	}
	if r.ChannelID == "" {
		return errors.New("execution request channel id is required")
	}
	if r.Project.Name == "" {
		return errors.New("execution request project is required")
	}
	if r.SourceType == slack.InboundSourceMention {
		if err := r.validateThreadContext(); err != nil {
			return err
		}
	}
	return nil
}

func (r ExecutionRequest) validateForPreparedEvent() error {
	if err := r.validateForEnqueue(); err != nil {
		return err
	}
	return r.validateThreadContext()
}

func (r ExecutionRequest) validateThreadContext() error {
	if r.ThreadTS == "" {
		return errors.New("execution request thread timestamp is required")
	}
	if r.SessionName == "" {
		return errors.New("execution request session name is required")
	}
	return nil
}

func (r ExecutionRequest) String() string {
	return fmt.Sprintf("%s/%s", r.SourceType, r.DeliveryID)
}
