package runtime

import (
	"context"
	"strings"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/slack"
)

const (
	defaultSlackProgressFlushEventCount = 4
	defaultSlackProgressFlushInterval   = 2 * time.Second
	streamingSlackMessageSoftLimit      = 2000
)

type SlackThreadProgressPublisherConfig struct {
	FlushEventCount int
	FlushInterval   time.Duration
	Now             func() time.Time
}

type SlackThreadProgressPublisher struct {
	client          slack.Client
	req             SlackThreadRenderRequest
	flushEventCount int
	flushInterval   time.Duration
	now             func() time.Time

	progress           []string
	assistantProgress  strings.Builder
	assistantSawChunks bool
	finalParts         []string
	terminal           string
	sessionDone        bool
	pendingCount       int
}

func (r SlackThreadRenderer) NewProgressPublisher(req SlackThreadRenderRequest, cfg SlackThreadProgressPublisherConfig) (*SlackThreadProgressPublisher, error) {
	if err := validateSlackThreadRenderRequest(r.Client, req); err != nil {
		return nil, err
	}

	flushEventCount := cfg.FlushEventCount
	if flushEventCount <= 0 {
		flushEventCount = defaultSlackProgressFlushEventCount
	}

	flushInterval := cfg.FlushInterval
	if flushInterval <= 0 {
		flushInterval = defaultSlackProgressFlushInterval
	}

	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	return &SlackThreadProgressPublisher{
		client:          r.Client,
		req:             req,
		flushEventCount: flushEventCount,
		flushInterval:   flushInterval,
		now:             now,
		progress:        make([]string, 0, flushEventCount),
		finalParts:      make([]string, 0, 2),
	}, nil
}

func (p *SlackThreadProgressPublisher) Consume(ctx context.Context, event ACPXTurnEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	switch event.Kind {
	case ACPXEventSessionStarted:
		p.appendProgress("Session started"+suffixWithText(event.Text), true)
	case ACPXEventAssistantThinking:
		p.appendProgress("Thinking"+suffixWithText(event.Text), true)
	case ACPXEventToolStarted:
		return nil
	case ACPXEventToolFinished:
		return nil
	case ACPXEventAssistantMessageChunk:
		p.appendAssistantChunk(event.Text)
	case ACPXEventAssistantMessageFinal:
		if text := strings.TrimSpace(event.Text); text != "" {
			p.finalParts = append(p.finalParts, text)
		}
	case ACPXEventSessionDone:
		p.sessionDone = true
	case ACPXEventSessionError:
		p.finalParts = p.finalParts[:0]
		p.terminal = "Session error" + suffixWithText(event.Text)
	case ACPXEventSessionCancelled:
		p.finalParts = p.finalParts[:0]
		p.terminal = "Session cancelled" + suffixWithText(event.Text)
	}

	return nil
}

func (p *SlackThreadProgressPublisher) HasPendingProgress() bool {
	if len(p.progress) > 0 {
		return true
	}
	return hasFlushableSlackStreamChunk(p.assistantProgress.String())
}

func (p *SlackThreadProgressPublisher) PendingProgressCount() int {
	return p.pendingCount
}

func (p *SlackThreadProgressPublisher) FlushEventCount() int {
	return p.flushEventCount
}

func (p *SlackThreadProgressPublisher) FlushInterval() time.Duration {
	return p.flushInterval
}

func (p *SlackThreadProgressPublisher) ShouldFlushByCount() bool {
	return p.pendingCount >= p.flushEventCount && p.HasPendingProgress()
}

func (p *SlackThreadProgressPublisher) HasNonAssistantProgress() bool {
	return len(p.progress) > 0
}

func (p *SlackThreadProgressPublisher) Flush(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.flush(ctx, true, false)
}

func (p *SlackThreadProgressPublisher) Finish(ctx context.Context, terminalErr error) error {
	if terminalErr != nil && p.terminal == "" {
		p.finalParts = p.finalParts[:0]
		p.terminal = "Session error" + suffixWithText(terminalErr.Error())
	}

	switch {
	case p.terminal != "":
		if err := p.flush(ctx, true, true); err != nil {
			return err
		}
	case p.assistantSawChunks:
		if err := p.flush(ctx, true, true); err != nil {
			return err
		}
	case len(p.finalParts) > 0 || p.sessionDone:
		if err := p.flush(ctx, false, false); err != nil {
			return err
		}
	default:
		if err := p.flush(ctx, true, true); err != nil {
			return err
		}
	}

	text := ""
	switch {
	case p.terminal != "":
		text = p.terminal
	case p.assistantSawChunks:
		return nil
	case len(p.finalParts) > 0:
		text = strings.Join(p.finalParts, "\n\n")
	case p.sessionDone:
		text = "Session complete."
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}

	return p.client.PostThreadMessage(ctx, slack.Message{
		ChannelID: p.req.ChannelID,
		ThreadTS:  p.req.ThreadTS,
		Text:      text,
	})
}

func (p *SlackThreadProgressPublisher) appendProgress(text string, nonAssistant bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	p.progress = append(p.progress, text)
	p.pendingCount++
}

func (p *SlackThreadProgressPublisher) appendAssistantChunk(text string) {
	if text == "" {
		return
	}
	p.assistantSawChunks = true

	if p.assistantProgress.Len() == 0 {
		if strings.TrimSpace(text) == "" {
			return
		}
	}
	p.assistantProgress.WriteString(text)
	p.pendingCount++
}

func (p *SlackThreadProgressPublisher) flush(ctx context.Context, includeAssistant bool, forceAssistantTail bool) error {
	if len(p.progress) > 0 {
		text := formatBatchMessage("Progress", p.progress)
		if strings.TrimSpace(text) != "" {
			if err := p.client.PostThreadMessage(ctx, slack.Message{
				ChannelID: p.req.ChannelID,
				ThreadTS:  p.req.ThreadTS,
				Text:      text,
			}); err != nil {
				return err
			}
		}
		p.progress = p.progress[:0]
	}
	if includeAssistant {
		chunks, remainder := splitSlackStreamingText(p.assistantProgress.String(), forceAssistantTail)
		for _, chunk := range chunks {
			if err := p.client.PostThreadMessage(ctx, slack.Message{
				ChannelID: p.req.ChannelID,
				ThreadTS:  p.req.ThreadTS,
				Text:      chunk,
			}); err != nil {
				return err
			}
		}
		p.assistantProgress.Reset()
		if remainder != "" {
			p.assistantProgress.WriteString(remainder)
		}
	}
	p.pendingCount = 0
	return nil
}

func hasFlushableSlackStreamChunk(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	runes := []rune(text)
	if len(runes) > streamingSlackMessageSoftLimit {
		return true
	}
	for _, r := range runes {
		if r == '\n' {
			return true
		}
	}
	return false
}

func splitSlackStreamingText(text string, flushTail bool) ([]string, string) {
	remaining := []rune(text)
	chunks := make([]string, 0, len(remaining)/streamingSlackMessageSoftLimit+1)

	for len(remaining) > 0 {
		cut := -1
		if len(remaining) > streamingSlackMessageSoftLimit {
			cut = streamingSlackMessageSoftLimit
			for i := streamingSlackMessageSoftLimit; i > 0; i-- {
				if remaining[i-1] == '\n' {
					cut = i - 1
					break
				}
			}
		} else {
			for i := len(remaining); i > 0; i-- {
				if remaining[i-1] == '\n' {
					cut = i - 1
					break
				}
			}
			if cut < 0 && !flushTail {
				break
			}
			if cut < 0 {
				cut = len(remaining)
			}
		}

		if cut == 0 && len(remaining) > 0 && remaining[0] == '\n' {
			remaining = remaining[1:]
			continue
		}
		if cut <= 0 {
			cut = minInt(len(remaining), streamingSlackMessageSoftLimit)
		}

		chunk := strings.TrimRight(string(remaining[:cut]), "\n")
		if strings.TrimSpace(chunk) != "" {
			chunks = append(chunks, chunk)
		}
		remaining = remaining[cut:]
		if len(remaining) > 0 && remaining[0] == '\n' {
			remaining = remaining[1:]
		}
		if !flushTail && len(remaining) <= streamingSlackMessageSoftLimit {
			hasNewline := false
			for _, r := range remaining {
				if r == '\n' {
					hasNewline = true
					break
				}
			}
			if !hasNewline {
				break
			}
		}
	}

	return chunks, string(remaining)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
