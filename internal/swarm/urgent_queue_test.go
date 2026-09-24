package swarm

import (
	"context"
	"encoding/json"
	"testing"
)

func TestUrgentInputsKeepIdentityAndPrecedeBufferedOrdinaryTurns(t *testing.T) {
	f := newHumanFixture(t)
	ctx := context.Background()
	input := func(eventID, text string) InputPayload {
		return InputPayload{Text: text, Source: Source{Kind: "slack", EventID: eventID, ChannelID: f.feature.ChannelID, ThreadTS: f.feature.ThreadTS, ActorID: "human"}}
	}
	ordinary, _, err := f.s.Ingest(ctx, f.feature.FeatureID, input("ordinary", "old buffered message"))
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := f.s.IngestUrgent(ctx, f.feature.FeatureID, input("urgent-1", "!first correction"))
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := f.s.IngestUrgent(ctx, f.feature.FeatureID, input("urgent-2", "!second correction"))
	if err != nil {
		t.Fatal(err)
	}
	if ordinary.MailboxSeq >= first.MailboxSeq || first.MailboxSeq >= second.MailboxSeq {
		t.Fatal("source identity or durable sequence was reordered")
	}
	if again, duplicate, err := f.s.IngestUrgent(ctx, f.feature.FeatureID, input("urgent-1", "!first correction")); err != nil || !duplicate || again != first {
		t.Fatalf("urgent replay changed receipt: %+v, %t, %v", again, duplicate, err)
	}
	if _, _, err := f.s.IngestUrgent(ctx, f.feature.FeatureID, input("ordinary-2", "plain text")); err == nil {
		t.Fatal("nonurgent input entered urgent interface")
	}
	var mailbox MailboxResponse
	if err := json.Unmarshal(f.call("orchestrator", "GET", "/mailbox?wait_seconds=0", nil, 200), &mailbox); err != nil {
		t.Fatal(err)
	}
	if len(mailbox.Messages) != 3 || mailbox.Messages[0].MailboxSeq != first.MailboxSeq || mailbox.Messages[1].MailboxSeq != second.MailboxSeq || mailbox.Messages[2].MailboxSeq != ordinary.MailboxSeq {
		t.Fatalf("urgent mailbox order is wrong: %+v", mailbox.Messages)
	}
	start := func(seq int64, status int) string {
		id := NewID()
		f.call("orchestrator", "POST", "/owner-turns/start", OwnerStartRequest{TurnID: id, FeatureID: f.feature.FeatureID, InputMailboxSeq: seq}, status)
		return id
	}
	start(ordinary.MailboxSeq, 409)
	start(second.MailboxSeq, 409)
	firstTurn := start(first.MailboxSeq, 201)
	f.finish(firstTurn, "", []ActionReceipt{}, 201)
	secondTurn := start(second.MailboxSeq, 201)
	f.finish(secondTurn, "", []ActionReceipt{}, 201)
	ordinaryTurn := start(ordinary.MailboxSeq, 201)
	f.finish(ordinaryTurn, "", []ActionReceipt{}, 201)
	if len(f.history().Turns) != 3 {
		t.Fatal("deferred ordinary input was lost or replayed")
	}
}
