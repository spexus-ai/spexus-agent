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
