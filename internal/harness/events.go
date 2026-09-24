package harness

import (
	"encoding/json"
	"fmt"
	"strings"
)

type EventKind string

const (
	EventSessionStarted        EventKind = "session_started"
	EventAssistantThinking     EventKind = "assistant_thinking"
	EventToolStarted           EventKind = "tool_started"
	EventToolFinished          EventKind = "tool_finished"
	EventAssistantMessageChunk EventKind = "assistant_message_chunk"
	EventAssistantMessageFinal EventKind = "assistant_message_final"
	EventSessionDone           EventKind = "session_done"
	EventSessionError          EventKind = "session_error"
	EventSessionCancelled      EventKind = "session_cancelled"
)

type Event struct {
	Kind        EventKind `json:"kind"`
	Text        string    `json:"text,omitempty"`
	ToolName    string    `json:"toolName,omitempty"`
	ToolStatus  string    `json:"toolStatus,omitempty"`
	SessionName string    `json:"sessionName,omitempty"`
}

func TranslatePromptOutput(output string) ([]Event, error) {
	var events []Event
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return nil, fmt.Errorf("parse agent event: %w", err)
		}
		if event.Kind == "" {
			return nil, fmt.Errorf("agent event kind is required")
		}
		events = append(events, event)
	}
	return events, nil
}
