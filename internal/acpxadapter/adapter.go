package acpxadapter

import (
	"context"
	"strings"
)

type SessionRequest struct {
	ProjectPath string
	ThreadTS    string
	Prompt      string
}

type SessionResult struct {
	SessionName string
	Output      string
}

type Adapter interface {
	EnsureSession(context.Context, SessionRequest) (SessionResult, error)
	StartPrompt(context.Context, SessionRequest) (PromptStream, error)
	SendPrompt(context.Context, SessionRequest) (SessionResult, error)
	Status(context.Context, string) (SessionResult, error)
	Cancel(context.Context, string) error
}

func SessionName(threadTS string) string {
	return "slack-" + strings.TrimSpace(threadTS)
}
