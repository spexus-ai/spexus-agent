package swarm

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

// A committed task.resume may lose its HTTP receipt when the coordinator
// exits. The retained state must replay the same receipt and reject a second
// continuation, without allocating another attempt or worker delivery.
func TestHumanContinuationCommitLostReceiptAndRestart(t *testing.T) {
	f := newHumanFixture(t)
	ctx := context.Background()
	initial := f.ownerTurn()
	dispatch := f.dispatch(initial, "worker-a")
	dispatch.ProtocolVersion = 2
	f.post("orchestrator", dispatch, 201)
	f.finish(initial, "", []ActionReceipt{{MessageID: dispatch.MessageID, Status: "stored"}}, 201)
	accepted := f.event(dispatch, "task.accepted", AcceptedPayload{DispatchMessageID: dispatch.MessageID, ProfileRevision: f.profiles["worker-a"].Revision}, dispatch.MessageID)
	accepted.ProtocolVersion = 2
	f.post("worker-a", accepted, 201)
	started := f.event(dispatch, "task.started", StartedPayload{AcceptedMessageID: accepted.MessageID}, accepted.MessageID)
	started.ProtocolVersion = 2
	f.post("worker-a", started, 201)
	blocked := f.event(dispatch, "task.result", ResultPayload{Outcome: "blocked", Summary: "Needs choice", Evidence: []Evidence{}, Origin: "worker", Blocker: ptrBlocker(humanBlocker())}, started.MessageID)
	blocked.ProtocolVersion = 2
	resultReceipt := f.post("worker-a", blocked, 201)
	f.call("orchestrator", "POST", "/acks", AckRequest{MailboxSeqs: []int64{resultReceipt.MailboxSeq}}, 200)
	requestTurn := NewID()
	f.call("orchestrator", "POST", "/owner-turns/start", OwnerStartRequest{TurnID: requestTurn, FeatureID: f.feature.FeatureID, InputMailboxSeq: resultReceipt.MailboxSeq}, 201)
	d := f.history().Dependencies[0]
	request := Envelope{ProtocolVersion: 2, MessageID: NewID(), Type: "human.request", TenantID: f.cfg.TenantID, ProjectID: f.cfg.ProjectID, FeatureID: f.feature.FeatureID, FromAgentID: "orchestrator", ToAgentID: "coordinator", OwnerTurnID: requestTurn, SentAt: f.s.stamp(), Payload: mustJSON(HumanRequestPayload{DependencyID: d.ID, Blocker: humanBlocker()})}
	f.post("orchestrator", request, 201)
	f.finish(requestTurn, "", []ActionReceipt{{MessageID: request.MessageID, Status: "stored"}}, 201)
	d = f.history().Dependencies[0]
	view := humanBackendView{ID: d.RequestID, TenantID: f.cfg.TenantID, ProjectID: f.cfg.ProjectID, FeatureID: f.feature.FeatureID, AllowedResponders: []string{"human"}, Options: humanBlocker().Options, State: "open", Revision: 1}
	view.Dependency.ID = d.ID
	view.Slack.WorkspaceID, view.Slack.ChannelID, view.Slack.ThreadTS = "workspace", f.feature.ChannelID, f.feature.ThreadTS
	if err := f.s.acceptHumanEnvelope(ctx, d.RequestID, mustJSON(humanEnvelope{SchemaVersion: 1, Data: mustJSON(view)})); err != nil {
		t.Fatal(err)
	}
	decisionID := NewID()
	view.State, view.Revision = "answered", 2
	view.Terminal = &struct {
		ID       string          `json:"id"`
		Kind     string          `json:"kind"`
		Response json.RawMessage `json:"response"`
		Source   json.RawMessage `json:"source"`
	}{ID: decisionID, Kind: "answer", Response: mustJSON(map[string]any{"kind": "answer", "option_id": "a", "text": "Use A"}), Source: mustJSON(map[string]any{"actor_id": "human", "workspace_id": "workspace", "channel_id": f.feature.ChannelID, "thread_ts": f.feature.ThreadTS, "message_ts": "124.1"})}
	if err := f.s.acceptHumanEnvelope(ctx, d.RequestID, mustJSON(humanEnvelope{SchemaVersion: 1, Data: mustJSON(view)})); err != nil {
		t.Fatal(err)
	}
	d = f.history().Dependencies[0]
	if d.State != "resolved" || d.DecisionID != decisionID {
		t.Fatalf("decision not applied: %+v", d)
	}
	var decision Delivery
	for _, m := range f.history().Messages {
		if m.Type == "human.decision" {
			decision = m
		}
	}
	if decision.MailboxSeq == 0 {
		t.Fatal("decision input missing")
	}
	f.call("orchestrator", "POST", "/acks", AckRequest{MailboxSeqs: []int64{decision.MailboxSeq}}, 200)
	turn := NewID()
	f.call("orchestrator", "POST", "/owner-turns/start", OwnerStartRequest{TurnID: turn, FeatureID: f.feature.FeatureID, InputMailboxSeq: decision.MailboxSeq}, 201)
	var original DispatchPayload
	if err := json.Unmarshal(dispatch.Payload, &original); err != nil {
		t.Fatal(err)
	}
	original.AcceptBy = time.Now().UTC().Add(60 * time.Second).Format(time.RFC3339Nano)
	resume := Envelope{ProtocolVersion: 2, MessageID: NewID(), Type: "task.resume", TenantID: dispatch.TenantID, ProjectID: dispatch.ProjectID, FeatureID: dispatch.FeatureID, FromAgentID: "orchestrator", ToAgentID: "worker-a", OwnerTurnID: turn, JobID: dispatch.JobID, AttemptID: NewID(), SentAt: f.s.stamp(), Payload: mustJSON(ResumeTaskPayload{DependencyID: d.ID, DecisionID: decisionID, WorkerAgentID: "worker-a", Dispatch: original})}
	first := f.post("orchestrator", resume, 201) // Commit, then discard receipt.
	before := f.history()
	statePath := f.s.dbPath()
	f.server.Close()
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.s, err = Open(ctx, statePath, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.server = httptest.NewTLSServer(f.s.Handler())
	if err = f.s.SetRecoveryBarrier(ctx, f.feature.FeatureID, ""); err != nil {
		t.Fatal(err)
	}
	if got := f.post("orchestrator", resume, 200); got != first {
		t.Fatalf("committed continuation receipt changed: before=%+v after=%+v", first, got)
	}
	conflict := resume
	conflict.MessageID, conflict.AttemptID = NewID(), NewID()
	f.post("orchestrator", conflict, 409)
	after := f.history()
	if len(after.Jobs) != len(before.Jobs) || len(after.Jobs[0].Attempts) != 2 || after.Dependencies[0].ContinuationAttemptID != resume.AttemptID {
		t.Fatal("replay allocated another attempt or changed dependency")
	}
	var resumes, deliveries int
	if err = f.s.db.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE kind='task.resume' AND attempt_id=?", resume.AttemptID).Scan(&resumes); err != nil || resumes != 1 {
		t.Fatalf("resume message count=%d err=%v", resumes, err)
	}
	if err = f.s.db.QueryRowContext(ctx, "SELECT count(*) FROM mailbox_delivery d JOIN messages m ON m.id=d.message_row WHERE m.kind='task.resume' AND m.attempt_id=?", resume.AttemptID).Scan(&deliveries); err != nil || deliveries != 1 {
		t.Fatalf("worker delivery count=%d err=%v", deliveries, err)
	}
}
