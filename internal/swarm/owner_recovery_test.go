package swarm

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

// A failed owner turn may have committed its current review before a later
// duplicate action was rejected. Recovery must redeliver only the existing
// result and must not create another result, review, or worker attempt.
func TestOfflineOwnerResultRedeliveryIsScopedAndIdempotent(t *testing.T) {
	f := newHumanFixture(t)
	ctx := context.Background()
	initial := f.ownerTurn()
	a, b := f.dispatch(initial, "worker-a"), f.dispatch(initial, "worker-b")
	a.ProtocolVersion, b.ProtocolVersion = 2, 2
	f.post("orchestrator", a, 201)
	f.post("orchestrator", b, 201)
	f.finish(initial, "", []ActionReceipt{{MessageID: a.MessageID, Status: "stored"}, {MessageID: b.MessageID, Status: "stored"}}, 201)
	var ownerAcks []int64
	startWorker := func(d Envelope) Envelope {
		accepted := f.event(d, "task.accepted", AcceptedPayload{DispatchMessageID: d.MessageID, ProfileRevision: f.s.profiles[d.ToAgentID].Revision}, d.MessageID)
		accepted.ProtocolVersion = 2
		ownerAcks = append(ownerAcks, f.post(d.ToAgentID, accepted, 201).MailboxSeq)
		started := f.event(d, "task.started", StartedPayload{AcceptedMessageID: accepted.MessageID}, accepted.MessageID)
		started.ProtocolVersion = 2
		ownerAcks = append(ownerAcks, f.post(d.ToAgentID, started, 201).MailboxSeq)
		return started
	}
	as, bs := startWorker(a), startWorker(b)
	f.call("orchestrator", "POST", "/acks", AckRequest{MailboxSeqs: ownerAcks}, 200)
	ar := f.event(a, "task.result", resultPayload(), as.MessageID)
	br := f.event(b, "task.result", resultPayload(), bs.MessageID)
	ar.ProtocolVersion, br.ProtocolVersion = 2, 2
	arr := f.post("worker-a", ar, 201)
	brr := f.post("worker-b", br, 201)
	makeReview := func(d, result Envelope, owner string, reason string) Envelope {
		return Envelope{ProtocolVersion: 2, MessageID: NewID(), Type: "task.review", TenantID: f.cfg.TenantID, ProjectID: f.cfg.ProjectID, FeatureID: f.feature.FeatureID, FromAgentID: "orchestrator", ToAgentID: d.ToAgentID, OwnerTurnID: owner, JobID: d.JobID, AttemptID: d.AttemptID, CausationID: cause(result.MessageID), SentAt: f.s.stamp(), Payload: mustJSON(ReviewPayload{ResultMessageID: result.MessageID, Verdict: "accepted", Reason: reason, Evidence: []Evidence{}})}
	}
	f.call("orchestrator", "POST", "/acks", AckRequest{MailboxSeqs: []int64{brr.MailboxSeq}}, 200)
	bTurn := NewID()
	f.call("orchestrator", "POST", "/owner-turns/start", OwnerStartRequest{TurnID: bTurn, FeatureID: f.feature.FeatureID, InputMailboxSeq: brr.MailboxSeq}, 201)
	bReview := makeReview(b, br, bTurn, "B checked")
	f.post("orchestrator", bReview, 201)
	f.finish(bTurn, "", []ActionReceipt{{MessageID: bReview.MessageID, Status: "stored"}}, 201)
	f.call("orchestrator", "POST", "/acks", AckRequest{MailboxSeqs: []int64{arr.MailboxSeq}}, 200)
	failedTurn := NewID()
	f.call("orchestrator", "POST", "/owner-turns/start", OwnerStartRequest{TurnID: failedTurn, FeatureID: f.feature.FeatureID, InputMailboxSeq: arr.MailboxSeq}, 201)
	aReview := makeReview(a, ar, failedTurn, "A checked")
	f.post("orchestrator", aReview, 201)
	duplicateB := makeReview(b, br, failedTurn, "B checked again with changed evidence")
	f.post("orchestrator", duplicateB, 409)
	code := "transition_conflict"
	f.call("orchestrator", "POST", "/owner-turns/"+failedTurn+"/finish", OwnerFinishRequest{Outcome: "failed", Reply: "", Actions: []ActionReceipt{{MessageID: aReview.MessageID, Status: "stored"}, {MessageID: duplicateB.MessageID, Status: "rejected", ErrorCode: &code}}, Error: &TaskError{Code: "action_rejected", Message: "action_rejected", Retryable: false}, Observation: json.RawMessage("null")}, 201)
	r := OwnerRedeliveryRequest{FeatureID: f.feature.FeatureID, FailedTurnID: failedTurn, OriginalSeq: arr.MailboxSeq, ResultMessageID: ar.MessageID, ReviewMessageID: aReview.MessageID, Actor: "operator-test", Reason: "Recover final summary after duplicate B review"}
	bad := r
	bad.ReviewMessageID = bReview.MessageID
	if _, err := f.s.redeliverOwnerResult(ctx, bad); err == nil {
		t.Fatal("review from another turn was accepted")
	}
	bad = r
	bad.ResultMessageID = br.MessageID
	if _, err := f.s.redeliverOwnerResult(ctx, bad); err == nil {
		t.Fatal("wrong result was accepted")
	}
	if err := f.s.SetRecoveryBarrier(ctx, f.feature.FeatureID, "source_catchup_required"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.redeliverOwnerResult(ctx, r); err == nil {
		t.Fatal("barrier was bypassed")
	}
	if err := f.s.SetRecoveryBarrier(ctx, f.feature.FeatureID, ""); err != nil {
		t.Fatal(err)
	}
	activeInput, _, err := f.s.Ingest(ctx, f.feature.FeatureID, InputPayload{Text: "Independent follow-up", Source: Source{Kind: "slack", EventID: NewID(), ChannelID: f.feature.ChannelID, ThreadTS: f.feature.ThreadTS, ActorID: "human"}})
	if err != nil {
		t.Fatal(err)
	}
	f.call("orchestrator", "POST", "/acks", AckRequest{MailboxSeqs: []int64{activeInput.MailboxSeq}}, 200)
	activeTurn := NewID()
	f.call("orchestrator", "POST", "/owner-turns/start", OwnerStartRequest{TurnID: activeTurn, FeatureID: f.feature.FeatureID, InputMailboxSeq: activeInput.MailboxSeq}, 201)
	if _, err := f.s.redeliverOwnerResult(ctx, r); err == nil {
		t.Fatal("active owner was bypassed")
	}
	f.finish(activeTurn, "", []ActionReceipt{}, 201)
	if err := f.s.StopFeature(ctx, f.feature.FeatureID, "human", "Operator stop before recovery"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.redeliverOwnerResult(ctx, r); err == nil {
		t.Fatal("stopped feature was bypassed")
	}
	if err := f.s.ContinueFeature(ctx, f.feature.FeatureID, "human"); err != nil {
		t.Fatal(err)
	}
	before := f.history()
	path := f.s.dbPath()
	f.server.Close()
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	newOwnerInstance := NewID()
	if err := ReconcileOffline(ctx, path, f.cfg, ReconcileRequest{AgentID: "orchestrator", OldInstanceID: f.instances["orchestrator"], NewInstanceID: newOwnerInstance, Actor: "operator-test", Reason: "Replace stopped owner runner", ContainerID: "stopped-owner-test", ContainerStopped: true, CheckedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	receipt, err := RedeliverOwnerResultOffline(ctx, path, f.cfg, r)
	if err != nil {
		t.Fatal(err)
	}
	f.s, err = Open(ctx, path, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.server = httptest.NewTLSServer(f.s.Handler())
	f.instances["orchestrator"] = newOwnerInstance
	if err = f.s.SetRecoveryBarrier(ctx, f.feature.FeatureID, ""); err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "queued" || receipt.NewSeq <= arr.MailboxSeq {
		t.Fatalf("bad recovery receipt: %+v", receipt)
	}
	again, err := f.s.redeliverOwnerResult(ctx, r)
	if err != nil || again != receipt {
		t.Fatalf("replay changed recovery: %+v, %v", again, err)
	}
	bad = r
	bad.Reason = "different operator intent"
	if _, err := f.s.redeliverOwnerResult(ctx, bad); err == nil {
		t.Fatal("different replay was accepted")
	}
	var mailbox MailboxResponse
	if err := json.Unmarshal(f.call("orchestrator", "GET", "/mailbox?wait_seconds=0", nil, 200), &mailbox); err != nil {
		t.Fatal(err)
	}
	if len(mailbox.Messages) != 1 || mailbox.Messages[0].MailboxSeq != receipt.NewSeq || mailbox.Messages[0].MessageID != ar.MessageID || mailbox.Messages[0].Type != "task.result" {
		t.Fatalf("recovery changed task result or delivery identity: %+v", mailbox.Messages)
	}
	newTurn := NewID()
	f.call("orchestrator", "POST", "/owner-turns/start", OwnerStartRequest{TurnID: newTurn, FeatureID: f.feature.FeatureID, InputMailboxSeq: receipt.NewSeq}, 201)
	f.finish(newTurn, "A and B verified", []ActionReceipt{}, 201)
	after := f.history()
	if len(after.Jobs) != len(before.Jobs) || len(after.Messages) != len(before.Messages) || len(after.Turns) != len(before.Turns)+1 {
		t.Fatalf("recovery replayed business action: before=%d/%d/%d after=%d/%d/%d", len(before.Jobs), len(before.Messages), len(before.Turns), len(after.Jobs), len(after.Messages), len(after.Turns))
	}
	if after.Jobs[0].Attempts[0].Review != "accepted" || after.Jobs[1].Attempts[0].Review != "accepted" {
		t.Fatal("review state changed")
	}
}

func TestOfflineOpenPreservesRecoveryBarrierAndExclusiveLock(t *testing.T) {
	f := newHumanFixture(t)
	ctx := context.Background()
	path := f.s.dbPath()
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	offline, err := openStore(ctx, path, f.cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	defer offline.Close()
	if err := offline.transaction(ctx, func(tx *sql.Tx) error { return barrier(ctx, tx, f.feature.FeatureID) }); err != nil {
		t.Fatalf("offline open inserted a recovery barrier: %v", err)
	}
	if _, err := openStore(ctx, path, f.cfg, false); err == nil {
		t.Fatal("concurrent operator acquired exclusive coordinator lock")
	}
}
