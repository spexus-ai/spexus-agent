package runtime

import "github.com/spexus-ai/spexus-agent/internal/acpxadapter"

type ACPXEventKind = acpxadapter.EventKind

const (
	ACPXEventSessionStarted        = acpxadapter.EventSessionStarted
	ACPXEventAssistantThinking     = acpxadapter.EventAssistantThinking
	ACPXEventToolStarted           = acpxadapter.EventToolStarted
	ACPXEventToolFinished          = acpxadapter.EventToolFinished
	ACPXEventAssistantMessageChunk = acpxadapter.EventAssistantMessageChunk
	ACPXEventAssistantMessageFinal = acpxadapter.EventAssistantMessageFinal
	ACPXEventSessionDone           = acpxadapter.EventSessionDone
	ACPXEventSessionError          = acpxadapter.EventSessionError
	ACPXEventSessionCancelled      = acpxadapter.EventSessionCancelled
)

type ACPXTurnEvent = acpxadapter.Event

func TranslateACPXTurnOutput(output string) ([]ACPXTurnEvent, error) {
	return acpxadapter.TranslatePromptOutput(output)
}
