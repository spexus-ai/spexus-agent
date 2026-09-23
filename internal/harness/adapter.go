package harness

import (
	"context"
	"strings"
)

type SessionRequest struct {
	ProjectPath string
	ChannelID   string
	ThreadTS    string
	Prompt      string
}

type SessionResult struct {
	SessionName string
	Output      string
}

type Adapter interface {
	StartPrompt(context.Context, SessionRequest) (PromptStream, error)
	Cancel(context.Context, string) error
}

func SessionName(threadTS string) string {
	return "slack-" + strings.TrimSpace(threadTS)
}
