package slack

import (
	"errors"
	"fmt"
	"strings"
)

const (
	SocketModeEnvelopeEventsAPI     = "events_api"
	SocketModeEnvelopeSlashCommands = "slash_commands"
	SocketModeMessageType           = "message"
	SocketModeAppMentionType        = "app_mention"
)

var ErrSocketModeEventUnsupported = errors.New("unsupported slack socket mode event")

type SocketModeMessage struct {
	EventID   string `json:"event_id,omitempty"`
	Type      string `json:"type,omitempty"`
	ChannelID string `json:"channel,omitempty"`
	ThreadTS  string `json:"thread_ts,omitempty"`
	Timestamp string `json:"ts,omitempty"`
	UserID    string `json:"user,omitempty"`
	BotID     string `json:"bot_id,omitempty"`
	AppID     string `json:"app_id,omitempty"`
	Text      string `json:"text,omitempty"`
	Subtype   string `json:"subtype,omitempty"`
}

type SocketModeSlashCommand struct {
	Command     string `json:"command,omitempty"`
	ChannelID   string `json:"channel_id,omitempty"`
	UserID      string `json:"user_id,omitempty"`
	Text        string `json:"text,omitempty"`
	ResponseURL string `json:"response_url,omitempty"`
}

func (e SocketModeMessage) Normalize() (Event, error) {
	switch strings.TrimSpace(e.Type) {
	case SocketModeMessageType, SocketModeAppMentionType:
	default:
		return Event{}, ErrSocketModeEventUnsupported
	}
	if strings.TrimSpace(e.ChannelID) == "" {
		return Event{}, fmt.Errorf("slack event channel id is required")
	}
	if strings.TrimSpace(e.UserID) == "" {
		return Event{}, fmt.Errorf("slack event user id is required")
	}

	return Event{
		ID:        strings.TrimSpace(e.EventID),
		ChannelID: strings.TrimSpace(e.ChannelID),
		ThreadTS:  strings.TrimSpace(e.ThreadTS),
		Timestamp: strings.TrimSpace(e.Timestamp),
		UserID:    strings.TrimSpace(e.UserID),
		Text:      e.Text,
	}, nil
}

func (e SocketModeMessage) NormalizeInbound() (InboundInvocation, error) {
	sourceType := ""
	threadTS := strings.TrimSpace(e.ThreadTS)
	switch strings.TrimSpace(e.Type) {
	case SocketModeAppMentionType:
		sourceType = InboundSourceMention
		if threadTS == "" {
			threadTS = strings.TrimSpace(e.Timestamp)
		}
	case SocketModeMessageType:
		sourceType = InboundSourceMessage
	default:
		return InboundInvocation{}, ErrSocketModeEventUnsupported
	}
	if strings.TrimSpace(e.ChannelID) == "" {
		return InboundInvocation{}, fmt.Errorf("slack event channel id is required")
	}
	if strings.TrimSpace(e.UserID) == "" {
		return InboundInvocation{}, fmt.Errorf("slack event user id is required")
	}

	return InboundInvocation{
		SourceType:  sourceType,
		DeliveryID:  strings.TrimSpace(e.EventID),
		ChannelID:   strings.TrimSpace(e.ChannelID),
		UserID:      strings.TrimSpace(e.UserID),
		CommandText: e.Text,
		ThreadTS:    threadTS,
	}, nil
}

func (e SocketModeSlashCommand) NormalizeInbound(envelopeID string) (InboundInvocation, error) {
	if strings.TrimSpace(e.ChannelID) == "" {
		return InboundInvocation{}, fmt.Errorf("slack slash command channel id is required")
	}
	if strings.TrimSpace(e.UserID) == "" {
		return InboundInvocation{}, fmt.Errorf("slack slash command user id is required")
	}

	return InboundInvocation{
		SourceType:    InboundSourceSlash,
		DeliveryID:    strings.TrimSpace(envelopeID),
		ChannelID:     strings.TrimSpace(e.ChannelID),
		UserID:        strings.TrimSpace(e.UserID),
		CommandText:   e.Text,
		ResponseURL:   strings.TrimSpace(e.ResponseURL),
		AckEnvelopeID: strings.TrimSpace(envelopeID),
	}, nil
}
