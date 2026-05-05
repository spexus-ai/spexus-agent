package acpxadapter

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
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return nil, nil
	}

	parser := newEventLineParser()
	lines := strings.Split(output, "\n")
	events := make([]Event, 0, len(lines))
	for _, line := range lines {
		lineEvents, err := parser.Parse(line)
		if err != nil {
			return nil, err
		}
		events = append(events, lineEvents...)
	}
	events = append(events, parser.Finish()...)
	return events, nil
}

type eventLineParser struct {
	toolTitles       map[string]string
	finalTextBuilder strings.Builder
	jsonSeen         bool
}

type acpxRPCEnvelope struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *acpxRPCError   `json:"error"`
}

type acpxRPCError struct {
	Message string `json:"message"`
}

type acpxSessionUpdateParams struct {
	SessionID string            `json:"sessionId"`
	Update    acpxSessionUpdate `json:"update"`
}

type acpxSessionUpdate struct {
	SessionUpdate string          `json:"sessionUpdate"`
	Content       json.RawMessage `json:"content"`
	ToolCallID    string          `json:"toolCallId"`
	Title         string          `json:"title"`
	Kind          string          `json:"kind"`
	Status        string          `json:"status"`
	RawOutput     json.RawMessage `json:"rawOutput"`
}

type acpxContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type acpxPromptResult struct {
	StopReason string `json:"stopReason"`
}

type acpxSessionNewResult struct {
	SessionID string `json:"sessionId"`
}

func newEventLineParser() *eventLineParser {
	return &eventLineParser{
		toolTitles: make(map[string]string),
	}
}

func (p *eventLineParser) Parse(line string) ([]Event, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, nil
	}
	if !strings.HasPrefix(line, "{") {
		if p.jsonSeen {
			return nil, fmt.Errorf("parse acpx structured output: invalid character %q looking for beginning of value", line[:1])
		}
		return nil, fmt.Errorf("parse acpx structured output: unsupported format")
	}

	p.jsonSeen = true

	var envelope acpxRPCEnvelope
	if err := json.Unmarshal([]byte(line), &envelope); err != nil {
		return nil, fmt.Errorf("parse acpx structured output: %w", err)
	}
	if envelope.Error != nil {
		return []Event{{
			Kind: EventSessionError,
			Text: strings.TrimSpace(envelope.Error.Message),
		}}, nil
	}

	switch envelope.Method {
	case "session/new":
		var result acpxSessionNewResult
		if len(envelope.Result) > 0 && json.Unmarshal(envelope.Result, &result) == nil && strings.TrimSpace(result.SessionID) != "" {
			return []Event{{
				Kind:        EventSessionStarted,
				SessionName: strings.TrimSpace(result.SessionID),
				Text:        strings.TrimSpace(result.SessionID),
			}}, nil
		}
		return nil, nil
	case "session/update":
		var params acpxSessionUpdateParams
		if err := json.Unmarshal(envelope.Params, &params); err != nil {
			return nil, fmt.Errorf("parse acpx structured output: %w", err)
		}
		switch params.Update.SessionUpdate {
		case "agent_message_chunk":
			text, err := parseContentText(params.Update.Content)
			if err != nil {
				return nil, fmt.Errorf("parse acpx structured output: %w", err)
			}
			return p.appendAssistantChunk(text), nil
		case "tool_call":
			title := strings.TrimSpace(firstNonEmpty(params.Update.Title, params.Update.Kind, params.Update.ToolCallID))
			if params.Update.ToolCallID != "" {
				p.toolTitles[params.Update.ToolCallID] = title
			}
			return []Event{{
				Kind:       EventToolStarted,
				ToolName:   title,
				ToolStatus: strings.TrimSpace(params.Update.Status),
			}}, nil
		case "tool_call_update":
			status := strings.TrimSpace(params.Update.Status)
			if status == "completed" || status == "failed" || status == "cancelled" {
				title := strings.TrimSpace(firstNonEmpty(p.toolTitles[params.Update.ToolCallID], params.Update.Title, params.Update.ToolCallID))
				return []Event{{
					Kind:       EventToolFinished,
					ToolName:   title,
					ToolStatus: status,
				}}, nil
			}
		}
		return nil, nil
	default:
		if len(envelope.Result) == 0 {
			return nil, nil
		}
		var result acpxPromptResult
		if err := json.Unmarshal(envelope.Result, &result); err == nil && result.StopReason != "" {
			events := make([]Event, 0, 2)
			switch strings.TrimSpace(result.StopReason) {
			case "end_turn":
				events = append(events, p.finishAssistantText()...)
				events = append(events, Event{Kind: EventSessionDone})
			case "cancelled":
				events = append(events, Event{Kind: EventSessionCancelled, Text: "cancelled"})
			default:
				events = append(events, p.finishAssistantText()...)
				events = append(events, Event{Kind: EventSessionDone, Text: strings.TrimSpace(result.StopReason)})
			}
			return events, nil
		}
	}

	return nil, nil
}

func (p *eventLineParser) Finish() []Event {
	return p.finishAssistantText()
}

func (p *eventLineParser) appendAssistantChunk(text string) []Event {
	if text == "" {
		return nil
	}

	p.finalTextBuilder.WriteString(text)
	if strings.TrimSpace(text) == "" {
		return nil
	}

	return []Event{{
		Kind: EventAssistantMessageChunk,
		Text: text,
	}}
}

func (p *eventLineParser) finishAssistantText() []Event {
	text := strings.TrimSpace(p.finalTextBuilder.String())
	if text == "" {
		p.finalTextBuilder.Reset()
		return nil
	}

	p.finalTextBuilder.Reset()
	return []Event{{
		Kind: EventAssistantMessageFinal,
		Text: text,
	}}
}

func parseContentText(data json.RawMessage) (string, error) {
	data = bytesTrimSpace(data)
	if len(data) == 0 || string(data) == "null" {
		return "", nil
	}

	if data[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return "", err
		}
		return text, nil
	}

	if data[0] == '{' {
		var content acpxContent
		if err := json.Unmarshal(data, &content); err != nil {
			return "", err
		}
		if content.Type == "text" || content.Type == "" {
			return content.Text, nil
		}
		return "", nil
	}

	if data[0] == '[' {
		var parts []json.RawMessage
		if err := json.Unmarshal(data, &parts); err != nil {
			return "", err
		}
		var builder strings.Builder
		for _, part := range parts {
			text, err := parseContentText(part)
			if err != nil {
				return "", err
			}
			builder.WriteString(text)
		}
		return builder.String(), nil
	}

	return "", fmt.Errorf("unsupported content payload: %s", string(data))
}

func bytesTrimSpace(data []byte) []byte {
	start := 0
	for start < len(data) && (data[start] == ' ' || data[start] == '\n' || data[start] == '\t' || data[start] == '\r') {
		start++
	}
	end := len(data)
	for end > start && (data[end-1] == ' ' || data[end-1] == '\n' || data[end-1] == '\t' || data[end-1] == '\r') {
		end--
	}
	return data[start:end]
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
