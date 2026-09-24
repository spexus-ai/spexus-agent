package swarmslack

import (
	"context"
	"errors"
	"fmt"
	"github.com/spexus-ai/spexus-agent/internal/slack"
	"github.com/spexus-ai/spexus-agent/internal/swarm"
	"strings"
	"time"
)

type Store interface {
	Ingest(context.Context, string, swarm.InputPayload) (swarm.Receipt, bool, error)
	StopFeature(context.Context, string, string, string) error
	ContinueFeature(context.Context, string, string) error
	QueueSlackNotice(context.Context, string, string, string) error
	History(context.Context, string) (swarm.History, error)
	ClaimSlack(context.Context) (*swarm.SlackDelivery, error)
	SettleSlack(context.Context, string, string, string) error
}
type DeliveryAPI interface {
	Post(context.Context, swarm.SlackDelivery) (string, error)
	Find(context.Context, swarm.SlackDelivery) (string, bool, error)
}
type Bridge struct {
	Store    Store
	Features []swarm.Feature
	API      DeliveryAPI
	Logf     func(string, ...any)
}

func (b *Bridge) log(format string, args ...any) {
	if b.Logf != nil {
		b.Logf(format, args...)
	}
}
func (b *Bridge) Handle(ctx context.Context, event slack.Event) (resultErr error) {
	var f *swarm.Feature
	for i := range b.Features {
		candidate := &b.Features[i]
		if candidate.ChannelID == event.ChannelID && candidate.ThreadTS == event.ThreadTimestamp() {
			f = candidate
			break
		}
	}
	if f == nil {
		return nil
	}
	allowed := false
	for _, actor := range f.AllowedActorIDs {
		if actor == event.UserID {
			allowed = true
		}
	}
	if !allowed {
		return nil
	} // Never reveal the private feature to another actor.
	if event.ID == "" {
		return errors.New("slack event missing stable ID")
	}
	defer func() {
		if resultErr == nil {
			return
		}
		code := "runtime_unavailable"
		var apiErr *swarm.APIError
		if errors.As(resultErr, &apiErr) {
			code = apiErr.Code
		}
		if err := b.Store.QueueSlackNotice(ctx, f.FeatureID, event.ID, "Request rejected: "+code+". Use !status to inspect current work."); err != nil {
			b.log("could not persist Slack rejection notice")
		}
	}()
	text := strings.TrimSpace(event.Text)
	switch text {
	case "!status":
		h, err := b.Store.History(ctx, f.FeatureID)
		if err != nil {
			return err
		}
		var s strings.Builder
		fmt.Fprintf(&s, "Feature %s · owner %s · stopped=%t", f.FeatureID, f.OwnerAgentID, h.Feature.Stopped)
		for _, j := range h.Jobs {
			for _, a := range j.Attempts {
				fmt.Fprintf(&s, "\njob %s · attempt %s · worker %s · %s · review=%s", j.JobID, a.AttemptID, a.AssignedAgentID, a.State, a.Review)
			}
		}
		return b.notice(ctx, *f, event.ID, s.String())
	case "!stop":
		if err := b.Store.StopFeature(ctx, f.FeatureID, event.UserID, "Slack !stop"); err != nil {
			return err
		}
		return b.notice(ctx, *f, event.ID, "Stop requested. Running work is pending cancellation until its process confirms completion.")
	case "!continue":
		if err := b.Store.ContinueFeature(ctx, f.FeatureID, event.UserID); err != nil {
			return err
		}
		return b.notice(ctx, *f, event.ID, "Feature reopened. No earlier model call was replayed; send a new instruction after checking the history.")
	}
	_, _, err := b.Store.Ingest(ctx, f.FeatureID, swarm.InputPayload{Text: event.Text, Source: swarm.Source{Kind: "slack", EventID: event.ID, ChannelID: event.ChannelID, ThreadTS: event.ThreadTimestamp(), ActorID: event.UserID}})
	return err
}
func (b *Bridge) notice(ctx context.Context, f swarm.Feature, eventID, text string) error {
	return b.Store.QueueSlackNotice(ctx, f.FeatureID, eventID, text)
}

// DeliverOne never retries a request with an uncertain Slack outcome. Only a
// complete correlation lookup can requeue it; unavailable history leaves it
// visible in the operator history as delivery_unknown.
func (b *Bridge) DeliverOne(ctx context.Context) error {
	d, err := b.Store.ClaimSlack(ctx)
	if err != nil || d == nil {
		return err
	}
	ts, err := b.API.Post(ctx, *d)
	if err != nil {
		if settle := b.Store.SettleSlack(ctx, d.ID, "delivery_unknown", ""); settle != nil {
			return settle
		}
		return err
	}
	return b.Store.SettleSlack(ctx, d.ID, "sent", ts)
}
func (b *Bridge) Reconcile(ctx context.Context) error {
	for _, f := range b.Features {
		h, err := b.Store.History(ctx, f.FeatureID)
		if err != nil {
			return err
		}
		for _, d := range h.SlackOutbox {
			if d.Status != "delivery_unknown" {
				continue
			}
			ts, found, err := b.API.Find(ctx, d)
			if err != nil {
				return err
			}
			status := "queued"
			if found {
				status = "sent"
			}
			if err = b.Store.SettleSlack(ctx, d.ID, status, ts); err != nil {
				return err
			}
		}
	}
	return nil
}
func (b *Bridge) Run(ctx context.Context, source slack.EventSource) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events, err := source.Events(ctx)
	if err != nil {
		return err
	}
	defer source.Close()
	// Slack publication/history may be slow. It must never block ingress of
	// !stop and the coordinator's out-of-band control mailbox.
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		reconcile := time.NewTicker(30 * time.Second)
		defer reconcile.Stop()
		if err := b.Reconcile(ctx); err != nil {
			b.log("slack reconciliation pending: %v", err)
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if err := b.DeliverOne(ctx); err != nil {
					b.log("slack delivery requires reconciliation: %v", err)
				}
			case <-reconcile.C:
				if err := b.Reconcile(ctx); err != nil {
					b.log("slack reconciliation pending: %v", err)
				}
			}
		}
	}()
	defer func() { cancel(); <-done }()
	for {
		select {
		case <-ctx.Done():
			return nil
		case e, ok := <-events:
			if !ok {
				return errors.New("slack source closed")
			}
			if err := b.Handle(ctx, e); err != nil {
				b.log("slack input rejected: %v", err)
			}
		}
	}
}
