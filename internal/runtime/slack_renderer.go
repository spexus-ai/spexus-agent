package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spexus-ai/spexus-agent/internal/slack"
)

var ErrSlackRendererClientRequired = errors.New("slack renderer client is required")

const streamingSlackMessageSoftLimit = 2000

type SlackThreadRenderRequest struct {
	ChannelID   string
	ThreadTS    string
	SessionName string
}

type SlackThreadRenderer struct {
	Client slack.Client
}

type SlackThreadStream struct {
	renderer SlackThreadRenderer
	req      SlackThreadRenderRequest
	sentText string
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
	if r.Client == nil {
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

	messages := buildSlackThreadMessages(req, events)
	for _, message := range messages {
		if err := r.Client.PostThreadMessage(ctx, message); err != nil {
			return err
		}
	}

	return nil
}

func (r SlackThreadRenderer) NewStream(ctx context.Context, req SlackThreadRenderRequest) (*SlackThreadStream, error) {
	if err := r.validate(ctx, req); err != nil {
		return nil, err
	}
	return &SlackThreadStream{
		renderer: r,
		req:      req,
	}, nil
}

func (s *SlackThreadStream) RenderOutput(ctx context.Context, output string) error {
	events, err := TranslateACPXTurnOutput(output)
	if err != nil {
		return err
	}
	return s.Render(ctx, events)
}

func (s *SlackThreadStream) Render(ctx context.Context, events []ACPXTurnEvent) error {
	if s == nil {
		return errors.New("slack thread stream is required")
	}
	if err := s.renderer.validate(ctx, s.req); err != nil {
		return err
	}

	state := buildStreamingSlackThreadState(events)
	if state.text == "" {
		return nil
	}

	remaining := state.text
	if strings.HasPrefix(state.text, s.sentText) {
		remaining = state.text[len(s.sentText):]
	} else {
		s.sentText = ""
	}

	for {
		chunk, rest, ok := nextStreamingSlackChunk(remaining, state.terminal)
		if !ok {
			return nil
		}
		if strings.TrimSpace(chunk) != "" {
			if _, err := s.renderer.Client.PostMessage(ctx, slack.Message{
				ChannelID: s.req.ChannelID,
				ThreadTS:  s.req.ThreadTS,
				Text:      chunk,
			}); err != nil {
				return err
			}
			s.sentText += chunk
			if strings.HasPrefix(remaining, chunk) {
				sentSeparator := remaining[len(chunk) : len(remaining)-len(rest)]
				s.sentText += sentSeparator
			}
		}
		remaining = rest
		if strings.TrimSpace(remaining) == "" {
			return nil
		}
	}
}

func (r SlackThreadRenderer) validate(ctx context.Context, req SlackThreadRenderRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.Client == nil {
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
	finalParts := make([]string, 0, len(events))
	messages := make([]slack.Message, 0, 2)
	terminal := ""
	sessionDone := false

	flushProgress := func() {
		if len(progress) == 0 {
			return
		}
		messages = append(messages, slack.Message{
			ChannelID: req.ChannelID,
			ThreadTS:  req.ThreadTS,
			Text:      formatBatchMessage("Progress", progress),
		})
		progress = progress[:0]
	}

	for _, event := range events {
		switch event.Kind {
		case ACPXEventSessionStarted:
			appendProgressLine(&progress, "Session started"+suffixWithText(event.Text))
		case ACPXEventAssistantThinking:
			appendProgressLine(&progress, "Thinking"+suffixWithText(event.Text))
		case ACPXEventToolStarted:
			continue
		case ACPXEventToolFinished:
			continue
		case ACPXEventAssistantMessageChunk:
			appendProgressLine(&progress, event.Text)
		case ACPXEventAssistantMessageFinal:
			flushProgress()
			if text := strings.TrimSpace(event.Text); text != "" {
				finalParts = append(finalParts, text)
			}
		case ACPXEventSessionDone:
			flushProgress()
			sessionDone = true
		case ACPXEventSessionError:
			flushProgress()
			finalParts = finalParts[:0]
			terminal = "Session error" + suffixWithText(event.Text)
		case ACPXEventSessionCancelled:
			flushProgress()
			finalParts = finalParts[:0]
			terminal = "Session cancelled" + suffixWithText(event.Text)
		}
	}

	flushProgress()
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

type streamingSlackThreadState struct {
	text     string
	terminal bool
}

func buildStreamingSlackThreadTexts(events []ACPXTurnEvent) []string {
	state := buildStreamingSlackThreadState(events)
	if state.text == "" {
		return nil
	}
	return splitStreamingSlackText(state.text, slack.MessageTextSoftLimit)
}

func buildStreamingSlackThreadState(events []ACPXTurnEvent) streamingSlackThreadState {
	parts := make([]string, 0, len(events))
	terminal := ""
	sessionDone := false

	for _, event := range events {
		switch event.Kind {
		case ACPXEventAssistantMessageChunk, ACPXEventAssistantMessageFinal:
			if text := strings.Trim(event.Text, " \t\r"); strings.TrimSpace(text) != "" {
				parts = append(parts, text)
			}
		case ACPXEventSessionError:
			terminal = "Session error" + suffixWithText(event.Text)
		case ACPXEventSessionCancelled:
			terminal = "Session cancelled" + suffixWithText(event.Text)
		case ACPXEventSessionDone:
			sessionDone = true
		}
	}

	if terminal != "" {
		return streamingSlackThreadState{text: terminal, terminal: true}
	}
	if len(parts) == 0 {
		if sessionDone {
			return streamingSlackThreadState{text: "Session complete.", terminal: true}
		}
		return streamingSlackThreadState{}
	}
	return streamingSlackThreadState{
		text:     strings.Join(parts, "\n\n"),
		terminal: sessionDone,
	}
}

func nextStreamingSlackChunk(text string, terminal bool) (string, string, bool) {
	if text == "" {
		return "", "", false
	}

	runes := []rune(text)
	if len(runes) > streamingSlackMessageSoftLimit {
		cut := streamingSlackMessageSoftLimit
		if newline := lastRuneIndex(runes[:streamingSlackMessageSoftLimit], '\n'); newline >= 0 {
			cut = newline
		}
		if cut <= 0 {
			cut = streamingSlackMessageSoftLimit
		}
		chunk := strings.TrimRight(string(runes[:cut]), "\n")
		restStart := cut
		if restStart < len(runes) && runes[restStart] == '\n' {
			restStart++
		}
		return chunk, string(runes[restStart:]), true
	}

	if terminal {
		return text, "", true
	}

	newline := lastRuneIndex(runes, '\n')
	if newline < 0 {
		return "", text, false
	}
	chunk := strings.TrimRight(string(runes[:newline]), "\n")
	rest := string(runes[newline+1:])
	if chunk == "" {
		return "", rest, false
	}
	return chunk, rest, true
}

func lastRuneIndex(runes []rune, target rune) int {
	for i := len(runes) - 1; i >= 0; i-- {
		if runes[i] == target {
			return i
		}
	}
	return -1
}

func splitStreamingSlackText(text string, limit int) []string {
	if len([]rune(text)) <= limit {
		return []string{text}
	}

	remaining := []rune(text)
	parts := make([]string, 0, len(remaining)/limit+1)
	for len(remaining) > limit {
		cut := limit
		for i := limit; i > 0; i-- {
			if remaining[i-1] == '\n' {
				cut = i - 1
				break
			}
		}
		if cut == 0 {
			cut = limit
		}
		part := strings.TrimRight(string(remaining[:cut]), "\n")
		if part != "" {
			parts = append(parts, part)
		}
		remaining = remaining[cut:]
		if len(remaining) > 0 && remaining[0] == '\n' {
			remaining = remaining[1:]
		}
	}
	if len(remaining) > 0 {
		parts = append(parts, string(remaining))
	}
	return parts
}

func appendProgressLine(lines *[]string, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	*lines = append(*lines, text)
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

func suffixWithText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return ": " + text
}
