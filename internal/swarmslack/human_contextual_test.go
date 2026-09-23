package swarmslack

import (
	"context"
	"strings"
	"testing"

	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

type contextualRouteStore struct {
	*swarm.Store
	boundRequest string
	bindError    error
	boundKind    string
	boundText    string
	answers      []swarm.HumanAnswerInput
	inputs       []swarm.InputPayload
	notices      []string
	settled      int
}

func (s *contextualRouteStore) BindContextualHumanAnswer(_ context.Context, _ swarm.SlackSource, kind, text string) (string, error) {
	s.boundKind, s.boundText = kind, text
	return s.boundRequest, s.bindError
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
		body     string
		decision bool
	}{
		{"Ответ: объясните подробно\nс примерами", "answer", "объясните подробно\nс примерами", true},
		{"Отказ: нет полномочий", "deny", "нет полномочий", true},
		{"Рассмотрите следующий шаг", "", "", false},
		{"Ответ без двоеточия", "", "", false},
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
			if len(s.inputs) != 0 || len(s.answers) != 1 || s.answers[0].RequestID != requestID || s.answers[0].Kind != tc.kind || s.answers[0].Text != tc.body || s.answers[0].ActorID != "U" || s.answers[0].MessageTS != in.MessageTS || s.boundKind != tc.kind || s.boundText != tc.body {
				t.Fatalf("%q misrouted: %+v", tc.text, s)
			}
		} else if len(s.answers) != 0 || len(s.inputs) != 1 || s.inputs[0].Text != tc.text {
			t.Fatalf("ordinary owner input intercepted: %+v", s)
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
