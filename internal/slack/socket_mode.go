package slack

import (
	"errors"
	"fmt"
	"strings"
)

const (
	SocketModeMessageType    = "message"
	SocketModeAppMentionType = "app_mention"
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
