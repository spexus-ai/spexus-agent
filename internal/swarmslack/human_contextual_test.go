package swarmslack

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

type contextualRouteStore struct {
	*swarm.Store
	boundRequest  string
	bindError     error
	boundKind     string
	boundText     string
	boundSelector int
	terminal      string
	optionLabel   string
	answers       []swarm.HumanAnswerInput
	inputs        []swarm.InputPayload
	notices       []string
	settled       int
}

func (s *contextualRouteStore) BindContextualHumanAnswer(_ context.Context, _ swarm.SlackSource, kind string, selector int, text string) (string, error) {
	s.boundKind, s.boundSelector, s.boundText = kind, selector, text
	return s.boundRequest, s.bindError
}
func (s *contextualRouteStore) History(_ context.Context, _ string) (swarm.History, error) {
	if s.terminal != "" {
		projection := swarm.HumanProjection{RequestID: s.boundRequest, BackendState: s.terminal}
		view := swarm.History{HumanRequests: []swarm.HumanProjection{projection}}
		if s.optionLabel != "" {
			projection.View = json.RawMessage(`{"terminal":{"kind":"answer","response":{"option_id":"no"}}}`)
			view.HumanRequests[0] = projection
			view.Dependencies = []swarm.Dependency{{RequestID: s.boundRequest, Blocker: swarm.Blocker{Options: []swarm.HumanOption{{ID: "no", Label: s.optionLabel}}}}}
		}
		return view, nil
	}
	return swarm.History{}, nil
}

func TestClosedSelectorShowsCanonicalChoiceNotRequestedDeny(t *testing.T) {
	s := &contextualRouteStore{boundRequest: swarm.NewID(), terminal: "answered", optionLabel: "Нет"}
	h := &humanIngress{store: s}
	in := swarm.SlackSource{WorkspaceID: "W", ChannelID: "C", ThreadTS: "1.000001", MessageTS: "2.000001", FeatureID: swarm.NewID(), ActorID: "U", Text: "ответ 3: отказ потому что не хочу"}
	if err := h.processOne(context.Background(), in, false); err != nil {
		t.Fatal(err)
	}
	if len(s.answers) != 0 || len(s.notices) != 1 || s.settled != 1 || s.boundKind != "deny" || s.boundSelector != 3 || !strings.Contains(s.notices[0], "Вопрос #3 уже закрыт") || !strings.Contains(s.notices[0], "Выбран вариант: Нет") || strings.Contains(s.notices[0], "Отказ с причиной") {
		t.Fatalf("closed request falsely accepted natural deny: %+v", s)
	}
}
func (s *contextualRouteStore) RecordHumanAnswer(_ context.Context, a swarm.HumanAnswerInput) (string, bool, error) {
	s.answers = append(s.answers, a)
	return swarm.NewID(), false, nil
}
func (s *contextualRouteStore) Ingest(_ context.Context, _ string, in swarm.InputPayload) (swarm.Receipt, bool, error) {
	s.inputs = append(s.inputs, in)
	return swarm.Receipt{}, false, nil
}
func (s *contextualRouteStore) QueueSlackNotice(_ context.Context, _, _, text string) error {
	s.notices = append(s.notices, text)
	return nil
}
func (s *contextualRouteStore) SettleSlackSource(_ context.Context, _ swarm.SlackSource) error {
	s.settled++
	return nil
}

func TestContextualReplyRoutesOnlyExplicitPrefix(t *testing.T) {
	requestID := swarm.NewID()
	for _, tc := range []struct {
		text     string
		kind     string
		selector int
		body     string
		decision bool
	}{
		{"Ответ: объясните подробно\nс примерами", "answer", 0, "объясните подробно\nс примерами", true},
		{"Отказ: нет полномочий", "deny", 0, "нет полномочий", true},
		{"Ответ #2: решение для второго", "answer", 2, "решение для второго", true},
		{"Отказ #3: нет данных", "deny", 3, "нет данных", true},
		{"ответ 2: я тебе ответил и я не знаю что такое P3.", "answer", 2, "я тебе ответил и я не знаю что такое P3.", true},
		{"ответ 3: отказ потому что не хочу", "deny", 3, "потому что не хочу", true},
		{"ответ 3: отказ, потому что не хочу", "deny", 3, "потому что не хочу", true},
		{"ответ 3: отказ: не хочу", "deny", 3, "не хочу", true},
		{"Ответ 2: отказ от старого варианта, выбираю новый", "answer", 2, "отказ от старого варианта, выбираю новый", true},
		{"ответ #2 я тебе ответил что такое P3", "answer", 2, "я тебе ответил что такое P3", true},
		{"ОТВЕТ\u00a0#2 : ОтКаЗ  потому что не хочу", "deny", 2, "потому что не хочу", true},
		{"Рассмотрите следующий шаг", "", 0, "", false},
	} {
		s := &contextualRouteStore{boundRequest: requestID}
		h := &humanIngress{store: s}
		in := swarm.SlackSource{WorkspaceID: "W", ChannelID: "C", ThreadTS: "1.000001", MessageTS: "2.000001", FeatureID: swarm.NewID(), ActorID: "U", Text: tc.text}
		if err := h.processOne(context.Background(), in, false); err != nil {
			t.Fatal(err)
		}
		if s.settled != 1 {
			t.Fatalf("%q settled=%d", tc.text, s.settled)
		}
		if tc.decision {
			if len(s.inputs) != 0 || len(s.answers) != 1 || len(s.notices) != 0 || s.answers[0].RequestID != requestID || s.answers[0].Kind != tc.kind || s.answers[0].Text != tc.body || s.answers[0].ActorID != "U" || s.answers[0].MessageTS != in.MessageTS || s.boundKind != tc.kind || s.boundSelector != tc.selector || s.boundText != tc.body {
				t.Fatalf("%q misrouted: %+v", tc.text, s)
			}
		} else if len(s.answers) != 0 || len(s.inputs) != 1 || s.inputs[0].Text != tc.text {
			t.Fatalf("ordinary owner input intercepted: %+v", s)
		}
	}
}

func TestContextualReplayReadsCanonicalTerminalBeforeAnotherDecision(t *testing.T) {
	s := &contextualRouteStore{boundRequest: swarm.NewID(), terminal: "denied"}
	h := &humanIngress{store: s}
	in := swarm.SlackSource{WorkspaceID: "W", ChannelID: "C", ThreadTS: "1.000001", MessageTS: "2.000001", FeatureID: swarm.NewID(), ActorID: "U", Text: "Отказ #2: нет полномочий"}
	if err := h.processOne(context.Background(), in, false); err != nil {
		t.Fatal(err)
	}
	if len(s.answers) != 0 || len(s.notices) != 1 || s.settled != 1 || !strings.Contains(s.notices[0], "Вопрос #2 уже закрыт") || s.boundSelector != 2 {
		t.Fatalf("terminal replay did not stay idempotent: %+v", s)
	}
}

func TestMalformedSelectorNeverBecomesOwnerInput(t *testing.T) {
	for _, text := range []string{"Ответ #abc: да", "Отказ #0: причина", "Ответ #2 #3: текст", "Ответ без двоеточия", "Ответ #2", "Ответ 3: отказ", "Ответ 3: отказ потому что"} {
		s := &contextualRouteStore{}
		h := &humanIngress{store: s}
		in := swarm.SlackSource{WorkspaceID: "W", ChannelID: "C", ThreadTS: "1.000001", MessageTS: "2.000001", FeatureID: swarm.NewID(), ActorID: "U", Text: text}
		if err := h.processOne(context.Background(), in, false); err != nil {
			t.Fatal(err)
		}
		if len(s.inputs) != 0 || len(s.answers) != 0 || s.settled != 1 {
			t.Fatalf("malformed selector escaped decision parser: %+v", s)
		}
	}
}

func TestAmbiguousContextualReplyFailsClosedWithoutOwnerInput(t *testing.T) {
	s := &contextualRouteStore{bindError: &swarm.APIError{Status: 409, Code: "ambiguous_human_request", Message: "ambiguous_human_request"}}
	h := &humanIngress{store: s}
	in := swarm.SlackSource{WorkspaceID: "W", ChannelID: "C", ThreadTS: "1.000001", MessageTS: "2.000001", FeatureID: swarm.NewID(), ActorID: "U", Text: "Отказ: нет полномочий"}
	if err := h.processOne(context.Background(), in, false); err != nil {
		t.Fatal(err)
	}
	if len(s.inputs) != 0 || len(s.answers) != 0 || s.settled != 1 || len(s.notices) != 1 || !strings.Contains(s.notices[0], "несколько вопросов") {
		t.Fatalf("ambiguous reply did not fail closed: %+v", s)
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
	view.Turns = []swarm.OwnerTurn{{State: "running"}}
	if got := desiredThreadStatus(view, false); got != "готовит ответ…" {
		t.Fatalf("running owner status=%q", got)
	}
	if got := desiredThreadStatus(view, true); got != "проверяет ответ…" {
		t.Fatalf("decision priority status=%q", got)
	}
	view.Turns[0].State = "succeeded"
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
