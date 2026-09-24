package swarm

import (
	"context"
	"encoding/json"
	"testing"
)

func TestUrgentOwnerInterruptLeavesFeatureAndWorkersRunning(t *testing.T) {
	f := newHumanFixture(t)
	ctx := context.Background()
	turnID := f.ownerTurn()
	dispatch := f.dispatch(turnID, "worker-a")
	dispatch.ProtocolVersion = 2
	f.post("orchestrator", dispatch, 201)
	before := f.history()
	if err := f.s.InterruptOwnerTurn(ctx, f.feature.FeatureID, "stranger", "urgent message"); err == nil {
		t.Fatal("unauthorized actor interrupted owner")
	}
	if err := f.s.InterruptOwnerTurn(ctx, f.feature.FeatureID, "human", "urgent message"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.InterruptOwnerTurn(ctx, f.feature.FeatureID, "human", "urgent message"); err != nil {
		t.Fatal(err)
	}
	after := f.history()
	if after.Feature.Stopped || len(after.Turns) != 1 || !after.Turns[0].CancelRequested {
		t.Fatalf("owner interruption changed feature state: %+v", after.Turns)
	}
	if len(after.Jobs) != len(before.Jobs) || after.Jobs[0].Attempts[0].CancelRequested {
		t.Fatal("urgent owner interruption cancelled independent worker")
	}
	var controls MailboxResponse
	if err := json.Unmarshal(f.call("orchestrator", "GET", "/mailbox?lane=control&wait_seconds=0", nil, 200), &controls); err != nil {
		t.Fatal(err)
	}
	if len(controls.Messages) != 1 || controls.Messages[0].Type != "turn.cancel" || controls.Messages[0].OwnerTurnID != turnID {
		t.Fatalf("expected one durable owner cancellation: %+v", controls.Messages)
	}
	var cancel CancelPayload
	if err := json.Unmarshal(controls.Messages[0].Payload, &cancel); err != nil || cancel.RequestedBy != "human" {
		t.Fatalf("wrong cancellation source: %+v, %v", cancel, err)
	}
	input, duplicate, err := f.s.Ingest(ctx, f.feature.FeatureID, InputPayload{Text: "!Please change the approach", Source: Source{Kind: "slack", EventID: "urgent-source", ChannelID: f.feature.ChannelID, ThreadTS: f.feature.ThreadTS, ActorID: "human"}})
	if err != nil || duplicate || input.MailboxSeq == 0 {
		t.Fatalf("urgent input was not retained: %+v, %t, %v", input, duplicate, err)
	}
}
