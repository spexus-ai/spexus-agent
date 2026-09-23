package swarm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func ValidateEnvelope(e Envelope) error { return validateEnvelope(e) }
func (s *Store) postMessage(ctx context.Context, p Principal, e Envelope) (Receipt, bool, error) {
	var r Receipt
	duplicate := false
	err := validateEnvelope(e)
	if err == nil {
		err = s.transaction(ctx, func(tx *sql.Tx) error {
			if err := bound(ctx, tx, p); err != nil {
				return err
			}
			var err error
			r, duplicate, err = s.applyMessage(ctx, tx, p, e, false)
			return err
		})
	}
	if err != nil {
		s.reject(ctx, p, e, err)
	}
	return r, duplicate, err
}
func (s *Store) existing(ctx context.Context, tx *sql.Tx, e Envelope) (Receipt, bool, error) {
	var receipt Receipt
	canon, err := canonical(mustJSON(e))
	if err != nil {
		return receipt, false, err
	}
	var old, raw []byte
	err = tx.QueryRowContext(ctx, `SELECT canonical,receipt FROM messages WHERE sender=? AND message_id=? UNION ALL SELECT canonical,receipt FROM message_aliases WHERE sender=? AND message_id=? LIMIT 1`, e.FromAgentID, e.MessageID, e.FromAgentID, e.MessageID).Scan(&old, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return receipt, false, nil
	}
	if err != nil {
		return receipt, false, err
	}
	if string(old) != string(canon) {
		return receipt, false, wireError(409, "idempotency_conflict")
	}
	err = json.Unmarshal(raw, &receipt)
	return receipt, true, err
}
func (s *Store) phaseDuplicate(ctx context.Context, tx *sql.Tx, e Envelope) (Receipt, bool, error) {
	var r Receipt
	if e.Type != "task.accepted" && e.Type != "task.started" && e.Type != "task.result" && e.Type != "task.review" && e.Type != "task.dispatch" {
		return r, false, nil
	}
	var raw, rr []byte
	err := tx.QueryRowContext(ctx, "SELECT canonical,receipt FROM messages WHERE attempt_id=? AND kind=? LIMIT 1", e.AttemptID, e.Type).Scan(&raw, &rr)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	var previous Envelope
	if err = json.Unmarshal(raw, &previous); err != nil {
		return r, false, err
	}
	a, _ := canonical(previous.Payload)
	b, _ := canonical(e.Payload)
	// Message identity/time/owner turn are not business identity. Authority was checked before this path.
	if previous.FromAgentID != e.FromAgentID || previous.ToAgentID != e.ToAgentID || previous.JobID != e.JobID || previous.FeatureID != e.FeatureID || string(a) != string(b) {
		return r, false, wireError(409, "transition_conflict")
	}
	if (previous.CausationID == nil) != (e.CausationID == nil) || previous.CausationID != nil && *previous.CausationID != *e.CausationID {
		return r, false, wireError(409, "causation_conflict")
	}
	if err = json.Unmarshal(rr, &r); err != nil {
		return r, false, err
	}
	canon, _ := canonical(mustJSON(e))
	if _, err = tx.ExecContext(ctx, "INSERT INTO message_aliases VALUES(?,?,?,?)", e.FromAgentID, e.MessageID, canon, rr); err != nil {
		return r, false, err
	}
	return r, true, s.audit(ctx, tx, Principal{AgentID: e.FromAgentID}, e, "business_duplicate", "")
}
func (s *Store) applyMessage(ctx context.Context, tx *sql.Tx, p Principal, e Envelope, internal bool) (Receipt, bool, error) {
	var r Receipt
	if e.TenantID != s.cfg.TenantID || e.ProjectID != s.cfg.ProjectID || (!internal && (e.FromAgentID != p.AgentID || e.FromAgentID == "coordinator")) {
		return r, false, wireError(403, "scope_or_sender_mismatch")
	}
	f, err := feature(ctx, tx, e.FeatureID)
	if err != nil {
		return r, false, err
	}
	if f.TenantID != e.TenantID || f.ProjectID != e.ProjectID {
		return r, false, wireError(403, "scope_mismatch")
	}
	var targetRole string
	if err = tx.QueryRowContext(ctx, "SELECT role FROM agents WHERE agent_id=?", e.ToAgentID).Scan(&targetRole); err != nil {
		return r, false, wireError(404, "recipient_unknown")
	}
	ownerAction := e.Type == "task.dispatch" || e.Type == "task.review" || e.Type == "task.cancel"
	if !internal {
		if ownerAction {
			if e.FromAgentID != f.OwnerAgentID || targetRole != "worker" {
				return r, false, wireError(403, "forbidden")
			}
		}
		if e.Type == "agent.input" || e.Type == "turn.cancel" {
			return r, false, wireError(403, "coordinator_only")
		}
		if e.Type == "task.result" {
			var rp ResultPayload
			_ = json.Unmarshal(e.Payload, &rp)
			if rp.Origin != "worker" {
				return r, false, wireError(403, "forged_origin")
			}
		}
	}
	if r, found, err := s.existing(ctx, tx, e); found || err != nil {
		if found {
			err = s.audit(ctx, tx, p, e, "duplicate", "")
		}
		return r, found, err
	}
	var a attemptRecord
	if e.Type != "task.dispatch" && e.Type != "agent.input" && e.Type != "turn.cancel" {
		a, err = attempt(ctx, tx, e.AttemptID)
		if err != nil {
			return r, false, err
		}
		if a.JobID != e.JobID || a.FeatureID != e.FeatureID {
			return r, false, wireError(404, "not_found")
		}
		if ownerAction {
			if a.AssignedAgentID != e.ToAgentID {
				return r, false, wireError(403, "foreign_attempt")
			}
		} else if !internal && (a.AssignedAgentID != e.FromAgentID || e.ToAgentID != f.OwnerAgentID) {
			return r, false, wireError(403, "foreign_attempt")
		}
		var current string
		if err = tx.QueryRowContext(ctx, "SELECT current_attempt_id FROM jobs WHERE id=?", e.JobID).Scan(&current); err != nil {
			return r, false, err
		}
		if current != e.AttemptID {
			return r, false, wireError(409, "stale_attempt")
		}
	}
	if r, found, err := s.phaseDuplicate(ctx, tx, e); found || err != nil {
		return r, found, err
	}
	if ownerAction && !internal {
		t, err := turn(ctx, tx, e.OwnerTurnID)
		if err != nil {
			return r, false, err
		}
		if t.AgentID != p.AgentID || t.InstanceID != p.InstanceID || t.FeatureID != f.FeatureID {
			return r, false, wireError(403, "foreign_turn")
		}
		if t.State != "running" || t.CancelRequested || f.Stopped {
			return r, false, wireError(409, "owner_turn_inactive")
		}
		var actions int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE sender=? AND turn_id=?", p.AgentID, e.OwnerTurnID).Scan(&actions); err != nil {
			return r, false, err
		}
		if actions >= 8 {
			return r, false, wireError(429, "action_limit")
		}

	}
	now := s.stamp()
	switch e.Type {
	case "agent.input":
		if !internal || e.ToAgentID != f.OwnerAgentID {
			return r, false, wireError(403, "coordinator_only")
		}
	case "task.dispatch":
		if f.Stopped {
			return r, false, wireError(409, "feature_stopped")
		}
		var d DispatchPayload
		_ = json.Unmarshal(e.Payload, &d)
		var profileID string
		if err = tx.QueryRowContext(ctx, "SELECT profile_id FROM agents WHERE agent_id=?", e.ToAgentID).Scan(&profileID); err != nil {
			return r, false, err
		}
		if profileID != d.Profile.ID || s.profiles[profileID] != d.Profile {
			return r, false, wireError(422, "profile_unavailable")
		}
		if d.AcceptBy == "" {
			d.AcceptBy = s.now().UTC().Add(60 * time.Second).Format(time.RFC3339Nano)
		}
		deadline, _ := time.Parse(time.RFC3339Nano, d.AcceptBy)
		if !deadline.After(s.now()) || deadline.After(s.now().Add(300*time.Second)) {
			return r, false, wireError(400, "invalid_deadline")
		}
		if d.RunTimeoutSeconds == 0 {
			d.RunTimeoutSeconds = 600
		}
		var active, total int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM attempts WHERE agent_id=? AND state NOT IN ('succeeded','failed','cancelled','interrupted')", e.ToAgentID).Scan(&active); err != nil {
			return r, false, err
		}
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM attempts WHERE state NOT IN ('succeeded','failed','cancelled','interrupted')").Scan(&total); err != nil {
			return r, false, err
		}
		if active > 0 || total >= 2 {
			return r, false, wireError(429, "capacity_exceeded")
		}
		var current, featureID string
		err = tx.QueryRowContext(ctx, "SELECT current_attempt_id,feature_id FROM jobs WHERE id=?", e.JobID).Scan(&current, &featureID)
		if err == nil {
			if featureID != f.FeatureID {
				return r, false, wireError(404, "not_found")
			}
			old, err := attempt(ctx, tx, current)
			if err != nil {
				return r, false, err
			}
			if !terminal(old.State) || old.State == "interrupted" && !old.Reconciled {
				return r, false, wireError(409, "reconciliation_required")
			}
			if _, err = tx.ExecContext(ctx, "UPDATE jobs SET current_attempt_id=? WHERE id=?", e.AttemptID, e.JobID); err != nil {
				return r, false, err
			}
		} else if errors.Is(err, sql.ErrNoRows) {
			if _, err = tx.ExecContext(ctx, "INSERT INTO jobs VALUES(?,?,?)", e.JobID, f.FeatureID, e.AttemptID); err != nil {
				return r, false, err
			}
		} else {
			return r, false, err
		}
		a = attemptRecord{Attempt: Attempt{AttemptID: e.AttemptID, AssignedAgentID: e.ToAgentID, State: "queued", Review: "pending", DispatchMessageID: e.MessageID}, JobID: e.JobID, FeatureID: f.FeatureID, Dispatch: d}
		if _, err = tx.ExecContext(ctx, "INSERT INTO attempts(id,job_id,agent_id,state,data) VALUES(?,?,?,?,?)", a.AttemptID, e.JobID, e.ToAgentID, a.State, mustJSON(a)); err != nil {
			return r, false, wireError(409, "attempt_conflict")
		}
	case "task.accepted":
		var d AcceptedPayload
		_ = json.Unmarshal(e.Payload, &d)
		if a.State != "queued" || a.CancelRequested {
			return r, false, wireError(409, "invalid_transition")
		}
		deadline, _ := time.Parse(time.RFC3339Nano, a.Dispatch.AcceptBy)
		if !s.now().Before(deadline) {
			return r, false, wireError(409, "accept_deadline_expired")
		}
		if d.DispatchMessageID != a.DispatchMessageID || d.ProfileRevision != a.Dispatch.Profile.Revision || e.CausationID == nil || *e.CausationID != a.DispatchMessageID {
			return r, false, wireError(409, "causation_conflict")
		}
		a.State = "accepted"
		a.AcceptedAt = now
		a.AcceptedMessageID = e.MessageID
		a.InstanceID = p.InstanceID
		if err = saveAttempt(ctx, tx, a); err != nil {
			return r, false, err
		}
	case "task.started":
		var d StartedPayload
		_ = json.Unmarshal(e.Payload, &d)
		if a.State != "accepted" || a.CancelRequested {
			return r, false, wireError(409, "invalid_transition")
		}
		deadline, _ := time.Parse(time.RFC3339Nano, a.AcceptedAt)
		if !s.now().Before(deadline.Add(30 * time.Second)) {
			return r, false, wireError(409, "start_deadline_expired")
		}
		if d.AcceptedMessageID != a.AcceptedMessageID || e.CausationID == nil || *e.CausationID != a.AcceptedMessageID || a.InstanceID != p.InstanceID {
			return r, false, wireError(409, "causation_conflict")
		}
		a.State = "running"
		a.StartedAt = &now
		if err = saveAttempt(ctx, tx, a); err != nil {
			return r, false, err
		}
	case "task.result":
		var d ResultPayload
		_ = json.Unmarshal(e.Payload, &d)
		if terminal(a.State) {
			return r, false, wireError(409, "already_terminal")
		}
		if internal {
			if d.Origin != "coordinator" || d.Outcome == "succeeded" {
				return r, false, wireError(403, "forged_origin")
			}
		} else {
			if a.InstanceID != "" && a.InstanceID != p.InstanceID {
				return r, false, wireError(409, "instance_conflict")
			}
			if a.CancelRequested {
				if d.Outcome == "succeeded" || d.Outcome == "failed" {
					return r, false, wireError(409, "cancel_requested")
				}
				if e.CausationID == nil || *e.CausationID != a.CancelMessageID {
					return r, false, wireError(409, "causation_conflict")
				}
				if d.Outcome == "interrupted" && (d.Error == nil || d.Error.Code != "cancel_after_completion" || d.Observation == nil) {
					return r, false, wireError(400, "observation_required")
				}
			} else {
				if d.Outcome == "cancelled" || d.Outcome == "interrupted" {
					return r, false, wireError(409, "cancel_not_requested")
				}
				cause := a.DispatchMessageID
				if a.State == "running" {
					if err = tx.QueryRowContext(ctx, "SELECT message_id FROM messages WHERE attempt_id=? AND kind='task.started'", a.AttemptID).Scan(&cause); err != nil {
						return r, false, err
					}
				} else if a.State == "accepted" {
					cause = a.AcceptedMessageID
				}
				if e.CausationID == nil || *e.CausationID != cause {
					return r, false, wireError(409, "causation_conflict")
				}
				if a.State != "running" && (d.Outcome != "failed" || d.Error == nil) {
					return r, false, wireError(409, "invalid_transition")
				}
			}
		}
		a.State = d.Outcome
		a.Result = &d
		a.Error = d.Error
		a.CompletedAt = &now
		a.ResultMessageID = e.MessageID
		if err = saveAttempt(ctx, tx, a); err != nil {
			return r, false, err
		}
	case "task.review":
		var d ReviewPayload
		_ = json.Unmarshal(e.Payload, &d)
		if !terminal(a.State) || a.ResultMessageID != d.ResultMessageID || e.CausationID == nil || *e.CausationID != d.ResultMessageID {
			return r, false, wireError(409, "invalid_review")
		}
		a.Review = d.Verdict
		if err = saveAttempt(ctx, tx, a); err != nil {
			return r, false, err
		}
	case "task.cancel":
		if terminal(a.State) {
			return r, false, wireError(409, "already_terminal")
		}
		if a.CancelRequested {
			return r, false, wireError(409, "cancel_already_requested")
		}
		a.CancelRequested = true
		a.CancelMessageID = e.MessageID
		if err = saveAttempt(ctx, tx, a); err != nil {
			return r, false, err
		}
	case "turn.cancel":
		if !internal {
			return r, false, wireError(403, "coordinator_only")
		}
	default:
		return r, false, wireError(400, "unsupported_type")
	}
	r, err = s.enqueue(ctx, tx, e)
	if err == nil {
		err = s.audit(ctx, tx, p, e, "stored", e.Type)
	}
	return r, false, err
}
func (s *Store) enqueue(ctx context.Context, tx *sql.Tx, e Envelope) (Receipt, error) {
	var r Receipt
	lane := "normal"
	special := e.Type == "task.result" || e.Type == "task.cancel" || e.Type == "turn.cancel"
	if e.Type == "task.cancel" || e.Type == "turn.cancel" {
		lane = "control"
	}
	canon, err := canonical(mustJSON(e))
	if err != nil {
		return r, err
	}
	var count, bytes int
	if err = tx.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(bytes),0) FROM mailbox_delivery WHERE agent_id=? AND acked=0 AND superseded=0 AND pending_notification=0", e.ToAgentID).Scan(&count, &bytes); err != nil {
		return r, err
	}
	pending := 0
	if count >= s.mailboxLimit || bytes+len(canon) > s.mailboxBytes {
		if !special {
			return r, wireError(429, "mailbox_full")
		}
		// Reserved space permits terminal/control; beyond it notification remains durably queued
		// with its final sequence, preserving normal-lane ordering without losing the outcome.
		if count >= s.mailboxLimit+s.mailboxReserve || bytes+len(canon) > s.mailboxBytes+s.mailboxReserve*MaxEnvelopeBytes {
			pending = 1
		}
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO mailbox_counters(agent_id,seq) VALUES(?,1) ON CONFLICT(agent_id) DO UPDATE SET seq=seq+1", e.ToAgentID); err != nil {
		return r, err
	}
	r = Receipt{MessageID: e.MessageID, Receipt: "stored", ReceivedAt: s.stamp()}
	if err = tx.QueryRowContext(ctx, "SELECT seq FROM mailbox_counters WHERE agent_id=?", e.ToAgentID).Scan(&r.MailboxSeq); err != nil {
		return r, err
	}
	res, err := tx.ExecContext(ctx, "INSERT INTO messages(sender,message_id,feature_id,kind,job_id,attempt_id,turn_id,canonical,receipt) VALUES(?,?,?,?,?,?,?,?,?)", e.FromAgentID, e.MessageID, e.FeatureID, e.Type, e.JobID, e.AttemptID, e.OwnerTurnID, canon, mustJSON(r))
	if err != nil {
		return r, err
	}
	row, err := res.LastInsertId()
	if err != nil {
		return r, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO mailbox_delivery(agent_id,seq,message_row,lane,pending_notification,bytes) VALUES(?,?,?,?,?,?)", e.ToAgentID, r.MailboxSeq, row, lane, pending, len(canon))
	return r, err
}
func (s *Store) Ingest(ctx context.Context, featureID string, input InputPayload) (Receipt, bool, error) {
	var r Receipt
	duplicate := false
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		f, err := feature(ctx, tx, featureID)
		if err != nil {
			return err
		}
		if !allowedActor(f, input.Source.ActorID) || input.Source.ChannelID != f.ChannelID || input.Source.ThreadTS != f.ThreadTS {
			return wireError(403, "untrusted_input")
		}
		if f.Stopped {
			return wireError(409, "feature_stopped")
		}
		canonicalInput, _ := canonical(mustJSON(input))
		var previous, raw []byte
		err = tx.QueryRowContext(ctx, "SELECT payload,receipt FROM ingress WHERE feature_id=? AND event_id=?", featureID, input.Source.EventID).Scan(&previous, &raw)
		if err == nil {
			if string(previous) != string(canonicalInput) {
				return wireError(409, "idempotency_conflict")
			}
			duplicate = true
			return json.Unmarshal(raw, &r)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		e := Envelope{ProtocolVersion: 1, MessageID: NewID(), Type: "agent.input", TenantID: f.TenantID, ProjectID: f.ProjectID, FeatureID: f.FeatureID, FromAgentID: "coordinator", ToAgentID: f.OwnerAgentID, SentAt: s.stamp(), Payload: mustJSON(input)}
		if err = validateEnvelope(e); err != nil {
			return err
		}
		r, _, err = s.applyMessage(ctx, tx, Principal{AgentID: "coordinator"}, e, true)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO ingress VALUES(?,?,?,?)", featureID, input.Source.EventID, canonicalInput, mustJSON(r))
		return err
	})
	return r, duplicate, err
}
func (s *Store) mailbox(ctx context.Context, p Principal, lane string, limit int) (MailboxResponse, error) {
	out := MailboxResponse{Messages: []Delivery{}}
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if err := bound(ctx, tx, p); err != nil {
			return err
		}
		// A pending notification is promoted only when earlier messages no longer consume capacity.
		var pending, count, bytes int
		if err := tx.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(bytes),0) FROM mailbox_delivery WHERE agent_id=? AND acked=0 AND superseded=0 AND pending_notification=0", p.AgentID).Scan(&count, &bytes); err != nil {
			return err
		}
		if count < s.mailboxLimit && bytes < s.mailboxBytes {
			if _, err := tx.ExecContext(ctx, "UPDATE mailbox_delivery SET pending_notification=0 WHERE agent_id=? AND seq=(SELECT min(seq) FROM mailbox_delivery WHERE agent_id=? AND pending_notification=1 AND acked=0 AND superseded=0)", p.AgentID, p.AgentID); err != nil {
				return err
			}
		}
		rows, err := tx.QueryContext(ctx, "SELECT m.canonical,m.receipt,d.pending_notification FROM mailbox_delivery d JOIN messages m ON m.id=d.message_row WHERE d.agent_id=? AND d.lane=? AND d.acked=0 AND d.superseded=0 ORDER BY d.seq LIMIT ?", p.AgentID, lane, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b, rr []byte
			if err = rows.Scan(&b, &rr, &pending); err != nil {
				return err
			}
			// Control is delivered independently even when normal notifications exhaust
			// the quota. Its durable sequence already exists; polling allocates no state.
			if pending != 0 && lane == "normal" {
				break
			}
			var e Envelope
			var r Receipt
			if err = json.Unmarshal(b, &e); err != nil {
				return err
			}
			if err = json.Unmarshal(rr, &r); err != nil {
				return err
			}
			out.Messages = append(out.Messages, Delivery{e, r.MailboxSeq, r.ReceivedAt})
		}
		return rows.Err()
	})
	return out, err
}
func (s *Store) ack(ctx context.Context, p Principal, r AckRequest) (AckResponse, error) {
	out := AckResponse{Acked: r.MailboxSeqs}
	if len(r.MailboxSeqs) == 0 || len(r.MailboxSeqs) > 20 {
		return out, wireError(400, "invalid_ack")
	}
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if err := bound(ctx, tx, p); err != nil {
			return err
		}
		for _, seq := range r.MailboxSeqs {
			var found int
			if seq < 1 {
				return wireError(400, "invalid_ack")
			}
			if err := tx.QueryRowContext(ctx, "SELECT 1 FROM mailbox_delivery WHERE agent_id=? AND seq=?", p.AgentID, seq).Scan(&found); err != nil {
				return wireError(404, "not_found")
			}
		}
		for _, seq := range r.MailboxSeqs {
			if _, err := tx.ExecContext(ctx, "UPDATE mailbox_delivery SET acked=1 WHERE agent_id=? AND seq=?", p.AgentID, seq); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}
func cause(id string) *string { return &id }
func (s *Store) synthetic(ctx context.Context, tx *sql.Tx, a attemptRecord, code string) error {
	f, err := feature(ctx, tx, a.FeatureID)
	if err != nil {
		return err
	}
	result := ResultPayload{Outcome: "failed", Summary: fmt.Sprintf("Task could not start: %s", code), Evidence: []Evidence{}, Error: &TaskError{Code: code, Message: code, Retryable: true}, Origin: "coordinator"}
	e := Envelope{ProtocolVersion: 1, MessageID: NewID(), Type: "task.result", TenantID: f.TenantID, ProjectID: f.ProjectID, FeatureID: f.FeatureID, FromAgentID: "coordinator", ToAgentID: f.OwnerAgentID, JobID: a.JobID, AttemptID: a.AttemptID, CausationID: cause(a.DispatchMessageID), SentAt: s.stamp(), Payload: mustJSON(result)}
	_, _, err = s.applyMessage(ctx, tx, Principal{AgentID: "coordinator"}, e, true)
	return err
}
