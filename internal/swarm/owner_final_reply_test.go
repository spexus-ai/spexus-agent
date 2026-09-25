package swarm

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
)

func finishReviewedResult(t *testing.T, f *fixture, dispatch Envelope, verdict, reply string, beforeFinish ...func()) (string, OwnerFinishRequest, OwnerFinishReceipt) {
	t.Helper()
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
	review := Envelope{ProtocolVersion: 2, MessageID: NewID(), Type: "task.review", TenantID: f.cfg.TenantID, ProjectID: f.cfg.ProjectID, FeatureID: f.feature.FeatureID, FromAgentID: "orchestrator", ToAgentID: dispatch.ToAgentID, OwnerTurnID: turnID, JobID: dispatch.JobID, AttemptID: dispatch.AttemptID, CausationID: cause(result.MessageID), SentAt: f.s.stamp(), Payload: mustJSON(ReviewPayload{ResultMessageID: result.MessageID, Verdict: verdict, Reason: "Checked", Evidence: []Evidence{}})}
	f.post("orchestrator", review, 201)
	for _, hook := range beforeFinish {
		hook()
	}
	request := OwnerFinishRequest{Outcome: "succeeded", Reply: reply, Actions: []ActionReceipt{{MessageID: review.MessageID, Status: "stored"}}, Observation: json.RawMessage("null")}
	var finished OwnerFinishReceipt
	if err := json.Unmarshal(f.call("orchestrator", "POST", "/owner-turns/"+turnID+"/finish", request, 201), &finished); err != nil {
		t.Fatal(err)
	}
	return turnID, request, finished
}

func TestFinalReviewReplyRequiresFeatureCompletionAndIsIdempotent(t *testing.T) {
	f := newHumanFixture(t)
	initial := f.ownerTurn()
	a := f.dispatch(initial, "worker-a")
	b := f.dispatch(initial, "worker-b")
	a.ProtocolVersion, b.ProtocolVersion = 2, 2
	f.post("orchestrator", a, 201)
	f.post("orchestrator", b, 201)
	f.finish(initial, "", []ActionReceipt{{MessageID: a.MessageID, Status: "stored"}, {MessageID: b.MessageID, Status: "stored"}}, 201)

	firstTurn, firstRequest, first := finishReviewedResult(t, f, a, "accepted", "Both results are complete.")
	if first.ReplyStatus != "none" || len(f.history().SlackOutbox) != 0 {
		t.Fatalf("premature reply escaped: receipt=%+v outbox=%+v", first, f.history().SlackOutbox)
	}
	var repeated OwnerFinishReceipt
	if err := json.Unmarshal(f.call("orchestrator", "POST", "/owner-turns/"+firstTurn+"/finish", firstRequest, 200), &repeated); err != nil {
		t.Fatal(err)
	}
	if repeated.ReplyStatus != "none" || len(f.history().SlackOutbox) != 0 {
		t.Fatalf("duplicate finish invented a reply: %+v", repeated)
	}
	_, _, last := finishReviewedResult(t, f, b, "accepted", "Both results are complete.")
	if last.ReplyStatus != "queued" {
		t.Fatalf("final review did not queue its reply: %+v", last)
	}
	h := f.history()
	if len(h.SlackOutbox) != 1 || h.SlackOutbox[0].Text != "Both results are complete." {
		t.Fatalf("final reply outbox=%+v", h.SlackOutbox)
	}
	var summaries int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM audit WHERE event='owner_summary_queued'`).Scan(&summaries); err != nil || summaries != 0 {
		t.Fatalf("extra summary after final reply: count=%d err=%v", summaries, err)
	}
}

func TestActionFreeReplyCannotCompleteUnreviewedResult(t *testing.T) {
	f := newHumanFixture(t)
	initial := f.ownerTurn()
	dispatch := f.dispatch(initial, "worker-a")
	dispatch.ProtocolVersion = f.s.wireVersion()
	f.post("orchestrator", dispatch, 201)
	f.finish(initial, "", []ActionReceipt{{MessageID: dispatch.MessageID, Status: "stored"}}, 201)
	accepted := f.event(dispatch, "task.accepted", AcceptedPayload{DispatchMessageID: dispatch.MessageID, ProfileRevision: f.profiles[dispatch.ToAgentID].Revision}, dispatch.MessageID)
	accepted.ProtocolVersion = f.s.wireVersion()
	f.post(dispatch.ToAgentID, accepted, 201)
	started := f.event(dispatch, "task.started", StartedPayload{AcceptedMessageID: accepted.MessageID}, accepted.MessageID)
	started.ProtocolVersion = f.s.wireVersion()
	f.post(dispatch.ToAgentID, started, 201)
	result := f.event(dispatch, "task.result", resultPayload(), started.MessageID)
	result.ProtocolVersion = f.s.wireVersion()
	receipt := f.post(dispatch.ToAgentID, result, 201)
	turnID := NewID()
	f.call("orchestrator", "POST", "/owner-turns/start", OwnerStartRequest{TurnID: turnID, FeatureID: f.feature.FeatureID, InputMailboxSeq: receipt.MailboxSeq}, 201)
	request := OwnerFinishRequest{Outcome: "succeeded", Reply: "Everything is done.", Actions: []ActionReceipt{}, Observation: json.RawMessage("null")}
	var finished OwnerFinishReceipt
	if err := json.Unmarshal(f.call("orchestrator", "POST", "/owner-turns/"+turnID+"/finish", request, 201), &finished); err != nil {
		t.Fatal(err)
	}
	if finished.ReplyStatus != "none" || len(f.history().SlackOutbox) != 0 || f.history().Jobs[0].Attempts[0].Review != "pending" {
		t.Fatalf("unreviewed result escaped as final reply: %+v", finished)
	}
}

func TestReviewRevisionCannotPublishFinalReply(t *testing.T) {
	f := newHumanFixture(t)
	initial := f.ownerTurn()
	d := f.dispatch(initial, "worker-a")
	d.ProtocolVersion = 2
	f.post("orchestrator", d, 201)
	f.finish(initial, "", []ActionReceipt{{MessageID: d.MessageID, Status: "stored"}}, 201)
	_, _, finished := finishReviewedResult(t, f, d, "revise", "Done.")
	if finished.ReplyStatus != "none" || len(f.history().SlackOutbox) != 0 {
		t.Fatalf("revise review published a final reply: %+v", finished)
	}
}

func TestOpenHumanQuestionSuppressesReviewReply(t *testing.T) {
	f := newHumanFixture(t)
	initial := f.ownerTurn()
	d := f.dispatch(initial, "worker-a")
	d.ProtocolVersion = 2
	f.post("orchestrator", d, 201)
	f.finish(initial, "", []ActionReceipt{{MessageID: d.MessageID, Status: "stored"}}, 201)
	requestID := publishedHumanRequest(t, f, []HumanOption{{ID: "approve", Label: "Разрешить"}}, "sent")
	_, _, finished := finishReviewedResult(t, f, d, "accepted", "Все готово.")
	if finished.ReplyStatus != "none" {
		t.Fatalf("open human question did not suppress final reply: %+v", finished)
	}
	for _, outbox := range f.history().SlackOutbox {
		if outbox.ID != requestID {
			t.Fatalf("review reply appeared beside open question: %+v", outbox)
		}
	}
}

func TestNewHumanInputSuppressesStaleReviewReply(t *testing.T) {
	f := newHumanFixture(t)
	initial := f.ownerTurn()
	d := f.dispatch(initial, "worker-a")
	d.ProtocolVersion = 2
	f.post("orchestrator", d, 201)
	f.finish(initial, "", []ActionReceipt{{MessageID: d.MessageID, Status: "stored"}}, 201)
	_, _, finished := finishReviewedResult(t, f, d, "accepted", "Все готово.", func() {
		_, _, err := f.s.Ingest(context.Background(), f.feature.FeatureID, InputPayload{Text: "Подожди, у меня ещё вопрос.", Source: Source{Kind: "slack", EventID: NewID(), ChannelID: f.feature.ChannelID, ThreadTS: f.feature.ThreadTS, ActorID: "human"}})
		if err != nil {
			t.Fatal(err)
		}
	})
	if finished.ReplyStatus != "none" || len(f.history().SlackOutbox) != 0 {
		t.Fatalf("newer human input did not suppress stale final reply: %+v", finished)
	}
}

func TestStopSuppressesQueuedFinalReplyButPreservesStopNotice(t *testing.T) {
	f := newHumanFixture(t)
	initial := f.ownerTurn()
	d := f.dispatch(initial, "worker-a")
	d.ProtocolVersion = 2
	f.post("orchestrator", d, 201)
	f.finish(initial, "", []ActionReceipt{{MessageID: d.MessageID, Status: "stored"}}, 201)
	_, _, finished := finishReviewedResult(t, f, d, "accepted", "Все готово.")
	if finished.ReplyStatus != "queued" {
		t.Fatalf("expected final reply to be queued: %+v", finished)
	}
	if err := f.s.StopFeature(context.Background(), f.feature.FeatureID, "human", "Stop before Slack delivery"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.QueueSlackNotice(context.Background(), f.feature.FeatureID, "stop-notice", "Работа остановлена."); err != nil {
		t.Fatal(err)
	}
	delivery, err := f.s.ClaimSlack(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if delivery == nil || delivery.Text != "Работа остановлена." {
		t.Fatalf("stale final reply escaped or stop notice was lost: %+v", delivery)
	}
	h := f.history()
	if len(h.SlackOutbox) != 2 || h.SlackOutbox[0].Status != "suppressed" || h.Turns[len(h.Turns)-1].ReplyStatus != "suppressed" {
		t.Fatalf("queued reply not suppressed after stop: outbox=%+v turns=%+v", h.SlackOutbox, h.Turns)
	}
}

func TestDeferredStopTurnCannotReplyAfterContinue(t *testing.T) {
	f := newHumanFixture(t)
	ctx := context.Background()
	if err := f.s.StopFeature(ctx, f.feature.FeatureID, "human", "Slack stop"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.ContinueFeature(ctx, f.feature.FeatureID, "human"); err != nil {
		t.Fatal(err)
	}
	start := func(text string) string {
		t.Helper()
		receipt, _, err := f.s.IngestUrgent(ctx, f.feature.FeatureID, InputPayload{Text: text, Source: Source{Kind: "slack", EventID: NewID(), ChannelID: f.feature.ChannelID, ThreadTS: f.feature.ThreadTS, ActorID: "human"}})
		if err != nil {
			t.Fatal(err)
		}
		turnID := NewID()
		f.call("orchestrator", "POST", "/owner-turns/start", OwnerStartRequest{TurnID: turnID, FeatureID: f.feature.FeatureID, InputMailboxSeq: receipt.MailboxSeq}, 201)
		return turnID
	}
	stopTurn := start("!stop")
	finish := OwnerFinishRequest{Outcome: "succeeded", Reply: "Остановлено. Новые действия не предпринимаю.", Actions: []ActionReceipt{}, Observation: json.RawMessage("null")}
	var stopped OwnerFinishReceipt
	if err := json.Unmarshal(f.call("orchestrator", "POST", "/owner-turns/"+stopTurn+"/finish", finish, 201), &stopped); err != nil {
		t.Fatal(err)
	}
	if stopped.ReplyStatus != "suppressed" || len(f.history().SlackOutbox) != 0 {
		t.Fatalf("stale stop reply escaped after continue: %+v", stopped)
	}
	var repeated OwnerFinishReceipt
	if err := json.Unmarshal(f.call("orchestrator", "POST", "/owner-turns/"+stopTurn+"/finish", finish, 200), &repeated); err != nil {
		t.Fatal(err)
	}
	if repeated.ReplyStatus != "suppressed" || len(f.history().SlackOutbox) != 0 {
		t.Fatalf("duplicate stop finish published a reply: %+v", repeated)
	}
	continueTurn := start("!continue")
	var resumed OwnerFinishReceipt
	if err := json.Unmarshal(f.call("orchestrator", "POST", "/owner-turns/"+continueTurn+"/finish", OwnerFinishRequest{Outcome: "succeeded", Reply: "Работа продолжается.", Actions: []ActionReceipt{}, Observation: json.RawMessage("null")}, 201), &resumed); err != nil {
		t.Fatal(err)
	}
	if resumed.ReplyStatus != "queued" || len(f.history().SlackOutbox) != 1 || f.history().SlackOutbox[0].Text != "Работа продолжается." {
		t.Fatalf("continue reply lost after stop suppression: %+v", resumed)
	}
}

func TestDeniedEarlierJobDoesNotBlockNewIndependentFinalReply(t *testing.T) {
	f := newHumanFixture(t)
	initial := f.ownerTurn()
	blocked := f.dispatch(initial, "worker-a")
	blocked.ProtocolVersion = 2
	f.post("orchestrator", blocked, 201)
	f.finish(initial, "", []ActionReceipt{{MessageID: blocked.MessageID, Status: "stored"}}, 201)
	accepted := f.event(blocked, "task.accepted", AcceptedPayload{DispatchMessageID: blocked.MessageID, ProfileRevision: f.profiles[blocked.ToAgentID].Revision}, blocked.MessageID)
	accepted.ProtocolVersion = 2
	f.post(blocked.ToAgentID, accepted, 201)
	started := f.event(blocked, "task.started", StartedPayload{AcceptedMessageID: accepted.MessageID}, accepted.MessageID)
	started.ProtocolVersion = 2
	f.post(blocked.ToAgentID, started, 201)
	result := f.event(blocked, "task.result", ResultPayload{Outcome: "blocked", Summary: "Needs a human choice", Evidence: []Evidence{}, Origin: "worker", Blocker: ptrBlocker(humanBlocker())}, started.MessageID)
	result.ProtocolVersion = 2
	f.post(blocked.ToAgentID, result, 201)
	dep := f.history().Dependencies[0]
	dep.State = "denied"
	if err := f.s.transaction(context.Background(), func(tx *sql.Tx) error {
		return saveDependency(context.Background(), tx, dep)
	}); err != nil {
		t.Fatal(err)
	}

	next := f.ownerTurn()
	independent := f.dispatch(next, "worker-b")
	independent.ProtocolVersion = 2
	f.post("orchestrator", independent, 201)
	f.finish(next, "", []ActionReceipt{{MessageID: independent.MessageID, Status: "stored"}}, 201)
	_, _, finished := finishReviewedResult(t, f, independent, "accepted", "Новая работа завершена.")
	if finished.ReplyStatus != "queued" {
		t.Fatalf("terminal denial of earlier work blocked an independent final reply: %+v", finished)
	}
}
