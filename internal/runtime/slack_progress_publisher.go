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

	progress                 []string
	flushedProgress          []string
	assistantProgress        strings.Builder
	flushedAssistantProgress string
	finalParts               []string
	terminal                 string
	sessionDone              bool
	pendingCount             int
	progressMessageTS        string
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
		p.appendProgress(formatToolLine("started", event), true)
	case ACPXEventToolFinished:
		p.appendProgress(formatToolLine("finished", event), true)
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
	assistant := strings.TrimSpace(p.assistantProgress.String())
	return len(p.progress) > 0 || assistant != p.flushedAssistantProgress
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
	return p.flush(ctx, true)
}

func (p *SlackThreadProgressPublisher) Finish(ctx context.Context, terminalErr error) error {
	if terminalErr != nil && p.terminal == "" {
		p.finalParts = p.finalParts[:0]
		p.terminal = "Session error" + suffixWithText(terminalErr.Error())
	}

	switch {
	case p.terminal != "":
		if err := p.flush(ctx, true); err != nil {
			return err
		}
	case len(p.finalParts) > 0 || p.sessionDone:
		if err := p.flush(ctx, false); err != nil {
			return err
		}
		p.assistantProgress.Reset()
		p.flushedAssistantProgress = ""
		p.pendingCount = 0
	default:
		if err := p.flush(ctx, true); err != nil {
			return err
		}
	}

	text := ""
	switch {
	case p.terminal != "":
		text = p.terminal
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

	if p.assistantProgress.Len() == 0 {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		p.assistantProgress.WriteString(text)
	} else {
		p.assistantProgress.WriteString(text)
	}

	p.pendingCount++
}

func (p *SlackThreadProgressPublisher) flush(ctx context.Context, includeAssistant bool) error {
	if !p.HasPendingProgress() {
		p.pendingCount = 0
		return nil
	}

	progressLines := append([]string(nil), p.flushedProgress...)
	progressLines = append(progressLines, p.progress...)
	assistant := ""
	if includeAssistant {
		assistant = strings.TrimSpace(p.assistantProgress.String())
	}

	if len(progressLines) == 0 && assistant == "" {
		p.progress = p.progress[:0]
		if includeAssistant {
			p.assistantProgress.Reset()
			p.flushedAssistantProgress = ""
		}
		p.pendingCount = 0
		return nil
	}

	text := formatLiveProgressMessage(progressLines, assistant)
	if strings.TrimSpace(text) == "" {
		p.progress = p.progress[:0]
		if includeAssistant {
			p.assistantProgress.Reset()
			p.flushedAssistantProgress = ""
		}
		p.pendingCount = 0
		return nil
	}

	if err := p.postOrUpdateProgress(ctx, text); err != nil {
		return err
	}

	p.progress = p.progress[:0]
	p.flushedProgress = progressLines
	if includeAssistant {
		p.flushedAssistantProgress = assistant
	}
	p.pendingCount = 0
	return nil
}

func (p *SlackThreadProgressPublisher) postOrUpdateProgress(ctx context.Context, text string) error {
	if p.progressMessageTS != "" {
		if updater, ok := p.client.(slack.MessageUpdater); ok {
			return updater.UpdateMessage(ctx, slack.MessageUpdate{
				ChannelID: p.req.ChannelID,
				Timestamp: p.progressMessageTS,
				Text:      text,
			})
		}
		return p.client.PostThreadMessage(ctx, slack.Message{
			ChannelID: p.req.ChannelID,
			ThreadTS:  p.req.ThreadTS,
			Text:      text,
		})
	}

	posted, err := p.client.PostMessage(ctx, slack.Message{
		ChannelID: p.req.ChannelID,
		ThreadTS:  p.req.ThreadTS,
		Text:      text,
	})
	if err != nil {
		return err
	}
	p.progressMessageTS = strings.TrimSpace(posted.Timestamp)
	return nil
}
