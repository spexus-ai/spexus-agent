package swarm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

func (s *Store) startOwner(ctx context.Context, p Principal, r OwnerStartRequest) (OwnerStartReceipt, bool, error) {
	out := OwnerStartReceipt{r.TurnID, "running", r.InputMailboxSeq}
	duplicate := false
	if !uuid(r.TurnID) || !uuid(r.FeatureID) || r.InputMailboxSeq < 1 {
		return out, false, wireError(400, "invalid_owner_start")
	}
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if err := bound(ctx, tx, p); err != nil {
			return err
		}
		f, err := feature(ctx, tx, r.FeatureID)
		if err != nil {
			return err
		}
		if f.OwnerAgentID != p.AgentID {
			return wireError(403, "foreign_owner")
		}
		t, err := turn(ctx, tx, r.TurnID)
		if err == nil {
			if t.AgentID != p.AgentID || t.InstanceID != p.InstanceID || t.Start != r {
				return wireError(409, "idempotency_conflict")
			}
			duplicate = true
			return nil
		}
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != 404 {
			return err
		}
		if f.Stopped {
			return wireError(409, "feature_stopped")
		}
		if err = barrier(ctx, tx, f.FeatureID); err != nil {
			return err
		}
		var raw []byte
		var superseded int
		err = tx.QueryRowContext(ctx, "SELECT m.canonical,d.superseded FROM mailbox_delivery d JOIN messages m ON m.id=d.message_row WHERE d.agent_id=? AND d.seq=?", p.AgentID, r.InputMailboxSeq).Scan(&raw, &superseded)
		if errors.Is(err, sql.ErrNoRows) {
			return wireError(404, "not_found")
		}
		if err != nil {
			return err
		}
		var e Envelope
		if err = json.Unmarshal(raw, &e); err != nil {
			return err
		}
		if e.FeatureID != r.FeatureID || superseded != 0 || e.Type != "agent.input" && e.Type != "task.result" && e.Type != "human.decision" {
			return wireError(409, "invalid_trigger")
		}
		var count int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM owner_turns WHERE agent_id=? AND state='running'", p.AgentID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return wireError(409, "owner_busy")
		}
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM owner_turns WHERE agent_id=? AND input_seq=?", p.AgentID, r.InputMailboxSeq).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return wireError(409, "input_already_started")
		}
		t = turnRecord{OwnerTurn: OwnerTurn{TurnID: r.TurnID, FeatureID: r.FeatureID, InputMailboxSeq: r.InputMailboxSeq, State: "running", ReplyStatus: "none"}, AgentID: p.AgentID, InstanceID: p.InstanceID, StartedAt: s.stamp(), Start: r}
		if _, err = tx.ExecContext(ctx, "INSERT INTO owner_turns VALUES(?,?,?,?,?,?)", r.TurnID, r.FeatureID, p.AgentID, r.InputMailboxSeq, t.State, mustJSON(t)); err != nil {
			return err
		}
		e.OwnerTurnID = r.TurnID
		return s.audit(ctx, tx, p, e, "owner_started", "")
	})
	if err != nil {
		s.reject(ctx, p, Envelope{OwnerTurnID: r.TurnID, FeatureID: r.FeatureID}, err)
	}
	return out, duplicate, err
}
func (s *Store) ownerTurn(ctx context.Context, p Principal, id string) (OwnerTurn, error) {
	var out OwnerTurn
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if err := bound(ctx, tx, p); err != nil {
			return err
		}
		t, err := turn(ctx, tx, id)
		if err != nil {
			return err
		}
		if t.AgentID != p.AgentID || t.InstanceID != p.InstanceID {
			return wireError(404, "not_found")
		}
		out = t.OwnerTurn
		return nil
	})
	return out, err
}
func (s *Store) finishOwner(ctx context.Context, p Principal, id string, r OwnerFinishRequest) (OwnerFinishReceipt, bool, error) {
	var out OwnerFinishReceipt
	duplicate := false
	if !uuid(id) || !terminal(r.Outcome) || len(r.Reply) > 16*1024 || r.Actions == nil || len(r.Actions) > 8 {
		return out, false, wireError(400, "invalid_owner_finish")
	}
	if err := validateError(r.Error); err != nil {
		return out, false, err
	}
	if r.Outcome == "succeeded" && r.Error != nil || r.Outcome != "succeeded" && r.Error == nil {
		return out, false, wireError(400, "invalid_error")
	}
	if len(r.Observation) > MaxEnvelopeBytes {
		return out, false, wireError(400, "observation_too_large")
	}
	if len(r.Observation) > 0 && string(r.Observation) != "null" {
		var observation map[string]json.RawMessage
		if err := json.Unmarshal(r.Observation, &observation); err != nil || observation == nil {
			return out, false, wireError(400, "invalid_observation")
		}
	}

	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if err := bound(ctx, tx, p); err != nil {
			return err
		}
		t, err := turn(ctx, tx, id)
		if err != nil {
			return err
		}
		if t.AgentID != p.AgentID || t.InstanceID != p.InstanceID {
			return wireError(403, "foreign_owner")
		}
		if t.Finish != nil {
			old, _ := canonical(mustJSON(t.Finish))
			fresh, _ := canonical(mustJSON(r))
			if string(old) != string(fresh) {
				return wireError(409, "idempotency_conflict")
			}
			receiptStatus := "none"
			if t.Finish.Reply != "" {
				receiptStatus = "queued"
			}
			out = OwnerFinishReceipt{t.TurnID, t.State, receiptStatus}
			duplicate = true
			return nil
		}
		if t.State != "running" {
			return wireError(409, "already_terminal")
		}
		if t.CancelRequested {
			if r.Outcome == "succeeded" || r.Outcome == "failed" {
				return wireError(409, "cancel_requested")
			}
			if r.Outcome == "interrupted" && (len(r.Observation) == 0 || string(r.Observation) == "null") {
				return wireError(400, "observation_required")
			}
		} else if r.Outcome == "cancelled" {
			return wireError(409, "cancel_not_requested")
		}
		seen := map[string]bool{}
		for _, a := range r.Actions {
			if !uuid(a.MessageID) || seen[a.MessageID] {
				return wireError(400, "invalid_action_receipt")
			}
			seen[a.MessageID] = true
			switch a.Status {
			case "stored":
				if a.ErrorCode != nil {
					return wireError(400, "invalid_action_receipt")
				}
				var fid, tid string
				err = tx.QueryRowContext(ctx, `SELECT feature_id,turn_id FROM messages WHERE sender=? AND message_id=?`, p.AgentID, a.MessageID).Scan(&fid, &tid)
				// Business duplicates are aliases and carry the submitted envelope's turn.
				if errors.Is(err, sql.ErrNoRows) {
					var b []byte
					err = tx.QueryRowContext(ctx, "SELECT canonical FROM message_aliases WHERE sender=? AND message_id=?", p.AgentID, a.MessageID).Scan(&b)
					if err == nil {
						var e Envelope
						err = json.Unmarshal(b, &e)
						fid = e.FeatureID
						tid = e.OwnerTurnID
					}
				}
				if err != nil || fid != t.FeatureID || tid != id {
					return wireError(409, "unproven_action")
				}
			case "rejected":
				if a.ErrorCode == nil || !codePattern.MatchString(*a.ErrorCode) {
					return wireError(400, "invalid_action_receipt")
				}
				var count int
				if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM audit WHERE agent_id=? AND instance_id=? AND message_id=? AND turn_id=? AND feature_id=? AND event='rejected' AND code=?", p.AgentID, p.InstanceID, a.MessageID, id, t.FeatureID, *a.ErrorCode).Scan(&count); err != nil {
					return err
				}
				if count == 0 {
					return wireError(409, "unproven_rejection")
				}
			default:
				return wireError(400, "invalid_action_receipt")
			}
		}
		// Every stored action in this turn must be represented; finish cannot conceal partial success.
		rows, err := tx.QueryContext(ctx, "SELECT message_id FROM messages WHERE sender=? AND turn_id=?", p.AgentID, id)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var mid string
			if err = rows.Scan(&mid); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, mid)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, mid := range ids {
			if !seen[mid] {
				return wireError(409, "missing_action_receipt")
			}
		}
		t.State = r.Outcome
		t.Error = r.Error
		t.Finish = &r
		t.ReplyStatus = "none"
		if r.Reply != "" {
			f, err := feature(ctx, tx, t.FeatureID)
			if err != nil {
				return err
			}
			d := SlackDelivery{ID: NewID(), FeatureID: t.FeatureID, TurnID: id, ChannelID: f.ChannelID, ThreadTS: f.ThreadTS, Text: r.Reply, Status: "queued"}
			if _, err = tx.ExecContext(ctx, "INSERT INTO slack_outbox VALUES(?,?,?,?,?)", d.ID, d.FeatureID, id, d.Status, mustJSON(d)); err != nil {
				return err
			}
			t.ReplyStatus = "queued"
		}
		if err = saveTurn(ctx, tx, t); err != nil {
			return err
		}
		out = OwnerFinishReceipt{id, t.State, t.ReplyStatus}
		return s.audit(ctx, tx, p, Envelope{FeatureID: t.FeatureID, OwnerTurnID: id}, "owner_finished", r.Outcome)
	})
	if err != nil {
		s.reject(ctx, p, Envelope{OwnerTurnID: id}, err)
	}
	return out, duplicate, err
}
func (s *Store) ClaimSlack(ctx context.Context) (*SlackDelivery, error) {
	var out *SlackDelivery
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		var b []byte
		err := tx.QueryRowContext(ctx, "SELECT data FROM slack_outbox WHERE status='queued' ORDER BY rowid LIMIT 1").Scan(&b)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var d SlackDelivery
		if err = json.Unmarshal(b, &d); err != nil {
			return err
		}
		d.Status = "sending"
		if d.TurnID != "" {
			t, err := turn(ctx, tx, d.TurnID)
			if err != nil {
				return err
			}
			t.ReplyStatus = "sending"
			if err = saveTurn(ctx, tx, t); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, "UPDATE slack_outbox SET status=?,data=? WHERE id=?", d.Status, mustJSON(d), d.ID); err != nil {
			return err
		}
		out = &d
		return nil
	})
	return out, err
}
func (s *Store) SettleSlack(ctx context.Context, id, status, slackTS string) error {
	if status != "sent" && status != "delivery_unknown" && status != "failed" && status != "queued" || status == "sent" && slackTS == "" {
		return wireError(400, "invalid_slack_settlement")
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var b []byte
		if err := tx.QueryRowContext(ctx, "SELECT data FROM slack_outbox WHERE id=?", id).Scan(&b); err != nil {
			return err
		}
		var d SlackDelivery
		if err := json.Unmarshal(b, &d); err != nil {
			return err
		}
		if d.Status == "sent" {
			if status == "sent" && d.SlackTS == slackTS {
				return nil
			}
			return wireError(409, "already_sent")
		}
		if d.Status != "sending" && d.Status != "delivery_unknown" {
			return wireError(409, "invalid_slack_transition")
		}
		d.Status = status
		d.SlackTS = slackTS
		if _, err := tx.ExecContext(ctx, "UPDATE slack_outbox SET status=?,data=? WHERE id=?", status, mustJSON(d), id); err != nil {
			return err
		}
		if d.TurnID != "" {
			t, err := turn(ctx, tx, d.TurnID)
			if err != nil {
				return err
			}
			t.ReplyStatus = status
			if err = saveTurn(ctx, tx, t); err != nil {
				return err
			}
		}
		return s.audit(ctx, tx, Principal{AgentID: "coordinator"}, Envelope{FeatureID: d.FeatureID, OwnerTurnID: d.TurnID}, "slack_delivery", status)
	})
}
func (s *Store) cancelAttempt(ctx context.Context, tx *sql.Tx, f Feature, a attemptRecord, actor, reason string) error {
	if terminal(a.State) || a.CancelRequested {
		return nil
	}
	e := Envelope{ProtocolVersion: s.wireVersion(), MessageID: NewID(), Type: "task.cancel", TenantID: f.TenantID, ProjectID: f.ProjectID, FeatureID: f.FeatureID, FromAgentID: "coordinator", ToAgentID: a.AssignedAgentID, JobID: a.JobID, AttemptID: a.AttemptID, CausationID: cause(a.DispatchMessageID), SentAt: s.stamp(), Payload: mustJSON(CancelPayload{reason, actor})}
	_, _, err := s.applyMessage(ctx, tx, Principal{AgentID: "coordinator"}, e, true)
	return err
}
func (s *Store) cancelTurn(ctx context.Context, tx *sql.Tx, f Feature, t turnRecord, actor, reason string) error {
	if terminal(t.State) || t.CancelRequested {
		return nil
	}
	t.CancelRequested = true
	if err := saveTurn(ctx, tx, t); err != nil {
		return err
	}
	e := Envelope{ProtocolVersion: s.wireVersion(), MessageID: NewID(), Type: "turn.cancel", TenantID: f.TenantID, ProjectID: f.ProjectID, FeatureID: f.FeatureID, FromAgentID: "coordinator", ToAgentID: f.OwnerAgentID, OwnerTurnID: t.TurnID, SentAt: s.stamp(), Payload: mustJSON(CancelPayload{reason, actor})}
	_, _, err := s.applyMessage(ctx, tx, Principal{AgentID: "coordinator"}, e, true)
	return err
}

// InterruptOwnerTurn cancels only the currently running owner turn. The
// feature, worker attempts, dependencies, and queued inputs remain available
// for the next turn. A repeated urgent Slack delivery is harmless because
// cancelTurn does not enqueue a second control for an already cancelled turn.
func (s *Store) InterruptOwnerTurn(ctx context.Context, id, actor, reason string) error {
	if !safeText(reason, 4096) {
		return wireError(400, "invalid_reason")
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		f, err := feature(ctx, tx, id)
		if err != nil {
			return err
		}
		if !allowedActor(f, actor) {
			return wireError(403, "untrusted_actor")
		}
		_, turns, err := active(ctx, tx)
		if err != nil {
			return err
		}
		for _, t := range turns {
			if t.FeatureID == id {
				return s.cancelTurn(ctx, tx, f, t, actor, reason)
			}
		}
		return nil
	})
}

func (s *Store) StopFeature(ctx context.Context, id, actor, reason string) error {
	if !safeText(reason, 4096) {
		return wireError(400, "invalid_reason")
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		f, err := feature(ctx, tx, id)
		if err != nil {
			return err
		}
		if !allowedActor(f, actor) {
			return wireError(403, "untrusted_actor")
		}
		f.Stopped = true
		if _, err = tx.ExecContext(ctx, "UPDATE features SET data=? WHERE id=?", mustJSON(f), id); err != nil {
			return err
		}
		if err = s.cancelDependencies(ctx, tx, id, actor, reason); err != nil {
			return err
		}
		attempts, turns, err := active(ctx, tx)
		if err != nil {
			return err
		}
		for _, a := range attempts {
			if a.FeatureID == id {
				if err = s.cancelAttempt(ctx, tx, f, a, actor, reason); err != nil {
					return err
				}
			}
		}
		for _, t := range turns {
			if t.FeatureID == id {
				if err = s.cancelTurn(ctx, tx, f, t, actor, reason); err != nil {
					return err
				}
			}
		}
		return s.audit(ctx, tx, Principal{AgentID: actor}, Envelope{FeatureID: id}, "feature_stopped", "")
	})
}
func (s *Store) ContinueFeature(ctx context.Context, id, actor string) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		f, err := feature(ctx, tx, id)
		if err != nil {
			return err
		}
		if !allowedActor(f, actor) {
			return wireError(403, "untrusted_actor")
		}
		attempts, turns, err := active(ctx, tx)
		if err != nil {
			return err
		}
		for _, a := range attempts {
			if a.FeatureID == id {
				return wireError(409, "unresolved_work")
			}
		}
		for _, t := range turns {
			if t.FeatureID == id {
				return wireError(409, "unresolved_work")
			}
		}
		// Old pending inputs cannot become implicit replay after a human continue.
		if _, err = tx.ExecContext(ctx, "UPDATE mailbox_delivery SET superseded=1 WHERE message_row IN (SELECT id FROM messages WHERE feature_id=? AND kind IN ('agent.input','task.result','human.decision')) AND acked=0", id); err != nil {
			return err
		}
		f.Stopped = false
		if _, err = tx.ExecContext(ctx, "UPDATE features SET data=? WHERE id=?", mustJSON(f), id); err != nil {
			return err
		}
		return s.audit(ctx, tx, Principal{AgentID: actor}, Envelope{FeatureID: id}, "feature_continued", "")
	})
}
func active(ctx context.Context, tx *sql.Tx) ([]attemptRecord, []turnRecord, error) {
	var attempts []attemptRecord
	var turns []turnRecord
	rows, err := tx.QueryContext(ctx, "SELECT data FROM attempts WHERE state NOT IN ('succeeded','failed','cancelled','interrupted','blocked')")
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			rows.Close()
			return nil, nil, err
		}
		var a attemptRecord
		if err = json.Unmarshal(b, &a); err != nil {
			rows.Close()
			return nil, nil, err
		}
		attempts = append(attempts, a)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, nil, err
	}
	rows.Close()
	rows, err = tx.QueryContext(ctx, "SELECT data FROM owner_turns WHERE state='running'")
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			rows.Close()
			return nil, nil, err
		}
		var t turnRecord
		if err = json.Unmarshal(b, &t); err != nil {
			rows.Close()
			return nil, nil, err
		}
		turns = append(turns, t)
	}
	err = rows.Err()
	rows.Close()
	return attempts, turns, err
}
func (s *Store) Sweep(ctx context.Context) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		attempts, turns, err := active(ctx, tx)
		if err != nil {
			return err
		}
		for _, a := range attempts {
			switch a.State {
			case "queued":
				deadline, _ := time.Parse(time.RFC3339Nano, a.Dispatch.AcceptBy)
				if !s.now().Before(deadline) {
					if err = s.synthetic(ctx, tx, a, "recipient_unavailable"); err != nil {
						return err
					}
				}
			case "accepted":
				deadline, _ := time.Parse(time.RFC3339Nano, a.AcceptedAt)
				if !s.now().Before(deadline.Add(30 * time.Second)) {
					if err = s.synthetic(ctx, tx, a, "start_deadline_expired"); err != nil {
						return err
					}
				}
			case "running":
				started, _ := time.Parse(time.RFC3339Nano, *a.StartedAt)
				if !s.now().Before(started.Add(time.Duration(a.Dispatch.RunTimeoutSeconds) * time.Second)) {
					f, err := feature(ctx, tx, a.FeatureID)
					if err != nil {
						return err
					}
					if err = s.cancelAttempt(ctx, tx, f, a, "coordinator", "run_timeout"); err != nil {
						return err
					}
				}
			}
		}
		for _, t := range turns {
			started, _ := time.Parse(time.RFC3339Nano, t.StartedAt)
			if !s.now().Before(started.Add(600 * time.Second)) {
				f, err := feature(ctx, tx, t.FeatureID)
				if err != nil {
					return err
				}
				if err = s.cancelTurn(ctx, tx, f, t, "coordinator", "run_timeout"); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
