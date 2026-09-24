package swarmslack

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/slack"
	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

func TestRootStopButtonWorksWithoutQuestionAndDoesNotRepeatStop(t *testing.T) {
	ctx := context.Background()
	store, feature := newHumanTransportStore(t)
	h := &humanIngress{bridge: &Bridge{Store: store, Features: []swarm.Feature{feature}}, store: store, workspace: "W"}
	click := slack.Event{ID: "delivery-1", WorkspaceID: "W", ChannelID: feature.ChannelID, ThreadTS: feature.ThreadTS, Timestamp: "100.000001", UserID: "U", FeatureControl: &slack.FeatureControl{FeatureID: feature.FeatureID, AnchorTS: feature.ThreadTS, ControlID: "stop"}}
	if err := h.handle(ctx, click); err != nil {
		t.Fatal(err)
	}
	if err := h.handle(ctx, click); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	click.ID, click.Timestamp = "delivery-2", "100.000002"
	if err := h.handle(ctx, click); err != nil {
		t.Fatalf("second click: %v", err)
	}
	history, err := store.History(ctx, feature.FeatureID)
	if err != nil || !history.Feature.Stopped || countOwnerInputs(history) != 0 {
		t.Fatalf("stop result=%+v err=%v", history.Feature, err)
	}
	stops := 0
	for _, a := range history.Audit {
		if a.Event == "feature_stopped" {
			stops++
		}
	}
	if stops != 1 {
		t.Fatalf("feature stopped %d times", stops)
	}
	click.FeatureControl.FeatureID = swarm.NewID()
	click.Timestamp = "100.000003"
	if err := h.handle(ctx, click); err == nil {
		t.Fatal("foreign feature button accepted")
	}
	click.FeatureControl.FeatureID = feature.FeatureID
	click.FeatureControl.AnchorTS = "2.000001"
	if err := h.handle(ctx, click); err == nil {
		t.Fatal("foreign anchor button accepted")
	}
}

func TestStoppedThreadDeliversStopAndLaterMessagesAfterContinue(t *testing.T) {
	ctx := context.Background()
	store, feature := newHumanTransportStore(t)
	if err := store.SetRecoveryBarrier(ctx, feature.FeatureID, ""); err != nil {
		t.Fatal(err)
	}
	h := &humanIngress{bridge: &Bridge{Store: store, Features: []swarm.Feature{feature}}, store: store, workspace: "W"}
	commit := func(ts, body string) {
		t.Helper()
		_, err := h.commit(ctx, feature, slack.Event{WorkspaceID: "W", ChannelID: feature.ChannelID, ThreadTS: feature.ThreadTS, Timestamp: ts, UserID: "U", Text: body})
		if err != nil {
			t.Fatal(err)
		}
	}
	commit("100.000001", "!stop")
	commit("100.000002", "Please remember this while paused")
	if history, err := store.History(ctx, feature.FeatureID); err != nil || !history.Feature.Stopped || countOwnerInputs(history) != 0 {
		t.Fatalf("stopped feature started owner work: %+v %v", history, err)
	}
	if err := h.processPending(ctx, feature, false); err != nil {
		t.Fatal(err)
	}
	if history, err := store.History(ctx, feature.FeatureID); err != nil || countOwnerInputs(history) != 0 {
		t.Fatalf("paused message escaped before continue: %+v %v", history, err)
	}
	commit("100.000003", "!continue")
	if err := h.processPending(ctx, feature, false); err != nil {
		t.Fatal(err)
	}
	history, err := store.History(ctx, feature.FeatureID)
	if err != nil || history.Feature.Stopped || countOwnerInputs(history) != 3 {
		t.Fatalf("reopen did not deliver complete conversation: %+v %v", history, err)
	}
	var inputs []swarm.InputPayload
	for _, message := range history.Messages {
		if message.Type != "agent.input" {
			continue
		}
		var input swarm.InputPayload
		if err := json.Unmarshal(message.Payload, &input); err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, input)
	}
	if len(inputs) != 3 || inputs[0].Text != "!stop" || inputs[1].Text != "!continue" || inputs[2].Text != "Please remember this while paused" {
		t.Fatalf("reopened input order=%+v", inputs)
	}
	if err := h.processPending(ctx, feature, false); err != nil {
		t.Fatal(err)
	}
	if history, err := store.History(ctx, feature.FeatureID); err != nil || countOwnerInputs(history) != 3 {
		t.Fatalf("replay duplicated owner input: %+v %v", history, err)
	}
}

func TestPendingOlderContinueCannotReopenAfterNewerStop(t *testing.T) {
	ctx := context.Background()
	store, feature := newHumanTransportStore(t)
	if err := store.SetRecoveryBarrier(ctx, feature.FeatureID, ""); err != nil {
		t.Fatal(err)
	}
	h := &humanIngress{bridge: &Bridge{Store: store, Features: []swarm.Feature{feature}}, store: store, workspace: "W"}
	old := swarm.SlackSource{WorkspaceID: "W", ChannelID: feature.ChannelID, ThreadTS: feature.ThreadTS, MessageTS: "100.000001", FeatureID: feature.FeatureID, ActorID: "U", Text: "!continue"}
	if _, err := store.CommitSlackSource(ctx, old); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after urgent owner ingress but before source settlement.
	input, err := h.ownerInput(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.IngestUrgent(ctx, feature.FeatureID, input); err != nil {
		t.Fatal(err)
	}
	if _, err := h.commit(ctx, feature, slack.Event{WorkspaceID: "W", ChannelID: feature.ChannelID, ThreadTS: feature.ThreadTS, Timestamp: "100.000002", UserID: "U", Text: "!stop"}); err != nil {
		t.Fatal(err)
	}
	if err := h.processPending(ctx, feature, false); err != nil {
		t.Fatal(err)
	}
	history, err := store.History(ctx, feature.FeatureID)
	if err != nil || !history.Feature.Stopped {
		t.Fatalf("older continue reopened after newer stop: %+v %v", history, err)
	}
	pending, err := store.PendingSlackSources(ctx, feature.FeatureID)
	if err != nil || len(pending) != 0 {
		t.Fatalf("stale source was not settled: %+v %v", pending, err)
	}
}

type blockingStopLookupStore struct {
	*swarm.Store
	lookupStarted chan struct{}
	releaseLookup chan struct{}
}

func (s *blockingStopLookupStore) NewerStopSource(ctx context.Context, featureID, ts string) (bool, error) {
	close(s.lookupStarted)
	select {
	case <-s.releaseLookup:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	return s.Store.NewerStopSource(ctx, featureID, ts)
}

func TestStopRacingContinueLeavesFeatureStopped(t *testing.T) {
	ctx := context.Background()
	store, feature := newHumanTransportStore(t)
	if err := store.SetRecoveryBarrier(ctx, feature.FeatureID, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.StopFeature(ctx, feature.FeatureID, "U", "initial pause"); err != nil {
		t.Fatal(err)
	}
	continueSource := swarm.SlackSource{WorkspaceID: "W", ChannelID: feature.ChannelID, ThreadTS: feature.ThreadTS, MessageTS: "100.000002", FeatureID: feature.FeatureID, ActorID: "U", Text: "!continue"}
	if _, err := store.CommitSlackSource(ctx, continueSource); err != nil {
		t.Fatal(err)
	}
	wrapped := &blockingStopLookupStore{Store: store, lookupStarted: make(chan struct{}), releaseLookup: make(chan struct{})}
	h := &humanIngress{bridge: &Bridge{Store: wrapped, Features: []swarm.Feature{feature}}, store: wrapped, workspace: "W"}
	continueDone := make(chan error, 1)
	go func() { continueDone <- h.processOne(ctx, continueSource, false) }()
	select {
	case <-wrapped.lookupStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("continue did not reach serialized stop check")
	}
	stopDone := make(chan error, 1)
	go func() {
		_, err := h.commit(ctx, feature, slack.Event{WorkspaceID: "W", ChannelID: feature.ChannelID, ThreadTS: feature.ThreadTS, Timestamp: "100.000003", UserID: "U", Text: "!stop"})
		stopDone <- err
	}()
	select {
	case err := <-stopDone:
		t.Fatalf("stop passed through locked continue: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(wrapped.releaseLookup)
	if err := <-continueDone; err != nil {
		t.Fatal(err)
	}
	if err := <-stopDone; err != nil {
		t.Fatal(err)
	}
	history, err := store.History(ctx, feature.FeatureID)
	if err != nil || !history.Feature.Stopped {
		t.Fatalf("racing stop did not win: %+v %v", history, err)
	}
}

func countOwnerInputs(history swarm.History) int {
	n := 0
	for _, message := range history.Messages {
		if message.Type == "agent.input" {
			n++
		}
	}
	return n
}
