package swarm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
)

// OwnerRedeliveryRequest is offline operator evidence, never an agent or Slack
// action. It identifies the failed turn and the exact immutable result/review.
type OwnerRedeliveryRequest struct {
	FeatureID       string `json:"feature_id"`
	FailedTurnID    string `json:"failed_turn_id"`
	OriginalSeq     int64  `json:"original_mailbox_seq"`
	ResultMessageID string `json:"result_message_id"`
	ReviewMessageID string `json:"review_message_id"`
	Actor           string `json:"actor"`
	Reason          string `json:"reason"`
}

type OwnerRedeliveryReceipt struct {
	FailedTurnID    string `json:"failed_turn_id"`
	ResultMessageID string `json:"result_message_id"`
	OriginalSeq     int64  `json:"original_mailbox_seq"`
	NewSeq          int64  `json:"new_mailbox_seq"`
	Status          string `json:"status"`
}

// RedeliverOwnerResultOffline must be invoked with the coordinator stopped.
// Open holds its exclusive state lock throughout the transaction. It creates
// only a mailbox delivery; the task.result, worker attempt, and human decision
// remain unchanged.
func RedeliverOwnerResultOffline(ctx context.Context, path string, cfg Config, r OwnerRedeliveryRequest) (OwnerRedeliveryReceipt, error) {
	if _, err := os.Stat(path); err != nil {
		return OwnerRedeliveryReceipt{}, err
	}
	s, err := openStore(ctx, path, cfg, false)
	if err != nil {
		return OwnerRedeliveryReceipt{}, err
	}
	defer s.Close()
	return s.redeliverOwnerResult(ctx, r)
}

func (s *Store) redeliverOwnerResult(ctx context.Context, r OwnerRedeliveryRequest) (OwnerRedeliveryReceipt, error) {
	var out OwnerRedeliveryReceipt
	if s.cfg.WireVersion != 2 || !uuid(r.FeatureID) || !uuid(r.FailedTurnID) || r.OriginalSeq < 1 || !uuid(r.ResultMessageID) || !uuid(r.ReviewMessageID) || !safeText(r.Actor, 256) || !safeText(r.Reason, 4096) {
		return out, wireError(400, "invalid_owner_recovery")
	}
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		var old OwnerRedeliveryRequest
		var newSeq int64
		err := tx.QueryRowContext(ctx, `SELECT feature_id,original_seq,result_message_id,review_message_id,new_seq,actor,reason FROM owner_redeliveries WHERE failed_turn_id=?`, r.FailedTurnID).Scan(&old.FeatureID, &old.OriginalSeq, &old.ResultMessageID, &old.ReviewMessageID, &newSeq, &old.Actor, &old.Reason)
		if err == nil {
			old.FailedTurnID = r.FailedTurnID
			if old != r {
				return wireError(409, "idempotency_conflict")
			}
			out = OwnerRedeliveryReceipt{r.FailedTurnID, r.ResultMessageID, r.OriginalSeq, newSeq, "queued"}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		f, err := feature(ctx, tx, r.FeatureID)
		if err != nil {
			return err
		}
		if f.Stopped {
			return wireError(409, "feature_stopped")
		}
		if err = barrier(ctx, tx, r.FeatureID); err != nil {
			return err
		}
		t, err := turn(ctx, tx, r.FailedTurnID)
		if err != nil {
			return err
		}
		if t.FeatureID != r.FeatureID || t.AgentID != f.OwnerAgentID || t.InputMailboxSeq != r.OriginalSeq || t.State != "failed" || t.Finish == nil || t.Finish.Outcome != "failed" || t.Error == nil || t.Error.Code != "action_rejected" {
			return wireError(409, "wrong_failed_turn")
		}
		var rejectedTransition, acceptedReview bool
		for _, action := range t.Finish.Actions {
			if action.Status == "rejected" && action.ErrorCode != nil && *action.ErrorCode == "transition_conflict" {
				rejectedTransition = true
			}
			if action.Status == "stored" && action.MessageID == r.ReviewMessageID {
				acceptedReview = true
			}
		}
		if len(t.Finish.Actions) != 2 || !rejectedTransition || !acceptedReview {
			return wireError(409, "wrong_failure_reason")
		}
		var running int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM owner_turns WHERE agent_id=? AND state='running'", f.OwnerAgentID).Scan(&running); err != nil {
			return err
		}
		if running != 0 {
			return wireError(409, "owner_busy")
		}
		var messageRow int64
		var source, reviewRaw []byte
		var acked, superseded, size int
		err = tx.QueryRowContext(ctx, `SELECT m.id,m.canonical,d.acked,d.superseded,d.bytes FROM mailbox_delivery d JOIN messages m ON m.id=d.message_row WHERE d.agent_id=? AND d.seq=? AND m.kind='task.result' AND m.message_id=?`, f.OwnerAgentID, r.OriginalSeq, r.ResultMessageID).Scan(&messageRow, &source, &acked, &superseded, &size)
		if errors.Is(err, sql.ErrNoRows) {
			return wireError(409, "wrong_result_trigger")
		}
		if err != nil {
			return err
		}
		var result Envelope
		if err = json.Unmarshal(source, &result); err != nil {
			return err
		}
		if acked != 1 || superseded != 0 || result.FeatureID != r.FeatureID || result.ToAgentID != f.OwnerAgentID || result.Type != "task.result" {
			return wireError(409, "wrong_result_trigger")
		}
		a, err := attempt(ctx, tx, result.AttemptID)
		if err != nil {
			return err
		}
		if a.FeatureID != r.FeatureID || a.JobID != result.JobID || a.ResultMessageID != result.MessageID || a.State != "succeeded" || a.Review != "accepted" {
			return wireError(409, "result_not_reviewed")
		}
		err = tx.QueryRowContext(ctx, `SELECT canonical FROM messages WHERE sender=? AND message_id=? AND feature_id=? AND turn_id=? AND kind='task.review' AND attempt_id=?`, f.OwnerAgentID, r.ReviewMessageID, r.FeatureID, r.FailedTurnID, result.AttemptID).Scan(&reviewRaw)
		if errors.Is(err, sql.ErrNoRows) {
			return wireError(409, "review_not_in_failed_turn")
		}
		if err != nil {
			return err
		}
		var review Envelope
		var reviewPayload ReviewPayload
		if err = json.Unmarshal(reviewRaw, &review); err != nil {
			return err
		}
		if err = json.Unmarshal(review.Payload, &reviewPayload); err != nil {
			return err
		}
		if review.JobID != result.JobID || review.CausationID == nil || *review.CausationID != result.MessageID || reviewPayload.ResultMessageID != result.MessageID || reviewPayload.Verdict != "accepted" {
			return wireError(409, "wrong_result_review")
		}
		var storedActions, otherActions int
		if err = tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(CASE WHEN kind!='task.review' OR message_id!=? THEN 1 ELSE 0 END),0) FROM messages WHERE sender=? AND turn_id=?`, r.ReviewMessageID, f.OwnerAgentID, r.FailedTurnID).Scan(&storedActions, &otherActions); err != nil {
			return err
		}
		if storedActions != 1 || otherActions != 0 {
			return wireError(409, "unsafe_owner_actions")
		}
		var pending int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM mailbox_delivery d JOIN messages m ON m.id=d.message_row WHERE d.agent_id=? AND m.feature_id=? AND d.acked=0 AND d.superseded=0`, f.OwnerAgentID, r.FeatureID).Scan(&pending); err != nil {
			return err
		}
		if pending != 0 {
			return wireError(409, "owner_mailbox_pending")
		}
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM mailbox_delivery WHERE agent_id=? AND acked=0 AND superseded=0 AND pending_notification=0`, f.OwnerAgentID).Scan(&pending); err != nil {
			return err
		}
		if pending >= s.mailboxLimit+s.mailboxReserve {
			return wireError(429, "mailbox_full")
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO mailbox_counters(agent_id,seq) VALUES(?,1) ON CONFLICT(agent_id) DO UPDATE SET seq=seq+1`, f.OwnerAgentID); err != nil {
			return err
		}
		if err = tx.QueryRowContext(ctx, `SELECT seq FROM mailbox_counters WHERE agent_id=?`, f.OwnerAgentID).Scan(&newSeq); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO mailbox_delivery(agent_id,seq,message_row,lane,pending_notification,bytes) VALUES(?,?,?,'normal',0,?)`, f.OwnerAgentID, newSeq, messageRow, size); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO owner_redeliveries(failed_turn_id,feature_id,original_seq,result_message_id,review_message_id,new_seq,actor,reason,at) VALUES(?,?,?,?,?,?,?,?,?)`, r.FailedTurnID, r.FeatureID, r.OriginalSeq, r.ResultMessageID, r.ReviewMessageID, newSeq, r.Actor, r.Reason, s.stamp()); err != nil {
			return err
		}
		if err = s.audit(ctx, tx, Principal{AgentID: r.Actor}, Envelope{FeatureID: r.FeatureID, MessageID: r.ResultMessageID, JobID: result.JobID, AttemptID: result.AttemptID, OwnerTurnID: r.FailedTurnID}, "operator_owner_redelivered", "summary_only"); err != nil {
			return err
		}
		out = OwnerRedeliveryReceipt{r.FailedTurnID, r.ResultMessageID, r.OriginalSeq, newSeq, "queued"}
		return nil
	})
	return out, err
}
