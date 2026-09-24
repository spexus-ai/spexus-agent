package swarmrunner

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

func TestOwnerInputCarriesRecordedHumanBlockerContext(t *testing.T) {
	request := &swarm.HumanRequestContext{
		RequestID: "123e4567-e89b-42d3-a456-426614174000",
		Question:  "Which path?", Reason: "The release path is undecided",
		Context:        "Safe path limits the change to the test environment; fast path widens it.",
		Recommendation: "Safe path", BlockedWork: "Continue worker A",
		Options: []swarm.HumanOption{{ID: "safe", Label: "Safe path"}}, Kind: "choice",
	}
	input := swarm.InputPayload{Text: swarm.HumanDetailsControlText, ActiveHumanRequest: request}
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{Role: "owner"}}
	prompt, err := r.input(context.Background(), swarm.Delivery{Envelope: swarm.Envelope{Type: "agent.input", Payload: payload}})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Event struct {
			Payload swarm.InputPayload `json:"payload"`
		} `json:"event"`
	}
	if err := json.Unmarshal([]byte(prompt), &decoded); err != nil {
		t.Fatal(err)
	}
	got := decoded.Event.Payload.ActiveHumanRequest
	if got == nil || got.Reason != request.Reason || got.Context != request.Context || got.BlockedWork != request.BlockedWork {
		t.Fatalf("owner lost the canonical context needed to explain the question: %+v", got)
	}
}
