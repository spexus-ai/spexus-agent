package swarmslack

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/slack"
)

// RunLegacyDurable keeps existing wire-one executions while using committed
// Socket Mode delivery and a persisted per-thread history cursor.
func (b *Bridge) RunLegacyDurable(ctx context.Context, source slack.DurableEventSource) error {
	store, ok := b.Store.(HumanStore)
	if !ok || source == nil {
		return errors.New("durable Slack store or source is not configured")
	}
	history, ok := b.API.(ThreadHistoryAPI)
	if !ok {
		return errors.New("Slack thread history is not configured")
	}
	h := &humanIngress{bridge: b, store: store, statusUpdates: map[string]threadStatusMark{}}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer source.Close()
	defer h.clearThreadStatuses(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		reconcile := time.NewTicker(30 * time.Second)
		defer reconcile.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if err := b.DeliverOne(ctx); err != nil {
					b.log("Slack publication pending: %v", err)
				}
				for _, f := range b.Features {
					view, err := store.History(ctx, f.FeatureID)
					if err != nil {
						b.log("Slack status history pending: %v", err)
						continue
					}
					h.updateThreadStatus(ctx, f, desiredThreadStatus(view, false))
				}
			case <-reconcile.C:
				if err := b.Reconcile(ctx); err != nil {
					b.log("Slack publication reconciliation pending: %v", err)
				}
			}
		}
	}()
	defer func() { cancel(); <-done }()
	err := source.RunDurable(ctx, func(ctx context.Context) error {
		h.clearThreadStatuses(ctx)
		for _, f := range b.Features {
			watermark, err := store.SlackWatermark(ctx, f.FeatureID)
			if err != nil {
				return err
			}
			upper, err := history.ScanThread(ctx, f.ChannelID, f.ThreadTS, watermark, func(ts, actor, text, thread string, edited bool) error {
				if edited || !timestampAfter(ts, watermark) {
					return nil
				}
				return b.Handle(ctx, slack.Event{ID: ts, ChannelID: f.ChannelID, ThreadTS: thread, Timestamp: ts, UserID: actor, Text: text})
			})
			if err != nil {
				return fmt.Errorf("catch up Slack thread %s: %w", f.FeatureID, err)
			}
			if upper != "" && timestampAfter(upper, watermark) {
				if err := store.AdvanceSlackWatermark(ctx, f.FeatureID, upper); err != nil {
					return err
				}
			}
		}
		return nil
	}, func(ctx context.Context) error {
		h.clearThreadStatuses(ctx)
		return nil
	}, func(ctx context.Context, event slack.Event) error {
		if event.HumanAction != nil || event.FeatureControl != nil {
			return nil
		}
		if !validTimestamp(event.Timestamp) {
			return errors.New("Slack event has no stable message timestamp")
		}
		event.ID = event.Timestamp
		return b.Handle(ctx, event)
	})
	if ctx.Err() != nil {
		return nil
	}
	return err
}
