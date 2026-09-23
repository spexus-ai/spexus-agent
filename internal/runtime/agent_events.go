package runtime

import "github.com/spexus-ai/spexus-agent/internal/harness"

type AgentEventKind = harness.EventKind

const (
	AgentEventSessionStarted        = harness.EventSessionStarted
	AgentEventAssistantThinking     = harness.EventAssistantThinking
	AgentEventToolStarted           = harness.EventToolStarted
	AgentEventToolFinished          = harness.EventToolFinished
	AgentEventAssistantMessageChunk = harness.EventAssistantMessageChunk
	AgentEventAssistantMessageFinal = harness.EventAssistantMessageFinal
	AgentEventSessionDone           = harness.EventSessionDone
	AgentEventSessionError          = harness.EventSessionError
	AgentEventSessionCancelled      = harness.EventSessionCancelled
)

type AgentTurnEvent = harness.Event

func TranslateAgentTurnOutput(output string) ([]AgentTurnEvent, error) {
	return harness.TranslatePromptOutput(output)
}
