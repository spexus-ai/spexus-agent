package swarm

import (
	"encoding/json"
	"testing"
)

func TestFinalReviewWithoutReplyQueuesOneSummaryOnlyTurn(t *testing.T) {
	f := newHumanFixture(t)
	initial := f.ownerTurn()
	a, b := f.dispatch(initial, "worker-a"), f.dispatch(initial, "worker-b")
	a.ProtocolVersion, b.ProtocolVersion = 2, 2
	f.post("orchestrator", a, 201)
	f.post("orchestrator", b, 201)
	f.finish(initial, "", []ActionReceipt{{MessageID: a.MessageID, Status: "stored"}, {MessageID: b.MessageID, Status: "stored"}}, 201)

	reviewResult := func(dispatch Envelope) (int64, string, string) {
		accepted := f.event(dispatch, "task.accepted", AcceptedPayload{DispatchMessageID: dispatch.MessageID, ProfileRevision: f.profiles[dispatch.ToAgentID].Revision}, dispatch.MessageID)
		accepted.ProtocolVersion = 2
		f.post(dispatch.ToAgentID, accepted, 201)
		started := f.event(dispatch, "task.started", StartedPayload{AcceptedMessageID: accepted.MessageID}, accepted.MessageID)
		started.ProtocolVersion = 2
		f.post(dispatch.ToAgentID, started, 201)
		result := f.event(dispatch, "task.result", resultPayload(), started.MessageID)
		result.ProtocolVersion = 2
		receipt := f.post(dispatch.ToAgentID, result, 201)
		turnID := NewID()
		f.call("orchestrator", "POST", "/owner-turns/start", OwnerStartRequest{TurnID: turnID, FeatureID: f.feature.FeatureID, InputMailboxSeq: receipt.MailboxSeq}, 201)
		review := Envelope{ProtocolVersion: 2, MessageID: NewID(), Type: "task.review", TenantID: f.cfg.TenantID, ProjectID: f.cfg.ProjectID, FeatureID: f.feature.FeatureID, FromAgentID: "orchestrator", ToAgentID: dispatch.ToAgentID, OwnerTurnID: turnID, JobID: dispatch.JobID, AttemptID: dispatch.AttemptID, CausationID: cause(result.MessageID), SentAt: f.s.stamp(), Payload: mustJSON(ReviewPayload{ResultMessageID: result.MessageID, Verdict: "accepted", Reason: "Checked", Evidence: []Evidence{}})}
		f.post("orchestrator", review, 201)
		f.finish(turnID, "", []ActionReceipt{{MessageID: review.MessageID, Status: "stored"}}, 201)
		return receipt.MailboxSeq, turnID, review.MessageID
	}
	_, _, _ = reviewResult(a)
	var queued int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM audit WHERE event='owner_summary_queued'`).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("summary before both results: count=%d err=%v", queued, err)
	}
	lastSeq, lastTurn, lastReview := reviewResult(b)
	f.call("orchestrator", "POST", "/owner-turns/"+lastTurn+"/finish", OwnerFinishRequest{Outcome: "succeeded", Reply: "", Actions: []ActionReceipt{{MessageID: lastReview, Status: "stored"}}, Observation: json.RawMessage("null")}, 200)
	if err := f.s.db.QueryRow(`SELECT count(*) FROM audit WHERE event='owner_summary_queued'`).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("summary after final review: count=%d err=%v", queued, err)
	}
	var summarySeq int64
	var kind string
	if err := f.s.db.QueryRow(`SELECT d.seq,m.kind FROM mailbox_delivery d JOIN messages m ON m.id=d.message_row WHERE d.agent_id='orchestrator' AND d.seq>? ORDER BY d.seq LIMIT 1`, lastSeq).Scan(&summarySeq, &kind); err != nil || kind != "task.result" {
		t.Fatalf("summary trigger: seq=%d kind=%q err=%v", summarySeq, kind, err)
	}
	if err := f.s.db.QueryRow(`SELECT count(*) FROM messages WHERE kind='task.result' AND job_id=?`, b.JobID).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("duplicate result: count=%d err=%v", queued, err)
	}
	turnID := NewID()
	f.call("orchestrator", "POST", "/owner-turns/start", OwnerStartRequest{TurnID: turnID, FeatureID: f.feature.FeatureID, InputMailboxSeq: summarySeq}, 201)
	var summaryFinish OwnerFinishReceipt
	if err := json.Unmarshal(f.call("orchestrator", "POST", "/owner-turns/"+turnID+"/finish", OwnerFinishRequest{Outcome: "succeeded", Reply: "Оба результата проверены.", Actions: []ActionReceipt{}, Observation: json.RawMessage("null")}, 201), &summaryFinish); err != nil {
		t.Fatal(err)
	}
	if summaryFinish.ReplyStatus != "queued" || len(f.history().SlackOutbox) != 1 || f.history().SlackOutbox[0].Text != "Оба результата проверены." {
		t.Fatalf("reviewed summary reply was suppressed: %+v", summaryFinish)
	}
	if err := f.s.db.QueryRow(`SELECT count(*) FROM audit WHERE event='owner_summary_queued'`).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("summary loop: count=%d err=%v", queued, err)
	}
}
