package slack

import (
	"context"
	"errors"
	"strings"
)

const DefaultProjectChannelPrefix = "spexus-"

var (
	ErrChannelNameTaken = errors.New("slack channel name already exists")
	ErrChannelNotFound  = errors.New("slack channel not found")
)

type Message struct {
	ChannelID string
	ThreadTS  string
	UserID    string
	Text      string
}

const ResponseTypeEphemeral = "ephemeral"

type ResponseURLMessage struct {
	ResponseURL  string `json:"response_url,omitempty"`
	Text         string `json:"text"`
	ResponseType string `json:"response_type,omitempty"`
}

type PostedMessage struct {
	ChannelID string
	Timestamp string
}

type MessageUpdate struct {
	ChannelID string
	Timestamp string
	Text      string
}

type MessageUpdater interface {
	UpdateMessage(context.Context, MessageUpdate) error
}

type Event struct {
	ID        string
	ChannelID string
	ThreadTS  string
	Timestamp string
	UserID    string
	Text      string
}

const (
	InboundSourceMention = "mention"
	InboundSourceSlash   = "slash"
)

type InboundInvocation struct {
	SourceType    string
	DeliveryID    string
	ChannelID     string
	UserID        string
	CommandText   string
	ThreadTS      string
	ResponseURL   string
	AckEnvelopeID string
	Acked         bool
}

type Channel struct {
	ID   string
	Name string
}

type CreateChannelRequest struct {
	Name string
}

type Client interface {
	PostMessage(context.Context, Message) (PostedMessage, error)
	PostThreadMessage(context.Context, Message) error
	CreateChannel(context.Context, CreateChannelRequest) (Channel, error)
	FindChannelByName(context.Context, string) (Channel, error)
	Close() error
}

type ResponseURLClient interface {
	PostResponseURLMessage(context.Context, ResponseURLMessage) error
}

type EventSource interface {
	Events(context.Context) (<-chan Event, error)
	Close() error
}

type InboundInvocationSource interface {
	InboundInvocations(context.Context) (<-chan InboundInvocation, error)
	Close() error
}

func (e Event) ThreadTimestamp() string {
	threadTS := strings.TrimSpace(e.ThreadTS)
	if threadTS != "" {
		return threadTS
	}

	return strings.TrimSpace(e.Timestamp)
}

func (e Event) IsThreadReply() bool {
	threadTS := strings.TrimSpace(e.ThreadTS)
	timestamp := strings.TrimSpace(e.Timestamp)

	return threadTS != "" && threadTS != timestamp
}

type ChannelProvisioner interface {
	ProvisionProjectChannel(context.Context, string) (Channel, error)
}

type ProjectChannelProvisioner struct {
	Client Client
	Prefix string
}

func ProjectChannelName(projectName string) string {
	projectName = strings.TrimSpace(projectName)
	if projectName == "" {
		return ""
	}
	return DefaultProjectChannelPrefix + projectName
}

func (p ProjectChannelProvisioner) ProvisionProjectChannel(ctx context.Context, projectName string) (Channel, error) {
	projectName = strings.TrimSpace(projectName)
	if projectName == "" {
		return Channel{}, errors.New("project name is required")
	}
	if p.Client == nil {
		return Channel{}, errors.New("slack client is required")
	}

	prefix := p.Prefix
	if prefix == "" {
		prefix = DefaultProjectChannelPrefix
	}

	channelName := prefix + projectName
	channel, err := p.Client.CreateChannel(ctx, CreateChannelRequest{Name: channelName})
	if err != nil {
		if errors.Is(err, ErrChannelNameTaken) {
			channel, err = p.Client.FindChannelByName(ctx, channelName)
		}
		if err != nil {
			return Channel{}, err
		}
	}
	if channel.Name == "" {
		channel.Name = channelName
	}
	if channel.Name != channelName {
		return Channel{}, errors.New("slack channel name mismatch")
	}
	if channel.ID == "" {
		return Channel{}, errors.New("slack channel id is required")
	}
	return channel, nil
}
