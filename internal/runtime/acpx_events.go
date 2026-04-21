package runtime

import (
	"encoding/json"
	"fmt"
	"strings"
)

type ACPXEventKind string

const (
	ACPXEventSessionStarted        ACPXEventKind = "session_started"
	ACPXEventAssistantThinking     ACPXEventKind = "assistant_thinking"
	ACPXEventToolStarted           ACPXEventKind = "tool_started"
	ACPXEventToolFinished          ACPXEventKind = "tool_finished"
	ACPXEventAssistantMessageChunk ACPXEventKind = "assistant_message_chunk"
	ACPXEventAssistantMessageFinal ACPXEventKind = "assistant_message_final"
	ACPXEventSessionDone           ACPXEventKind = "session_done"
	ACPXEventSessionError          ACPXEventKind = "session_error"
	ACPXEventSessionCancelled      ACPXEventKind = "session_cancelled"
)

type ACPXTurnEvent struct {
	Kind        ACPXEventKind `json:"kind"`
	Text        string        `json:"text,omitempty"`
	ToolName    string        `json:"toolName,omitempty"`
	ToolStatus  string        `json:"toolStatus,omitempty"`
	SessionName string        `json:"sessionName,omitempty"`
}

func TranslateACPXTurnOutput(output string) ([]ACPXTurnEvent, error) {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return nil, nil
	}

	if events, ok, err := parseACPXJSONLines(output); err != nil {
		return nil, err
	} else if ok {
		return events, nil
	}

	return nil, fmt.Errorf("parse acpx structured output: unsupported format")
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

func parseACPXJSONLines(output string) ([]ACPXTurnEvent, bool, error) {
	lines := strings.Split(output, "\n")
	events := make([]ACPXTurnEvent, 0, len(lines))
	toolTitles := make(map[string]string)
	var textBuilder strings.Builder
	jsonSeen := false

	flushAssistantText := func() {
		text := strings.TrimSpace(textBuilder.String())
		if text == "" {
			textBuilder.Reset()
			return
		}
		events = append(events, ACPXTurnEvent{
			Kind: ACPXEventAssistantMessageFinal,
			Text: text,
		})
		textBuilder.Reset()
	}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "{") {
			if jsonSeen {
				return nil, true, fmt.Errorf("parse acpx structured output: invalid character %q looking for beginning of value", line[:1])
			}
			return nil, false, nil
		}

		jsonSeen = true

		var envelope acpxRPCEnvelope
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			return nil, true, fmt.Errorf("parse acpx structured output: %w", err)
		}
		if envelope.Error != nil {
			flushAssistantText()
			events = append(events, ACPXTurnEvent{
				Kind: ACPXEventSessionError,
				Text: strings.TrimSpace(envelope.Error.Message),
			})
			continue
		}

		switch envelope.Method {
		case "session/new":
			var result acpxSessionNewResult
			if len(envelope.Result) > 0 && json.Unmarshal(envelope.Result, &result) == nil && strings.TrimSpace(result.SessionID) != "" {
				events = append(events, ACPXTurnEvent{
					Kind:        ACPXEventSessionStarted,
					SessionName: strings.TrimSpace(result.SessionID),
					Text:        strings.TrimSpace(result.SessionID),
				})
			}
		case "session/update":
			var params acpxSessionUpdateParams
			if err := json.Unmarshal(envelope.Params, &params); err != nil {
				return nil, true, fmt.Errorf("parse acpx structured output: %w", err)
			}
			switch params.Update.SessionUpdate {
			case "agent_message_chunk":
				text, err := parseACPXContentText(params.Update.Content)
				if err != nil {
					return nil, true, fmt.Errorf("parse acpx structured output: %w", err)
				}
				if text != "" {
					textBuilder.WriteString(text)
				}
			case "tool_call":
				flushAssistantText()
				title := strings.TrimSpace(firstNonEmpty(params.Update.Title, params.Update.Kind, params.Update.ToolCallID))
				if params.Update.ToolCallID != "" {
					toolTitles[params.Update.ToolCallID] = title
				}
				events = append(events, ACPXTurnEvent{
					Kind:       ACPXEventToolStarted,
					ToolName:   title,
					ToolStatus: strings.TrimSpace(params.Update.Status),
				})
			case "tool_call_update":
				flushAssistantText()
				title := strings.TrimSpace(firstNonEmpty(toolTitles[params.Update.ToolCallID], params.Update.Title, params.Update.ToolCallID))
				status := strings.TrimSpace(params.Update.Status)
				if status == "completed" || status == "failed" || status == "cancelled" {
					events = append(events, ACPXTurnEvent{
						Kind:       ACPXEventToolFinished,
						ToolName:   title,
						ToolStatus: status,
					})
				}
			}
		default:
			if len(envelope.Result) == 0 {
				continue
			}
			var result acpxPromptResult
			if err := json.Unmarshal(envelope.Result, &result); err == nil && result.StopReason != "" {
				flushAssistantText()
				switch strings.TrimSpace(result.StopReason) {
				case "end_turn":
					events = append(events, ACPXTurnEvent{Kind: ACPXEventSessionDone})
				case "cancelled":
					events = append(events, ACPXTurnEvent{Kind: ACPXEventSessionCancelled, Text: "cancelled"})
				default:
					events = append(events, ACPXTurnEvent{Kind: ACPXEventSessionDone, Text: strings.TrimSpace(result.StopReason)})
				}
			}
		}
	}

	if !jsonSeen {
		return nil, false, nil
	}

	flushAssistantText()
	return events, true, nil
}

func parseACPXContentText(data json.RawMessage) (string, error) {
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
			text, err := parseACPXContentText(part)
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

func normalizeACPXEventKind(values ...string) ACPXEventKind {
	for _, value := range values {
		switch strings.TrimSpace(strings.ToLower(value)) {
		case string(ACPXEventSessionStarted):
			return ACPXEventSessionStarted
		case string(ACPXEventAssistantThinking):
			return ACPXEventAssistantThinking
		case string(ACPXEventToolStarted):
			return ACPXEventToolStarted
		case string(ACPXEventToolFinished):
			return ACPXEventToolFinished
		case string(ACPXEventAssistantMessageChunk):
			return ACPXEventAssistantMessageChunk
		case string(ACPXEventAssistantMessageFinal):
			return ACPXEventAssistantMessageFinal
		case string(ACPXEventSessionDone):
			return ACPXEventSessionDone
		case string(ACPXEventSessionError):
			return ACPXEventSessionError
		case string(ACPXEventSessionCancelled):
			return ACPXEventSessionCancelled
		}
	}

	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
