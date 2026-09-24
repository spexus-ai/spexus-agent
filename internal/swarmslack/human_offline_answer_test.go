package swarmslack

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/slack"
	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

type offlineAnswerStore struct {
	*swarm.Store
	featureID string
	mu        sync.Mutex
	inputs    []swarm.InputPayload
	barrier   []string
}

func (s *offlineAnswerStore) IngestUrgent(ctx context.Context, featureID string, in swarm.InputPayload) (swarm.Receipt, bool, error) {
	receipt, duplicate, err := s.Store.IngestUrgent(ctx, featureID, in)
	if err != nil || duplicate {
		return receipt, duplicate, err
	}
	h, err := s.History(ctx, s.featureID)
	if err != nil {
		return receipt, duplicate, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inputs = append(s.inputs, in)
	s.barrier = append(s.barrier, h.RecoveryBarrier)
	return receipt, duplicate, nil
}

type offlineAnswerAPI struct {
	answerTS string
	text     string
}

func (*offlineAnswerAPI) VerifyWorkspace(context.Context, string) error { return nil }
func (a *offlineAnswerAPI) ScanThread(_ context.Context, _, thread, _ string, visit func(string, string, string, string, bool) error) (string, error) {
	return a.answerTS, visit(a.answerTS, "U", a.text, thread, false)
}
func (*offlineAnswerAPI) Post(context.Context, swarm.SlackDelivery) (string, error) {
	return "10.000001", nil
}
func (*offlineAnswerAPI) Find(context.Context, swarm.SlackDelivery) (string, bool, error) {
	return "", false, nil
}

// A Slack message posted while the coordinator is offline appears only in
// thread history. Catchup must commit and queue its full text before opening
// the execution barrier. Socket redelivery remains inert.
func TestOfflineHumanMessageHistoryCatchupBeforeResume(t *testing.T) {
	store, feature := newHumanTransportStore(t)
	wrapped := &offlineAnswerStore{Store: store, featureID: feature.FeatureID}
	requestID := swarm.NewID()
	api := &offlineAnswerAPI{answerTS: "9999999999.000002", text: "!answer " + requestID + " a"}
	bridge := &Bridge{Store: wrapped, Features: []swarm.Feature{feature}, API: api}
	source := &durableFixture{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- bridge.RunHuman(ctx, source, "W") }()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		wrapped.mu.Lock()
		count := len(wrapped.inputs)
		wrapped.mu.Unlock()
		h, err := store.History(ctx, feature.FeatureID)
		if err == nil && count == 1 && h.RecoveryBarrier == "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	wrapped.mu.Lock()
	if len(wrapped.inputs) != 1 || wrapped.inputs[0].Text != api.text || wrapped.inputs[0].Source.ActorID != "U" || wrapped.inputs[0].Source.MessageTS != api.answerTS || len(wrapped.barrier) != 1 || wrapped.barrier[0] != "slack_catchup" {
		wrapped.mu.Unlock()
		t.Fatalf("offline message not queued under catchup barrier: inputs=%+v barrier=%v", wrapped.inputs, wrapped.barrier)
	}
	wrapped.mu.Unlock()
	if err := source.Send(ctx, slack.Event{ID: "socket-copy", WorkspaceID: "W", ChannelID: feature.ChannelID, ThreadTS: feature.ThreadTS, Timestamp: api.answerTS, UserID: "U", Text: api.text}); err != nil {
		t.Fatal(err)
	}
	wrapped.mu.Lock()
	count := len(wrapped.inputs)
	wrapped.mu.Unlock()
	if count != 1 {
		t.Fatalf("redelivery applied answer %d times", count)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
