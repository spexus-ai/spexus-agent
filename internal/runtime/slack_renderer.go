package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spexus-ai/spexus-agent/internal/slack"
)

var ErrSlackRendererClientRequired = errors.New("slack renderer client is required")

type SlackThreadRenderRequest struct {
	ChannelID   string
	ThreadTS    string
	SessionName string
}

type SlackThreadRenderer struct {
	Client slack.Client
}

func RenderACPXTurnOutput(ctx context.Context, renderer SlackThreadRenderer, req SlackThreadRenderRequest, output string) error {
	events, err := TranslateACPXTurnOutput(output)
	if err != nil {
		return err
	}
	return renderer.Render(ctx, req, events)
}

func (r SlackThreadRenderer) Render(ctx context.Context, req SlackThreadRenderRequest, events []ACPXTurnEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateSlackThreadRenderRequest(r.Client, req); err != nil {
		return err
	}

	messages := buildSlackThreadMessages(req, events)
	for _, message := range messages {
		if err := r.Client.PostThreadMessage(ctx, message); err != nil {
			return err
		}
	}

	return nil
}

func validateSlackThreadRenderRequest(client slack.Client, req SlackThreadRenderRequest) error {
	if client == nil {
		return ErrSlackRendererClientRequired
	}
	if strings.TrimSpace(req.ChannelID) == "" {
		return fmt.Errorf("slack channel id is required")
	}
	if strings.TrimSpace(req.ThreadTS) == "" {
		return fmt.Errorf("slack thread timestamp is required")
	}
	if strings.TrimSpace(req.SessionName) == "" {
		return fmt.Errorf("slack session name is required")
	}
	return nil
}

func buildSlackThreadMessages(req SlackThreadRenderRequest, events []ACPXTurnEvent) []slack.Message {
	progress := make([]string, 0, len(events))
	var assistantProgress strings.Builder
	finalParts := make([]string, 0, len(events))
	messages := make([]slack.Message, 0, 2)
	terminal := ""
	sessionDone := false

	flushProgress := func(includeAssistant bool) {
		progressLines := append([]string(nil), progress...)
		assistant := ""
		if includeAssistant {
			assistant = strings.TrimSpace(assistantProgress.String())
		}
		if len(progressLines) == 0 && assistant == "" {
			if includeAssistant {
				assistantProgress.Reset()
			}
			return
		}
		messages = append(messages, slack.Message{
			ChannelID: req.ChannelID,
			ThreadTS:  req.ThreadTS,
			Text:      formatLiveProgressMessage(progressLines, assistant),
		})
		progress = progress[:0]
		if includeAssistant {
			assistantProgress.Reset()
		}
	}

	appendProgress := func(text string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		progress = append(progress, text)
	}

	appendAssistantChunk := func(text string) {
		if text == "" {
			return
		}
		if assistantProgress.Len() == 0 {
			text = strings.TrimSpace(text)
			if text == "" {
				return
			}
			assistantProgress.WriteString(text)
			return
		}
		assistantProgress.WriteString(text)
	}

	for _, event := range events {
		switch event.Kind {
		case ACPXEventSessionStarted:
			appendProgress("Session started" + suffixWithText(event.Text))
		case ACPXEventAssistantThinking:
			appendProgress("Thinking" + suffixWithText(event.Text))
		case ACPXEventToolStarted:
			continue
		case ACPXEventToolFinished:
			continue
		case ACPXEventAssistantMessageChunk:
			appendAssistantChunk(event.Text)
		case ACPXEventAssistantMessageFinal:
			if len(progress) > 0 {
				flushProgress(false)
			}
			if text := strings.TrimSpace(event.Text); text != "" {
				finalParts = append(finalParts, text)
			}
		case ACPXEventSessionDone:
			if len(progress) > 0 {
				flushProgress(false)
			}
			sessionDone = true
		case ACPXEventSessionError:
			flushProgress(true)
			finalParts = finalParts[:0]
			terminal = "Session error" + suffixWithText(event.Text)
		case ACPXEventSessionCancelled:
			flushProgress(true)
			finalParts = finalParts[:0]
			terminal = "Session cancelled" + suffixWithText(event.Text)
		}
	}

	if len(progress) > 0 || strings.TrimSpace(assistantProgress.String()) != "" {
		switch {
		case terminal != "":
			flushProgress(true)
		case len(finalParts) > 0 || sessionDone:
			flushProgress(false)
		default:
			flushProgress(true)
		}
	}
	if terminal != "" {
		finalParts = finalParts[:0]
		finalParts = append(finalParts, terminal)
	} else if sessionDone && len(finalParts) == 0 {
		finalParts = append(finalParts, "Session complete.")
	}

	if len(finalParts) == 0 {
		return messages
	}

	messages = append(messages, slack.Message{
		ChannelID: req.ChannelID,
		ThreadTS:  req.ThreadTS,
		Text:      strings.Join(finalParts, "\n\n"),
	})

	return messages
}

func appendProgressLine(lines *[]string, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	*lines = append(*lines, text)
}

func formatToolLine(action string, event ACPXTurnEvent) string {
	label := "Tool " + action
	if event.ToolName != "" {
		label += ": " + event.ToolName
	}
	if event.Text != "" {
		label += " - " + event.Text
	}
	return label
}

func formatBatchMessage(title string, lines []string) string {
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			filtered = append(filtered, line)
		}
	}
	if len(filtered) == 0 {
		return ""
	}

	builder := strings.Builder{}
	builder.WriteString(title)
	builder.WriteString(":\n")
	for i, line := range filtered {
		if i > 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString("- ")
		builder.WriteString(line)
	}
	return builder.String()
}

func formatLiveProgressMessage(progressLines []string, assistantText string) string {
	progress := formatBatchMessage("Progress", progressLines)
	assistantText = strings.TrimSpace(assistantText)

	switch {
	case progress == "":
		return assistantText
	case assistantText == "":
		return progress
	default:
		return progress + "\n\n" + assistantText
	}
}

func suffixWithText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return ": " + text
}
