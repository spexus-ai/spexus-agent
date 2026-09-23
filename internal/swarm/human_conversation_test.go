package swarm

import (
	"context"
	"encoding/json"
	"testing"
)

func TestHumanQuestionsPublishOneAtATime(t *testing.T) {
	f := newHumanFixture(t)
	ctx := context.Background()
	first := publishedHumanRequest(t, f, nil, "queued")
	second := publishedHumanRequest(t, f, nil, "queued")
	claim, err := f.s.ClaimSlack(ctx)
	if err != nil || claim == nil || claim.ID != first {
		t.Fatalf("first question claim=%+v err=%v", claim, err)
	}
	if err := f.s.SettleSlack(ctx, first, "sent", "123.000005"); err != nil {
		t.Fatal(err)
	}
	if claim, err := f.s.ClaimSlack(ctx); err != nil || claim != nil {
		t.Fatalf("second question visible before first resolution: %+v, %v", claim, err)
	}
	if _, err := f.s.db.ExecContext(ctx, "UPDATE human_projections SET state='answered' WHERE request_id=?", first); err != nil {
		t.Fatal(err)
	}
	claim, err = f.s.ClaimSlack(ctx)
	if err != nil || claim == nil || claim.ID != second {
		t.Fatalf("second question not released after canonical terminal: %+v, %v", claim, err)
	}
}

func TestSlackQuestionSnapshotIsImmutableAcrossDuplicateAndQueueAdvance(t *testing.T) {
	f := newHumanFixture(t)
	ctx := context.Background()
	first := publishedHumanRequest(t, f, nil, "sent")
	source := contextualSlackSource(f, "124.000001", "I need an explanation")
	if duplicate, err := f.s.CommitSlackSource(ctx, source); err != nil || duplicate {
		t.Fatalf("commit: duplicate=%t err=%v", duplicate, err)
	}
	committed, err := f.s.CommittedSlackSource(ctx, source.WorkspaceID, source.ChannelID, source.MessageTS)
	if err != nil || committed.ActiveHumanRequest == nil || committed.ActiveHumanRequest.RequestID != first {
		t.Fatalf("missing immutable first-question context: %+v, %v", committed, err)
	}
	if _, err := f.s.db.ExecContext(ctx, "UPDATE human_projections SET state='answered' WHERE request_id=?", first); err != nil {
		t.Fatal(err)
	}
	_ = publishedHumanRequest(t, f, nil, "sent")
	if duplicate, err := f.s.CommitSlackSource(ctx, source); err != nil || !duplicate {
		t.Fatalf("redelivery changed source: duplicate=%t err=%v", duplicate, err)
	}
	replayed, err := f.s.CommittedSlackSource(ctx, source.WorkspaceID, source.ChannelID, source.MessageTS)
	if err != nil || replayed.ActiveHumanRequest == nil || replayed.ActiveHumanRequest.RequestID != first {
		t.Fatalf("replay rebound to a later question: %+v, %v", replayed, err)
	}
}

func TestOwnerHumanRespondRequiresDeliveredTrustedSlackSource(t *testing.T) {
	f := newHumanFixture(t)
	ctx := context.Background()
	requestID := publishedHumanRequest(t, f, nil, "sent")
	source := contextualSlackSource(f, "124.000001", "I do not know what P3 means")
	if _, err := f.s.CommitSlackSource(ctx, source); err != nil {
		t.Fatal(err)
	}
	makeResponse := func(ts string) Envelope {
		return Envelope{ProtocolVersion: 2, MessageID: NewID(), Type: "human.respond", TenantID: f.cfg.TenantID, ProjectID: f.cfg.ProjectID, FeatureID: f.feature.FeatureID, FromAgentID: "orchestrator", ToAgentID: "coordinator", OwnerTurnID: NewID(), CausationID: cause(NewID()), SentAt: f.s.stamp(), Payload: mustJSON(HumanRespondPayload{RequestID: requestID, SourceMessageTS: ts, Kind: "answer", Text: source.Text})}
	}
	// A committed socket source alone is insufficient: the owner has not seen it.
	undelivered := makeResponse(source.MessageTS)
	undelivered.OwnerTurnID = f.ownerTurn()
	f.post("orchestrator", undelivered, 403)
	f.finish(undelivered.OwnerTurnID, "", []ActionReceipt{}, 201)
	committed, err := f.s.CommittedSlackSource(ctx, source.WorkspaceID, source.ChannelID, source.MessageTS)
	if err != nil {
		t.Fatal(err)
	}
	input := InputPayload{Text: committed.Text, Source: Source{Kind: "slack", EventID: "slack:" + committed.ChannelID + ":" + committed.MessageTS, ChannelID: committed.ChannelID, ThreadTS: committed.ThreadTS, ActorID: committed.ActorID, MessageTS: committed.MessageTS}, ActiveHumanRequest: committed.ActiveHumanRequest}
	receipt, _, err := f.s.Ingest(ctx, f.feature.FeatureID, input)
	if err != nil {
		t.Fatal(err)
	}
	f.call("orchestrator", "POST", "/acks", AckRequest{[]int64{receipt.MailboxSeq}}, 200)
	turnID := NewID()
	f.call("orchestrator", "POST", "/owner-turns/start", OwnerStartRequest{TurnID: turnID, FeatureID: f.feature.FeatureID, InputMailboxSeq: receipt.MailboxSeq}, 201)
	respond := makeResponse(source.MessageTS)
	respond.OwnerTurnID, respond.CausationID = turnID, cause(receipt.MessageID)
	first := f.post("orchestrator", respond, 201)
	if again := f.post("orchestrator", respond, 200); again != first {
		t.Fatalf("redelivery changed receipt: %+v %+v", first, again)
	}
	var operations int
	if err := f.s.db.QueryRowContext(ctx, `SELECT count(*) FROM backend_sync_operations WHERE request_id=? AND kind='decision'`, requestID).Scan(&operations); err != nil || operations != 1 {
		t.Fatalf("canonical operation count=%d err=%v", operations, err)
	}
	var raw []byte
	if err := f.s.db.QueryRowContext(ctx, `SELECT payload FROM backend_sync_operations WHERE request_id=? AND kind='decision'`, requestID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var operation struct {
		Source struct {
			ActorID   string `json:"actor_id"`
			MessageTS string `json:"message_ts"`
		} `json:"source"`
	}
	if err := json.Unmarshal(raw, &operation); err != nil || operation.Source.ActorID != source.ActorID || operation.Source.MessageTS != source.MessageTS {
		t.Fatalf("source provenance was model-forged: %s %v", raw, err)
	}
	other := makeResponse("124.000002")
	other.OwnerTurnID, other.CausationID = turnID, cause(receipt.MessageID)
	f.post("orchestrator", other, 403)
	// The matching source may still be pending settlement when a fast owner acts.
	var status string
	if err := f.s.db.QueryRowContext(ctx, `SELECT status FROM slack_sources WHERE workspace_id=? AND channel_id=? AND message_ts=?`, source.WorkspaceID, source.ChannelID, source.MessageTS).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("test did not exercise pending-source race: %q %v", status, err)
	}
}

func TestSourceBeforePublishedQuestionHasNoActiveSnapshot(t *testing.T) {
	f := newHumanFixture(t)
	ctx := context.Background()
	_ = publishedHumanRequest(t, f, nil, "sent")
	source := contextualSlackSource(f, "122.000001", "older message")
	if _, err := f.s.CommitSlackSource(ctx, source); err != nil {
		t.Fatal(err)
	}
	committed, err := f.s.CommittedSlackSource(ctx, source.WorkspaceID, source.ChannelID, source.MessageTS)
	if err != nil || committed.ActiveHumanRequest != nil {
		t.Fatalf("pre-question message borrowed new question: %+v, %v", committed, err)
	}
}

func TestDeferredStopSourceAppearsOnlyAfterContinue(t *testing.T) {
	f := newHumanFixture(t)
	ctx := context.Background()
	source := contextualSlackSource(f, "124.000001", "!stop")
	if _, err := f.s.CommitSlackSource(ctx, source); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SettleSlackSource(ctx, source); err != nil {
		t.Fatal(err)
	}
	stops, err := f.s.DeferredStopSources(ctx, f.feature.FeatureID)
	if err != nil || len(stops) != 1 || stops[0].MessageTS != source.MessageTS {
		t.Fatalf("deferred stop source=%+v err=%v", stops, err)
	}
	input := InputPayload{Text: source.Text, Source: Source{Kind: "slack", EventID: "slack:" + source.ChannelID + ":" + source.MessageTS, ChannelID: source.ChannelID, ThreadTS: source.ThreadTS, ActorID: source.ActorID, MessageTS: source.MessageTS}}
	if _, _, err := f.s.IngestUrgent(ctx, source.FeatureID, input); err != nil {
		t.Fatal(err)
	}
	stops, err = f.s.DeferredStopSources(ctx, f.feature.FeatureID)
	if err != nil || len(stops) != 0 {
		t.Fatalf("deferred stop replayed twice: %+v, %v", stops, err)
	}
}
