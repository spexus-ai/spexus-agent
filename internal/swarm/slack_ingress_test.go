package swarm

import (
	"context"
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
