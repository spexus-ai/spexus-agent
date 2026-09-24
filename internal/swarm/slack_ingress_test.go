package swarm

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// Test: Socket Mode and thread-history copies of one Slack message resolve to
// one committed source, while an edit at the same timestamp is rejected.
// Validates: AC-431/432 (REQ-349/350 - source dedup and immutable provenance).
func TestSlackSourceDedupAndConflict(t *testing.T) {
	f := newHumanFixture(t)
	ctx := context.Background()
	in := SlackSource{WorkspaceID: "workspace", ChannelID: f.feature.ChannelID, ThreadTS: f.feature.ThreadTS, MessageTS: "123.000005", FeatureID: f.feature.FeatureID, ActorID: "human", Text: "!stop", EventID: "socket-event"}
	if duplicate, err := f.s.CommitSlackSource(ctx, in); err != nil || duplicate {
		t.Fatalf("first commit duplicate=%t err=%v", duplicate, err)
	}
	in.EventID = ""
	if duplicate, err := f.s.CommitSlackSource(ctx, in); err != nil || !duplicate {
		t.Fatalf("history duplicate=%t err=%v", duplicate, err)
	}
	pending, err := f.s.PendingSlackSources(ctx, f.feature.FeatureID)
	if err != nil || len(pending) != 1 || pending[0].Text != "!stop" {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	in.Text = "!continue"
	if _, err := f.s.CommitSlackSource(ctx, in); err == nil {
		t.Fatal("edited source accepted")
	}
	in.Text = "!stop"
	if err := f.s.SettleSlackSource(ctx, in); err != nil {
		t.Fatal(err)
	}
	pending, err = f.s.PendingSlackSources(ctx, f.feature.FeatureID)
	if err != nil || len(pending) != 0 {
		t.Fatalf("settled source pending=%+v err=%v", pending, err)
	}
}

func TestFeatureStopButtonSourceIsScopedAndIdempotent(t *testing.T) {
	f := newHumanFixture(t)
	ctx := context.Background()
	in := SlackSource{WorkspaceID: "workspace", ChannelID: f.feature.ChannelID, ThreadTS: f.feature.ThreadTS, MessageTS: "124.000001", FeatureID: f.feature.FeatureID, ActorID: "human", SourceKind: "feature_control", QuestionTS: f.feature.ThreadTS, OptionID: "stop", Text: "!stop"}
	for _, tc := range []struct {
		name   string
		mutate func(*SlackSource)
	}{
		{"wrong anchor", func(s *SlackSource) { s.QuestionTS = "1.000002" }},
		{"foreign actor", func(s *SlackSource) { s.ActorID = "other" }},
		{"foreign channel", func(s *SlackSource) { s.ChannelID = "other" }},
		{"forged command", func(s *SlackSource) { s.Text = "!continue" }},
		{"request attached", func(s *SlackSource) { s.RequestID = NewID() }},
	} {
		bad := in
		tc.mutate(&bad)
		if _, err := f.s.CommitSlackSource(ctx, bad); err == nil {
			t.Fatalf("%s accepted", tc.name)
		}
	}
	if duplicate, err := f.s.CommitSlackSource(ctx, in); err != nil || duplicate {
		t.Fatalf("first stop control duplicate=%t err=%v", duplicate, err)
	}
	if duplicate, err := f.s.CommitSlackSource(ctx, in); err != nil || !duplicate {
		t.Fatalf("replayed stop control duplicate=%t err=%v", duplicate, err)
	}
	committed, err := f.s.CommittedSlackSource(ctx, in.WorkspaceID, in.ChannelID, in.MessageTS)
	if err != nil || committed.SourceKind != "feature_control" || committed.Text != "!stop" {
		t.Fatalf("committed stop control=%+v err=%v", committed, err)
	}
}

// Test: the click is committed only for the published request's exact channel,
// thread and bot message. A repeated Socket envelope has one local source.
// Validates: AC-431/464 (REQ-349/391 - click provenance and deduplication).
func TestHumanButtonSourceRequiresPublishedQuestion(t *testing.T) {
	f := newHumanFixture(t)
	ctx := context.Background()
	requestID := NewID()
	question := SlackDelivery{ID: requestID, FeatureID: f.feature.FeatureID, ChannelID: f.feature.ChannelID, ThreadTS: f.feature.ThreadTS, SlackTS: "123.000005", Status: "sent"}
	if err := f.s.transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO slack_outbox(id,feature_id,turn_id,status,data) VALUES(?,?,NULL,?,?)", question.ID, question.FeatureID, question.Status, mustJSON(question))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if featureID, thread, err := f.s.PublishedHumanQuestion(ctx, requestID, question.SlackTS); err != nil || featureID != f.feature.FeatureID || thread != f.feature.ThreadTS {
		t.Fatalf("published question scope feature=%q thread=%q err=%v", featureID, thread, err)
	}
	if _, _, err := f.s.PublishedHumanQuestion(ctx, requestID, "123.000006"); err == nil {
		t.Fatal("wrong bot timestamp resolved feature")
	}
	base := SlackSource{WorkspaceID: "workspace", ChannelID: f.feature.ChannelID, ThreadTS: f.feature.ThreadTS, MessageTS: "124.000001", FeatureID: f.feature.FeatureID, ActorID: "human", SourceKind: "block_action", QuestionTS: question.SlackTS, RequestID: requestID, OptionID: "a"}
	for _, tc := range []struct {
		name   string
		change func(*SlackSource)
	}{
		{"wrong question", func(s *SlackSource) { s.QuestionTS = "123.000006" }},
		{"wrong request", func(s *SlackSource) { s.RequestID = NewID() }},
		{"foreign actor", func(s *SlackSource) { s.ActorID = "other" }},
		{"foreign thread", func(s *SlackSource) { s.ThreadTS = "123.000009" }},
	} {
		in := base
		tc.change(&in)
		if _, err := f.s.CommitSlackSource(ctx, in); err == nil {
			t.Fatalf("%s accepted", tc.name)
		}
	}
	if duplicate, err := f.s.CommitSlackSource(ctx, base); err != nil || duplicate {
		t.Fatalf("first click duplicate=%t err=%v", duplicate, err)
	}
	if duplicate, err := f.s.CommitSlackSource(ctx, base); err != nil || !duplicate {
		t.Fatalf("repeat click duplicate=%t err=%v", duplicate, err)
	}
	conflict := base
	conflict.OptionID = "b"
	if _, err := f.s.CommitSlackSource(ctx, conflict); err == nil {
		t.Fatal("one click timestamp changed option")
	}
	control := base
	control.MessageTS, control.SourceKind, control.OptionID, control.Text = "124.000002", "button_control", "stop", "!stop"
	if duplicate, err := f.s.CommitSlackSource(ctx, control); err != nil || duplicate {
		t.Fatalf("stop control commit duplicate=%t err=%v", duplicate, err)
	}
	if duplicate, err := f.s.CommitSlackSource(ctx, control); err != nil || !duplicate {
		t.Fatalf("stop control redelivery duplicate=%t err=%v", duplicate, err)
	}
	control.MessageTS, control.OptionID, control.Text = "124.000003", "details", HumanDetailsControlText
	if _, err := f.s.CommitSlackSource(ctx, control); err == nil {
		t.Fatal("details control on inactive question accepted")
	}
	control.MessageTS, control.OptionID, control.Text = "124.000004", "stop", "!continue"
	if _, err := f.s.CommitSlackSource(ctx, control); err == nil {
		t.Fatal("control value forged an unsafe command")
	}
}

func TestDetailsControlRequiresActiveQuestionAndDedupsAfterClose(t *testing.T) {
	f := newHumanFixture(t)
	ctx := context.Background()
	requestID := publishedHumanRequest(t, f, nil, "sent")
	in := SlackSource{WorkspaceID: "workspace", ChannelID: f.feature.ChannelID, ThreadTS: f.feature.ThreadTS, MessageTS: "124.000001", FeatureID: f.feature.FeatureID, ActorID: "human", SourceKind: "button_control", QuestionTS: "123.000005", RequestID: requestID, OptionID: "details", Text: HumanDetailsControlText}
	if duplicate, err := f.s.CommitSlackSource(ctx, in); err != nil || duplicate {
		t.Fatalf("active details control duplicate=%t err=%v", duplicate, err)
	}
	committed, err := f.s.CommittedSlackSource(ctx, in.WorkspaceID, in.ChannelID, in.MessageTS)
	if err != nil || committed.ActiveHumanRequest == nil || committed.ActiveHumanRequest.RequestID != requestID || committed.ActiveHumanRequest.Reason != "Decision needed" || committed.ActiveHumanRequest.Context != "test" {
		t.Fatalf("details context=%+v err=%v", committed.ActiveHumanRequest, err)
	}
	if _, err := f.s.db.ExecContext(ctx, `UPDATE human_projections SET state='answered' WHERE request_id=?`, requestID); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := f.s.CommitSlackSource(ctx, in); err != nil || !duplicate {
		t.Fatalf("redelivery after close duplicate=%t err=%v", duplicate, err)
	}
	in.MessageTS = "124.000002"
	if _, err := f.s.CommitSlackSource(ctx, in); err == nil {
		t.Fatal("new details click on closed question accepted")
	}
}

func TestStopDoesNotPublishTechnicalCancellationNotice(t *testing.T) {
	f := newHumanFixture(t)
	ctx := context.Background()
	_ = publishedHumanRequest(t, f, nil, "sent")
	if err := f.s.StopFeature(ctx, f.feature.FeatureID, "human", "Slack stop control"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.s.db.QueryRowContext(ctx, "SELECT count(*) FROM slack_outbox WHERE feature_id=?", f.feature.FeatureID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("stop published a duplicate or technical notice: count=%d err=%v", count, err)
	}
}

// Test: a valid but oversized request is rejected before backend create, so
// Slack cannot truncate the decision options or answer syntax.
// Validates: AC-464 (REQ-391 - complete human question and response options).
func TestHumanQuestionMustFitSlackPublication(t *testing.T) {
	f := newHumanFixture(t)
	ctx := context.Background()
	p := HumanRequestPayload{StepKey: "approval", BlockedWork: strings.Repeat("w", 7900), Blocker: Blocker{Reason: strings.Repeat("r", 4000), Context: strings.Repeat("c", 8000), Question: strings.Repeat("q", 4000), Recommendation: strings.Repeat("m", 4000), Kind: "choice", Options: []HumanOption{}}}
	for i := 0; i < 8; i++ {
		p.Options = append(p.Options, HumanOption{ID: string(rune('a' + i)), Label: strings.Repeat("L", 1000)})
	}
	if err := validateBlocker(p.Blocker); err != nil {
		t.Fatalf("fixture invalid: %v", err)
	}
	e := Envelope{FeatureID: f.feature.FeatureID, OwnerTurnID: NewID(), MessageID: NewID()}
	err := f.s.transaction(ctx, func(tx *sql.Tx) error { return f.s.humanRequest(ctx, tx, e, p) })
	var api *APIError
	if !errors.As(err, &api) || api.Code != "slack_question_too_large" {
		t.Fatalf("oversized question accepted: %v", err)
	}
	var n int
	if err := f.s.db.QueryRowContext(ctx, "SELECT count(*) FROM backend_sync_operations").Scan(&n); err != nil || n != 0 {
		t.Fatalf("canonical create queued for truncated question: %d %v", n, err)
	}
}

// Test: the catchup watermark survives repeated reads and advances only
// after the caller reports a complete history scan.
// Validates: AC-432 (REQ-350 - restart-safe Slack catchup).
func TestSlackCatchupWatermark(t *testing.T) {
	f := newHumanFixture(t)
	ctx := context.Background()
	first, err := f.s.SlackWatermark(ctx, f.feature.FeatureID)
	if err != nil || !validSlackTS(first) {
		t.Fatalf("initial watermark=%q err=%v", first, err)
	}
	next := "9999999999.000001"
	if err := f.s.AdvanceSlackWatermark(ctx, f.feature.FeatureID, next); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.SlackWatermark(ctx, f.feature.FeatureID); err != nil || got != next {
		t.Fatalf("watermark=%q err=%v", got, err)
	}
	if err := f.s.AdvanceSlackWatermark(ctx, f.feature.FeatureID, first); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.SlackWatermark(ctx, f.feature.FeatureID); err != nil || got != next {
		t.Fatalf("watermark regressed=%q err=%v", got, err)
	}
}
