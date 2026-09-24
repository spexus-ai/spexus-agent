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
		isUrgent := urgentEnvelope(e)
		var urgentPending int
		err = tx.QueryRowContext(ctx, `SELECT count(*) FROM mailbox_delivery d JOIN messages m ON m.id=d.message_row
				WHERE d.agent_id=? AND m.feature_id=? AND d.superseded=0 AND m.kind='agent.input'
				AND substr(json_extract(m.canonical,'$.payload.text'),1,1)='!'
				AND (?=0 OR d.seq<?)
				AND NOT EXISTS (SELECT 1 FROM owner_turns t WHERE t.agent_id=d.agent_id AND t.input_seq=d.seq)`, p.AgentID, r.FeatureID, boolToInt(isUrgent), r.InputMailboxSeq).Scan(&urgentPending)
		if err != nil {
			return err
		}
		if urgentPending != 0 {
			return wireError(409, "urgent_input_pending")
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

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func urgentEnvelope(e Envelope) bool {
	var input InputPayload
	return e.Type == "agent.input" && json.Unmarshal(e.Payload, &input) == nil && urgentInput(input)
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
			out = OwnerFinishReceipt{t.TurnID, t.State, t.ReplyStatus}
			duplicate = true
			return nil
		}
		if t.State != "running" {
			return wireError(409, "already_terminal")
		}
		var triggerKind, triggerText string
		if err = tx.QueryRowContext(ctx, `SELECT m.kind,coalesce(json_extract(m.canonical,'$.payload.text'),'')
			FROM mailbox_delivery d JOIN messages m ON m.id=d.message_row
			WHERE d.agent_id=? AND d.seq=?`, t.AgentID, t.InputMailboxSeq).Scan(&triggerKind, &triggerText); err != nil {
			return err
		}
		// A stop is already acknowledged by the deterministic Slack notice.
		// It reaches the owner only after !continue, so its generated reply is
		// stale even if the model sees it before the continue input.
		stopControl := triggerKind == "agent.input" && triggerText == "!stop"
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
		// Keep the requested reply in the immutable finish for replay, but only
		// publish it when this transaction proves the review is truly final.
		replyReady := true
		if s.wireVersion() == 2 && r.Outcome == "succeeded" && r.Reply != "" && (len(r.Actions) != 0 || triggerKind == "task.result") {
			ready, err := s.finalReviewReady(ctx, tx, t, r.Actions)
			if err != nil {
				return err
			}
			replyReady = ready != nil
		}
		t.State = r.Outcome
		t.Error = r.Error
		t.Finish = &r
		t.ReplyStatus = "none"
		if stopControl && r.Reply != "" {
			t.ReplyStatus = "suppressed"
		}
		if r.Reply != "" && replyReady && !stopControl {
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
		if r.Outcome == "succeeded" && !stopControl && (r.Reply == "" || !replyReady) {
			if err = s.queueFinalSummary(ctx, tx, t, r.Actions); err != nil {
				return err
			}
		}
		out = OwnerFinishReceipt{id, t.State, t.ReplyStatus}
		if !replyReady {
			if err = s.audit(ctx, tx, p, Envelope{FeatureID: t.FeatureID, OwnerTurnID: id}, "owner_reply_suppressed", "final_review_not_ready"); err != nil {
				return err
			}
		}
		return s.audit(ctx, tx, p, Envelope{FeatureID: t.FeatureID, OwnerTurnID: id}, "owner_finished", r.Outcome)
	})
	if err != nil {
		s.reject(ctx, p, Envelope{OwnerTurnID: id}, err)
	}
	return out, duplicate, err
}

type finalReviewState struct {
	row    int64
	size   int64
	result Envelope
}

// A final review is ready only after the exact triggering result was accepted,
// every job is accepted, no human question remains, and no newer owner input
// is waiting. The check runs in the same transaction as owner finish.
func (s *Store) finalReviewReady(ctx context.Context, tx *sql.Tx, t turnRecord, actions []ActionReceipt) (*finalReviewState, error) {
	if s.wireVersion() != 2 || len(actions) > 1 {
		return nil, nil
	}
	var row, size int64
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT m.id,d.bytes,m.canonical FROM mailbox_delivery d JOIN messages m ON m.id=d.message_row WHERE d.agent_id=? AND d.seq=? AND m.kind='task.result'`, t.AgentID, t.InputMailboxSeq).Scan(&row, &size, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result Envelope
	if err = json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	if result.FeatureID != t.FeatureID {
		return nil, wireError(409, "foreign_result")
	}
	if len(actions) == 0 {
		// A summary-only turn reuses an already reviewed task.result. The
		// coordinator must prove that review from durable attempt state; an
		// action-free reply to a fresh result is not a final answer.
		var attemptRaw []byte
		err = tx.QueryRowContext(ctx, `SELECT data FROM attempts WHERE id=? AND job_id=?`, result.AttemptID, result.JobID).Scan(&attemptRaw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		var attempt Attempt
		if err = json.Unmarshal(attemptRaw, &attempt); err != nil {
			return nil, err
		}
		if attempt.ResultMessageID != result.MessageID || attempt.Review != "accepted" {
			return nil, nil
		}
	} else {
		if actions[0].Status != "stored" {
			return nil, nil
		}
		var reviewRaw []byte
		err = tx.QueryRowContext(ctx, `SELECT canonical FROM messages WHERE sender=? AND message_id=? AND turn_id=? AND kind='task.review' AND attempt_id=?`, t.AgentID, actions[0].MessageID, t.TurnID, result.AttemptID).Scan(&reviewRaw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		var review Envelope
		if err = json.Unmarshal(reviewRaw, &review); err != nil {
			return nil, err
		}
		var payload ReviewPayload
		if err = json.Unmarshal(review.Payload, &payload); err != nil {
			return nil, err
		}
		if review.JobID != result.JobID || review.CausationID == nil || *review.CausationID != result.MessageID || payload.ResultMessageID != result.MessageID || payload.Verdict != "accepted" {
			return nil, nil
		}
	}
	var total, unfinished, openQuestions int
	if err = tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(CASE
		WHEN a.state IN ('succeeded','failed') AND json_extract(a.data,'$.review')='accepted' THEN 0
		WHEN a.state='blocked' AND EXISTS (SELECT 1 FROM dependencies d WHERE d.job_id=j.id AND d.state IN ('denied','cancelled')) THEN 0
		ELSE 1 END),0)
		FROM jobs j JOIN attempts a ON a.id=j.current_attempt_id WHERE j.feature_id=?`, t.FeatureID).Scan(&total, &unfinished); err != nil {
		return nil, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM human_projections h JOIN dependencies d ON d.id=h.dependency_id WHERE d.feature_id=? AND h.state='open'`, t.FeatureID).Scan(&openQuestions); err != nil {
		return nil, err
	}
	if total == 0 || unfinished != 0 || openQuestions != 0 {
		return nil, nil
	}
	f, err := feature(ctx, tx, t.FeatureID)
	if err != nil {
		return nil, err
	}
	if f.Stopped {
		return nil, nil
	}
	var newer int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM mailbox_delivery WHERE agent_id=? AND seq>? AND acked=0 AND superseded=0`, t.AgentID, t.InputMailboxSeq).Scan(&newer); err != nil {
		return nil, err
	}
	if newer != 0 {
		return nil, nil
	}
	return &finalReviewState{row: row, size: size, result: result}, nil
}

// An accepted final review with no reply reuses its immutable task.result as
// a summary-only trigger. A repeated finish cannot enqueue another summary.
func (s *Store) queueFinalSummary(ctx context.Context, tx *sql.Tx, t turnRecord, actions []ActionReceipt) error {
	ready, err := s.finalReviewReady(ctx, tx, t, actions)
	if err != nil || ready == nil {
		return err
	}
	var count, bytes int
	if err = tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(bytes),0) FROM mailbox_delivery WHERE agent_id=? AND acked=0 AND superseded=0 AND pending_notification=0`, t.AgentID).Scan(&count, &bytes); err != nil {
		return err
	}
	pending := 0
	if count >= s.mailboxLimit || bytes+int(ready.size) > s.mailboxBytes {
		pending = 1
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO mailbox_counters(agent_id,seq) VALUES(?,1) ON CONFLICT(agent_id) DO UPDATE SET seq=seq+1`, t.AgentID); err != nil {
		return err
	}
	var seq int64
	if err = tx.QueryRowContext(ctx, `SELECT seq FROM mailbox_counters WHERE agent_id=?`, t.AgentID).Scan(&seq); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO mailbox_delivery(agent_id,seq,message_row,lane,pending_notification,bytes) VALUES(?,?,?,'normal',?,?)`, t.AgentID, seq, ready.row, pending, ready.size); err != nil {
		return err
	}
	return s.audit(ctx, tx, Principal{AgentID: "coordinator"}, ready.result, "owner_summary_queued", "final_review_without_reply")
}
func (s *Store) ClaimSlack(ctx context.Context) (*SlackDelivery, error) {
	var out *SlackDelivery
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT data FROM slack_outbox WHERE status='queued' ORDER BY rowid")
		if err != nil {
			return err
		}
		var queued []SlackDelivery
		for rows.Next() {
			var b []byte
			if err = rows.Scan(&b); err != nil {
				break
			}
			var d SlackDelivery
			if err = json.Unmarshal(b, &d); err != nil {
				break
			}
			queued = append(queued, d)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		var d SlackDelivery
		found := false
		for _, candidate := range queued {
			if candidate.TurnID != "" {
				f, err := feature(ctx, tx, candidate.FeatureID)
				if err != nil {
					return err
				}
				if f.Stopped {
					candidate.Status = "suppressed"
					if _, err = tx.ExecContext(ctx, "UPDATE slack_outbox SET status='suppressed',data=? WHERE id=? AND status='queued'", mustJSON(candidate), candidate.ID); err != nil {
						return err
					}
					t, err := turn(ctx, tx, candidate.TurnID)
					if err != nil {
						return err
					}
					t.ReplyStatus = "suppressed"
					if err = saveTurn(ctx, tx, t); err != nil {
						return err
					}
					if err = s.audit(ctx, tx, Principal{AgentID: "coordinator"}, Envelope{FeatureID: candidate.FeatureID, OwnerTurnID: candidate.TurnID}, "slack_delivery", "suppressed_after_stop"); err != nil {
						return err
					}
					continue
				}
			}
			if candidate.Question != nil {
				var state string
				if err = tx.QueryRowContext(ctx, "SELECT state FROM human_projections WHERE request_id=?", candidate.ID).Scan(&state); err != nil {
					return err
				}
				if state != "open" {
					candidate.Status = "failed"
					if _, err = tx.ExecContext(ctx, "UPDATE slack_outbox SET status='failed',data=? WHERE id=? AND status='queued'", mustJSON(candidate), candidate.ID); err != nil {
						return err
					}
					continue
				}
				var active int
				if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM slack_outbox o JOIN human_projections p ON p.request_id=o.id WHERE o.feature_id=? AND o.id!=? AND o.status IN ('sent','sending','delivery_unknown') AND p.state='open'`, candidate.FeatureID, candidate.ID).Scan(&active); err != nil {
					return err
				}
				if active != 0 {
					continue
				}
			}
			d, found = candidate, true
			break
		}
		if !found {
			return nil
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
