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
	answers   []swarm.HumanAnswerInput
	barrier   []string
}

func (s *offlineAnswerStore) RecordHumanAnswer(ctx context.Context, in swarm.HumanAnswerInput) (string, bool, error) {
	h, err := s.History(ctx, s.featureID)
	if err != nil {
		return "", false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answers = append(s.answers, in)
	s.barrier = append(s.barrier, h.RecoveryBarrier)
	return swarm.NewID(), false, nil
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

// A Slack text answer posted while the coordinator is offline appears only in
// thread history. Catchup must commit and reduce that source before opening
// the execution barrier. Socket redelivery of the same message remains inert.
func TestOfflineHumanAnswerHistoryCatchupBeforeResume(t *testing.T) {
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
		count := len(wrapped.answers)
		wrapped.mu.Unlock()
		h, err := store.History(ctx, feature.FeatureID)
		if err == nil && count == 1 && h.RecoveryBarrier == "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	wrapped.mu.Lock()
	if len(wrapped.answers) != 1 || wrapped.answers[0].RequestID != requestID || wrapped.answers[0].ActorID != "U" || wrapped.answers[0].MessageTS != api.answerTS || len(wrapped.barrier) != 1 || wrapped.barrier[0] != "slack_catchup" {
		wrapped.mu.Unlock()
		t.Fatalf("offline answer not reduced under catchup barrier: answers=%+v barrier=%v", wrapped.answers, wrapped.barrier)
	}
	wrapped.mu.Unlock()
	if err := source.Send(ctx, slack.Event{ID: "socket-copy", WorkspaceID: "W", ChannelID: feature.ChannelID, ThreadTS: feature.ThreadTS, Timestamp: api.answerTS, UserID: "U", Text: api.text}); err != nil {
		t.Fatal(err)
	}
	wrapped.mu.Lock()
	count := len(wrapped.answers)
	wrapped.mu.Unlock()
	if count != 1 {
		t.Fatalf("redelivery applied answer %d times", count)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
