package swarm

import (
	"encoding/json"
	"testing"
)

func TestWebProfileRejectsNoncanonicalAndUnsafeSnapshots(t *testing.T) {
	p := TextProfile{ID: "worker-a", Model: "openai-codex/gpt-6-luna", Reasoning: "minimal", Prompt: "Return JSON", Tools: []string{}, Extensions: []string{}}
	canonical, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateWebTextProfile(canonical); err != nil {
		t.Fatalf("canonical profile rejected: %v", err)
	}
	cases := map[string][]byte{
		"noncanonical bytes": append(append([]byte{}, canonical...), '\n'),
		"tool grant":         mustJSON(TextProfile{ID: p.ID, Model: p.Model, Reasoning: p.Reasoning, Prompt: p.Prompt, Tools: []string{"bash"}, Extensions: []string{}}),
		"invalid reasoning":  mustJSON(TextProfile{ID: p.ID, Model: p.Model, Reasoning: "fastest", Prompt: p.Prompt, Tools: []string{}, Extensions: []string{}}),
		"empty prompt":       mustJSON(TextProfile{ID: p.ID, Model: p.Model, Reasoning: p.Reasoning, Prompt: "  ", Tools: []string{}, Extensions: []string{}}),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateWebTextProfile(raw); err == nil {
				t.Fatal("unsafe snapshot accepted")
			}
		})
	}
}

func TestWebCoordinatorClaimsRequireLocalTypedExecution(t *testing.T) {
	f := newFixture(t)
	turnID := f.ownerTurn()
	worker := f.dispatch(turnID, "worker-a")
	f.post("orchestrator", worker, 201)
	base := LaunchClaimRequest{SchemaVersion: 1, ExecutionRef: ExecutionRef{WorkerAttemptID: worker.AttemptID}, ExpectedGeneration: 1, ExpectedRevision: f.profiles["worker-a"].Revision}
	f.call("worker-a", "POST", "/agent-profiles/worker-a/launch-claims", base, 409)
	accepted := f.event(worker, "task.accepted", AcceptedPayload{DispatchMessageID: worker.MessageID, ProfileRevision: base.ExpectedRevision}, worker.MessageID)
	f.post("worker-a", accepted, 201)
	var claim LaunchClaim
	if err := json.Unmarshal(f.call("worker-a", "POST", "/agent-profiles/worker-a/launch-claims", base, 201), &claim); err != nil {
		t.Fatal(err)
	}
	if claim.ExecutionRef != base.ExecutionRef || claim.Revision != base.ExpectedRevision || claim.Generation != 1 {
		t.Fatalf("claim changed execution identity: %+v", claim)
	}
	ownerRequest := LaunchClaimRequest{SchemaVersion: 1, ExecutionRef: ExecutionRef{OwnerTurnID: turnID}, ExpectedGeneration: 1, ExpectedRevision: f.profiles["orchestrator"].Revision}
	f.call("worker-a", "POST", "/agent-profiles/worker-a/launch-claims", ownerRequest, 409)
	f.call("orchestrator", "POST", "/agent-profiles/orchestrator/launch-claims", ownerRequest, 201)
}
