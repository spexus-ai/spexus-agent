package swarmslack

import (
	"context"
	"github.com/spexus-ai/spexus-agent/internal/slack"
	"github.com/spexus-ai/spexus-agent/internal/swarm"
	"sync"
	"testing"
	"time"
)

type blockedPublication struct {
	started chan struct{}
	once    sync.Once
}

func (a *blockedPublication) Post(ctx context.Context, d swarm.SlackDelivery) (string, error) {
	a.once.Do(func() { close(a.started) })
	<-ctx.Done()
	return "", ctx.Err()
}
func (a *blockedPublication) Find(context.Context, swarm.SlackDelivery) (string, bool, error) {
	return "", false, nil
}

type sourceFixture struct{ events chan slack.Event }

func (s *sourceFixture) Events(context.Context) (<-chan slack.Event, error) { return s.events, nil }
func (s *sourceFixture) Close() error                                       { return nil }

type stopStore struct {
	stopped chan struct{}
	once    sync.Once
}

func (s *stopStore) Ingest(context.Context, string, swarm.InputPayload) (swarm.Receipt, bool, error) {
	return swarm.Receipt{}, false, nil
}
func (s *stopStore) StopFeature(context.Context, string, string, string) error {
	s.once.Do(func() { close(s.stopped) })
	return nil
}
func (s *stopStore) ContinueFeature(context.Context, string, string) error          { return nil }
func (s *stopStore) QueueSlackNotice(context.Context, string, string, string) error { return nil }
func (s *stopStore) History(context.Context, string) (swarm.History, error) {
	return swarm.History{}, nil
}
func (s *stopStore) ClaimSlack(context.Context) (*swarm.SlackDelivery, error) {
	return &swarm.SlackDelivery{}, nil
}
func (s *stopStore) SettleSlack(context.Context, string, string, string) error { return nil }
func TestBlockedSlackPublicationDoesNotDelayStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := &blockedPublication{started: make(chan struct{})}
	store := &stopStore{stopped: make(chan struct{})}
	source := &sourceFixture{events: make(chan slack.Event, 1)}
	bridge := &Bridge{Store: store, API: api, Features: []swarm.Feature{{FeatureID: "fixture", ChannelID: "C", ThreadTS: "1.2", AllowedActorIDs: []string{"U"}}}}
	done := make(chan error, 1)
	go func() { done <- bridge.Run(ctx, source) }()
	select {
	case <-api.started:
	case <-time.After(3 * time.Second):
		t.Fatal("publication did not start")
	}
	source.events <- slack.Event{ID: "Ev-stop", ChannelID: "C", ThreadTS: "1.2", UserID: "U", Text: "!stop"}
	select {
	case <-store.stopped:
	case <-time.After(time.Second):
		t.Fatal("slow Slack API delayed out-of-band stop")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("bridge did not shut down")
	}
}
