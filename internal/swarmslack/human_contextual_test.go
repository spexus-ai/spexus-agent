package swarmslack

import (
	"context"
	"strings"
	"testing"

	"github.com/spexus-ai/spexus-agent/internal/slack"
	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

type conversationRouteStore struct {
	*swarm.Store
	active     *swarm.HumanRequestContext
	inputs     []swarm.InputPayload
	urgent     []swarm.InputPayload
	interrupts int
	settled    int
	order      []string
	stopped    bool
	committed  swarm.SlackSource
	notices    []string
}

func (s *conversationRouteStore) CommitSlackSource(_ context.Context, in swarm.SlackSource) (bool, error) {
	s.order = append(s.order, "commit")
	s.committed = in
	return false, nil
}
func (s *conversationRouteStore) CommittedSlackSource(context.Context, string, string, string) (swarm.SlackSource, error) {
	return s.committed, nil
}
func (s *conversationRouteStore) History(context.Context, string) (swarm.History, error) {
	return swarm.History{Feature: swarm.Feature{Stopped: s.stopped}}, nil
}

func (s *conversationRouteStore) ActiveHumanRequest(context.Context, string) (*swarm.HumanRequestContext, error) {
	return s.active, nil
}
func (s *conversationRouteStore) Ingest(_ context.Context, _ string, input swarm.InputPayload) (swarm.Receipt, bool, error) {
	s.inputs = append(s.inputs, input)
	return swarm.Receipt{}, false, nil
}
func (s *conversationRouteStore) IngestUrgent(_ context.Context, _ string, input swarm.InputPayload) (swarm.Receipt, bool, error) {
	s.urgent = append(s.urgent, input)
	s.order = append(s.order, "ingest")
	return swarm.Receipt{}, false, nil
}
func (s *conversationRouteStore) InterruptOwnerTurn(context.Context, string, string, string) error {
	s.interrupts++
	s.order = append(s.order, "interrupt")
	return nil
}
func (s *conversationRouteStore) SettleSlackSource(context.Context, swarm.SlackSource) error {
	s.settled++
	return nil
}
func (s *conversationRouteStore) QueueSlackNotice(_ context.Context, _, _, message string) error {
	s.notices = append(s.notices, message)
	return nil
}

func TestEveryHumanReplyReachesOwnerWithoutAnswerParser(t *testing.T) {
	request := &swarm.HumanRequestContext{RequestID: swarm.NewID(), Question: "What should happen?", Kind: "clarification"}
	for _, body := range []string{
		"я не знаю, что такое P3",
		"ответ 2: я тебе ответил и я не знаю что такое P3.",
		"ответ #2 я тебе ответил что такое P3",
		"Отказ #0: не хочу",
		"Ответ #abc: да",
		"поясните вопрос",
	} {
		store := &conversationRouteStore{active: request}
		h := &humanIngress{store: store}
		source := swarm.SlackSource{WorkspaceID: "W", ChannelID: "C", ThreadTS: "1.000001", MessageTS: "2.000001", FeatureID: swarm.NewID(), ActorID: "U", Text: body, ActiveHumanRequest: request}
		if err := h.processOne(context.Background(), source, false); err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		if len(store.inputs) != 1 || store.inputs[0].Text != body || store.inputs[0].ActiveHumanRequest == nil || store.inputs[0].ActiveHumanRequest.RequestID != request.RequestID || store.inputs[0].Source.MessageTS != source.MessageTS || store.settled != 1 || store.interrupts != 0 {
			t.Fatalf("%q not delivered as ordinary owner input: %+v", body, store)
		}
	}
}

func TestButtonIsStructuredOwnerInputNotDirectDecision(t *testing.T) {
	requestID := swarm.NewID()
	store := &conversationRouteStore{active: &swarm.HumanRequestContext{RequestID: requestID}}
	h := &humanIngress{store: store}
	source := swarm.SlackSource{WorkspaceID: "W", ChannelID: "C", ThreadTS: "1.000001", MessageTS: "2.000001", FeatureID: swarm.NewID(), ActorID: "U", SourceKind: "block_action", RequestID: requestID, OptionID: "no", ActiveHumanRequest: store.active}
	if err := h.processOne(context.Background(), source, false); err != nil {
		t.Fatal(err)
	}
	if len(store.inputs) != 1 || store.inputs[0].HumanAction == nil || store.inputs[0].HumanAction.RequestID != requestID || store.inputs[0].HumanAction.OptionID != "no" || store.inputs[0].Source.MessageTS != source.MessageTS || store.settled != 1 {
		t.Fatalf("button was not delivered to owner: %+v", store)
	}
}

func TestDetailsControlUsesCommittedQuestionWithoutOwnerTurn(t *testing.T) {
	store := &conversationRouteStore{}
	h := &humanIngress{store: store, workspace: "W"}
	requestID := swarm.NewID()
	request := &swarm.HumanRequestContext{RequestID: requestID, Question: "Which path?", Reason: "Scope is unclear", Context: "Two paths have different effects", Recommendation: "Choose the narrow path", BlockedWork: "Publish the result"}
	feature := swarm.Feature{FeatureID: swarm.NewID(), ChannelID: "C", ThreadTS: "1.000001", AllowedActorIDs: []string{"U"}}
	event := slack.Event{WorkspaceID: "W", ChannelID: "C", ThreadTS: "1.000001", Timestamp: "2.000001", UserID: "U", HumanAction: &slack.HumanAction{RequestID: requestID, ControlID: "details", QuestionTS: "1.000002"}}
	if _, err := h.commit(context.Background(), feature, event); err != nil {
		t.Fatal(err)
	}
	if store.committed.SourceKind != "button_control" || store.committed.RequestID != requestID || store.committed.OptionID != "details" || store.committed.Text != swarm.HumanDetailsControlText || store.interrupts != 0 {
		t.Fatalf("details button source=%+v interrupts=%d", store.committed, store.interrupts)
	}
	store.active = request
	store.committed.ActiveHumanRequest = request
	if err := h.processOne(context.Background(), store.committed, false); err != nil {
		t.Fatal(err)
	}
	if len(store.inputs) != 0 || len(store.notices) != 1 || store.settled != 1 || store.interrupts != 0 {
		t.Fatalf("details was not handled locally: inputs=%+v notices=%+v settled=%d interrupts=%d", store.inputs, store.notices, store.settled, store.interrupts)
	}
	if got := store.notices[0]; !strings.Contains(got, request.Reason) || !strings.Contains(got, request.Context) || !strings.Contains(got, request.Recommendation) {
		t.Fatalf("details omitted committed context: %q", got)
	}
}

func TestDetailsControlAfterQuestionClosedDoesNotCreateOwnerTurn(t *testing.T) {
	store := &conversationRouteStore{}
	h := &humanIngress{store: store}
	source := swarm.SlackSource{ChannelID: "C", MessageTS: "2.000001", FeatureID: swarm.NewID(), SourceKind: "button_control", RequestID: swarm.NewID(), OptionID: "details", ActiveHumanRequest: &swarm.HumanRequestContext{RequestID: swarm.NewID()}}
	if err := h.processOne(context.Background(), source, false); err != nil {
		t.Fatal(err)
	}
	if len(store.inputs) != 0 || len(store.notices) != 1 || store.settled != 1 || !strings.Contains(store.notices[0], "закрыт") {
		t.Fatalf("closed question details: inputs=%+v notices=%+v settled=%d", store.inputs, store.notices, store.settled)
	}
}

func TestBangMessageInterruptsAndReachesOwnerUnchanged(t *testing.T) {
	for _, body := range []string{"!", "!status", "! ответьте срочно"} {
		store := &conversationRouteStore{}
		h := &humanIngress{store: store}
		source := swarm.SlackSource{WorkspaceID: "W", ChannelID: "C", ThreadTS: "1.000001", MessageTS: "2.000001", FeatureID: swarm.NewID(), ActorID: "U", Text: body}
		if err := h.processOne(context.Background(), source, false); err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		if len(store.urgent) != 1 || store.urgent[0].Text != body || store.interrupts != 1 || store.settled != 1 {
			t.Fatalf("%q not urgent owner input: %+v", body, store)
		}
	}
}

func TestRunningContinueQueuesFullTextBeforeInterruptAndSocketAck(t *testing.T) {
	store := &conversationRouteStore{}
	h := &humanIngress{store: store, workspace: "W"}
	feature := swarm.Feature{FeatureID: swarm.NewID(), ChannelID: "C", ThreadTS: "1.000001", AllowedActorIDs: []string{"U"}}
	event := slack.Event{WorkspaceID: "W", ChannelID: feature.ChannelID, ThreadTS: feature.ThreadTS, Timestamp: "2.000001", UserID: "U", Text: "!continue"}
	if _, err := h.commit(context.Background(), feature, event); err != nil {
		t.Fatal(err)
	}
	if len(store.order) != 3 || store.order[0] != "commit" || store.order[1] != "ingest" || store.order[2] != "interrupt" || len(store.urgent) != 1 || store.urgent[0].Text != "!continue" {
		t.Fatalf("running !continue did not interrupt before ACK: %+v", store)
	}
}

func TestLeadingSpaceBeforeBangDoesNotStopOrInterrupt(t *testing.T) {
	store := &conversationRouteStore{}
	h := &humanIngress{store: store, workspace: "W"}
	feature := swarm.Feature{FeatureID: swarm.NewID(), ChannelID: "C", ThreadTS: "1.000001", AllowedActorIDs: []string{"U"}}
	event := slack.Event{WorkspaceID: "W", ChannelID: feature.ChannelID, ThreadTS: feature.ThreadTS, Timestamp: "2.000001", UserID: "U", Text: " !stop"}
	if _, err := h.commit(context.Background(), feature, event); err != nil {
		t.Fatal(err)
	}
	if len(store.order) != 1 || store.order[0] != "commit" || store.interrupts != 0 {
		t.Fatalf("leading space falsely triggered immediate command: %+v", store)
	}
	if err := h.processOne(context.Background(), store.committed, false); err != nil {
		t.Fatal(err)
	}
	if len(store.inputs) != 1 || store.inputs[0].Text != " !stop" || len(store.urgent) != 0 {
		t.Fatalf("leading space was not ordinary owner input: %+v", store)
	}
}

func TestHumanQuestionPendingIsDistinctFromCanonicalTerminal(t *testing.T) {
	id := swarm.NewID()
	projection := swarm.HumanProjection{RequestID: id, BackendState: "open"}
	view := swarm.History{HumanSync: []swarm.HumanSyncStatus{{RequestID: id, Kind: "decision", Status: "pending"}}}
	if got := humanQuestionState(view, id, projection); got != "pending" {
		t.Fatalf("local journal state=%q", got)
	}
	view.HumanSync[0].Status = "blocked"
	if got := humanQuestionState(view, id, projection); got != "attention" {
		t.Fatalf("backend error state=%q", got)
	}
	projection.BackendState = "answered"
	if got := humanQuestionState(view, id, projection); got != "answered" {
		t.Fatalf("canonical terminal state=%q", got)
	}
}

type threadStatusFixture struct{ calls []string }

func (f *threadStatusFixture) Post(context.Context, swarm.SlackDelivery) (string, error) {
	return "", nil
}
func (f *threadStatusFixture) Find(context.Context, swarm.SlackDelivery) (string, bool, error) {
	return "", false, nil
}
func (f *threadStatusFixture) SetThreadStatus(_ context.Context, _, _, status string) error {
	f.calls = append(f.calls, status)
	return nil
}

func TestHumanTypingStatusIsBestEffortAndDoesNotSpam(t *testing.T) {
	api := &threadStatusFixture{}
	f := swarm.Feature{FeatureID: swarm.NewID(), ChannelID: "C123", ThreadTS: "1.000001"}
	h := &humanIngress{bridge: &Bridge{API: api, Features: []swarm.Feature{f}}}
	h.updateThreadStatus(context.Background(), f, "проверяет ответ…")
	h.updateThreadStatus(context.Background(), f, "проверяет ответ…")
	h.updateThreadStatus(context.Background(), f, "")
	h.updateThreadStatus(context.Background(), f, "")
	if len(api.calls) != 2 || api.calls[0] != "проверяет ответ…" || api.calls[1] != "" {
		t.Fatalf("typing status calls=%v", api.calls)
	}
}

func TestHumanThreadStatusFollowsOwnerAfterDecisionAndClearsWhileWaiting(t *testing.T) {
	view := swarm.History{Feature: swarm.Feature{OwnerAgentID: "owner"}, Agents: []swarm.AgentStatus{{AgentID: "owner", Status: "online"}}, HumanRequests: []swarm.HumanProjection{{BackendState: "open"}}}
	if got := desiredThreadStatus(view, false); got != "" {
		t.Fatalf("waiting human showed typing status %q", got)
	}
	view.Turns = []swarm.OwnerTurn{{TurnID: "current", State: "running"}}
	if got := desiredThreadStatus(view, false); got != "готовит ответ…" {
		t.Fatalf("running owner status=%q", got)
	}
	view.Agents[0].OwnerTurnID = "current"
	for _, phase := range []struct{ name, want string }{{"thinking", "оркестратор думает…"}, {"tool", "оркестратор использует инструмент…"}, {"responding", "оркестратор пишет ответ…"}} {
		view.Agents[0].ActivityPhase = phase.name
		if got := desiredThreadStatus(view, false); got != phase.want {
			t.Fatalf("phase %q status=%q", phase.name, got)
		}
	}
	view.Agents[0].OwnerTurnID = "stale"
	if got := desiredThreadStatus(view, false); got != "готовит ответ…" {
		t.Fatalf("stale phase status=%q", got)
	}
	if got := desiredThreadStatus(view, true); got != "готовит ответ…" {
		t.Fatalf("active owner status behind decision sync=%q", got)
	}
	view.Turns[0].State = "succeeded"
	if got := desiredThreadStatus(view, true); got != "проверяет ответ…" {
		t.Fatalf("idle decision status=%q", got)
	}
	if got := desiredThreadStatus(view, false); got != "" {
		t.Fatalf("finished owner status=%q", got)
	}
	view.Turns[0].State = "running"
	view.Agents[0].Status = "unreachable"
	if got := desiredThreadStatus(view, false); got != "" {
		t.Fatalf("offline owner falsely typing %q", got)
	}
	view.Agents[0].Status = "online"
	view.Feature.Stopped = true
	if got := desiredThreadStatus(view, false); got != "" {
		t.Fatalf("stopped feature status=%q", got)
	}
	view.Feature.Stopped = false
	view.RecoveryBarrier = "slack_disconnected"
	if got := desiredThreadStatus(view, true); got != "" {
		t.Fatalf("disconnected decision falsely typing %q", got)
	}
	if got := desiredThreadStatus(view, false); got != "" {
		t.Fatalf("disconnected owner falsely typing %q", got)
	}
}

func TestWorkerPiActivityAppearsInFeatureThread(t *testing.T) {
	view := swarm.History{Feature: swarm.Feature{OwnerAgentID: "owner"}, Agents: []swarm.AgentStatus{{AgentID: "owner", Status: "online"}, {AgentID: "worker-a", Status: "online", ActiveAttemptID: "attempt-a", ActivityPhase: "tool"}}, Jobs: []swarm.JobView{{CurrentAttemptID: "attempt-a", Attempts: []swarm.Attempt{{AttemptID: "attempt-a", AssignedAgentID: "worker-a", State: "running"}}}}}
	if got := desiredThreadStatus(view, false); got != "исполнитель использует инструмент…" {
		t.Fatalf("worker tool status=%q", got)
	}
	view.Agents[1].ActiveAttemptID = "stale"
	if got := desiredThreadStatus(view, false); got != "" {
		t.Fatalf("stale worker activity status=%q", got)
	}
	view.Agents[1].ActiveAttemptID = "attempt-a"
	view.Agents[1].Status = "unreachable"
	if got := desiredThreadStatus(view, false); got != "" {
		t.Fatalf("unreachable worker activity status=%q", got)
	}
}

type disconnectStatusStore struct {
	*swarm.Store
	barrier string
}

func (s *disconnectStatusStore) History(_ context.Context, _ string) (swarm.History, error) {
	return swarm.History{RecoveryBarrier: s.barrier}, nil
}

func (s *disconnectStatusStore) SetRecoveryBarrier(_ context.Context, _, reason string) error {
	s.barrier = reason
	return nil
}

func TestDisconnectedClearsStatusAfterBarrierAndDoesNotRefresh(t *testing.T) {
	ctx := context.Background()
	api := &threadStatusFixture{}
	f := swarm.Feature{FeatureID: swarm.NewID(), ChannelID: "C123", ThreadTS: "1.000001", OwnerAgentID: "owner"}
	store := &disconnectStatusStore{}
	h := &humanIngress{bridge: &Bridge{API: api, Features: []swarm.Feature{f}}, store: store}
	h.updateThreadStatus(ctx, f, "готовит ответ…")
	if err := h.disconnected(ctx); err != nil {
		t.Fatal(err)
	}
	if store.barrier != "slack_disconnected" || len(api.calls) != 2 || api.calls[1] != "" {
		t.Fatalf("disconnect did not clear after barrier: barrier=%q calls=%v", store.barrier, api.calls)
	}
	view := swarm.History{Feature: f, RecoveryBarrier: store.barrier, Turns: []swarm.OwnerTurn{{State: "running"}}, Agents: []swarm.AgentStatus{{AgentID: "owner", Status: "online"}}}
	h.updateThreadStatus(ctx, f, desiredThreadStatus(view, false))
	h.updateThreadStatus(ctx, f, "готовит ответ…") // stale pre-disconnect snapshot
	if len(api.calls) != 2 {
		t.Fatalf("status refreshed behind disconnect barrier: %v", api.calls)
	}
}
