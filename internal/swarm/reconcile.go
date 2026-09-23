package swarm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

func ReconcileOffline(ctx context.Context, path string, cfg Config, r ReconcileRequest) error {
	if !uuid(r.OldInstanceID) || !uuid(r.NewInstanceID) || r.NewInstanceID == r.OldInstanceID || !safeText(r.Reason, 4096) || !safeText(r.Actor, 256) || !safeText(r.ContainerID, 256) || !r.ContainerStopped || r.CheckedAt.IsZero() || time.Since(r.CheckedAt) > 5*time.Minute || r.CheckedAt.After(time.Now().Add(5*time.Second)) {
		return wireError(400, "cessation_evidence_required")
	}
	// Reconciliation is an offline operator action. The service will establish
	// its Slack recovery barrier on the subsequent serve startup; doing so here
	// would block other offline operations in the same stopped window.
	s, err := openStore(ctx, path, cfg, false)
	if err != nil {
		return err
	}
	defer s.Close()
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var boundID string
		if err := tx.QueryRowContext(ctx, "SELECT instance_id FROM agents WHERE agent_id=?", r.AgentID).Scan(&boundID); err != nil {
			return wireError(404, "agent_not_found")
		}
		if boundID != r.OldInstanceID {
			return wireError(409, "old_instance_mismatch")
		}
		attempts, turns, err := active(ctx, tx)
		if err != nil {
			return err
		}
		for _, a := range attempts {
			if a.AssignedAgentID != r.AgentID || a.InstanceID != r.OldInstanceID {
				continue
			}
			f, err := feature(ctx, tx, a.FeatureID)
			if err != nil {
				return err
			}
			result := ResultPayload{Outcome: "interrupted", Summary: "Previous runner was stopped; unknown execution outcome requires review", Evidence: []Evidence{{Kind: "ref", Label: "Stopped container", ContentOrRef: "container:" + r.ContainerID}}, Error: &TaskError{Code: "operator_reconciled", Message: r.Reason, Retryable: false}, Origin: "coordinator"}
			e := Envelope{ProtocolVersion: s.wireVersion(), MessageID: NewID(), Type: "task.result", TenantID: f.TenantID, ProjectID: f.ProjectID, FeatureID: f.FeatureID, FromAgentID: "coordinator", ToAgentID: f.OwnerAgentID, JobID: a.JobID, AttemptID: a.AttemptID, CausationID: cause(a.DispatchMessageID), SentAt: s.stamp(), Payload: mustJSON(result)}
			if _, _, err = s.applyMessage(ctx, tx, Principal{AgentID: "coordinator"}, e, true); err != nil {
				return err
			}
			updated, err := attempt(ctx, tx, a.AttemptID)
			if err != nil {
				return err
			}
			updated.Reconciled = true
			if err = saveAttempt(ctx, tx, updated); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "UPDATE mailbox_delivery SET superseded=1 WHERE agent_id=? AND message_row IN (SELECT id FROM messages WHERE attempt_id=?)", r.AgentID, a.AttemptID); err != nil {
				return err
			}
		}
		for _, t := range turns {
			if t.AgentID != r.AgentID || t.InstanceID != r.OldInstanceID {
				continue
			}
			t.State = "interrupted"
			t.Error = &TaskError{Code: "operator_reconciled", Message: r.Reason, Retryable: false}
			if err = saveTurn(ctx, tx, t); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "UPDATE mailbox_delivery SET superseded=1 WHERE agent_id=? AND (seq=? OR message_row IN (SELECT id FROM messages WHERE turn_id=?))", r.AgentID, t.InputMailboxSeq, t.TurnID); err != nil {
				return err
			}
			if err = s.audit(ctx, tx, Principal{r.AgentID, r.OldInstanceID}, Envelope{FeatureID: t.FeatureID, OwnerTurnID: t.TurnID}, "owner_interrupted", "operator_reconciled"); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, "UPDATE agents SET instance_id=?,heartbeat=NULL WHERE agent_id=?", r.NewInstanceID, r.AgentID); err != nil {
			return err
		}
		// Evidence is persisted outside prompts and the public agent API.
		if _, err = tx.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS reconciliation (seq INTEGER PRIMARY KEY AUTOINCREMENT,agent_id TEXT NOT NULL,record BLOB NOT NULL)"); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO reconciliation(agent_id,record) VALUES(?,?)", r.AgentID, mustJSON(r)); err != nil {
			return err
		}
		return s.audit(ctx, tx, Principal{r.AgentID, r.NewInstanceID}, Envelope{}, "operator_reconciled", "")
	})
}

// QueueSlackNotice is for trusted status/command responses; model replies use owner finish.
func (s *Store) QueueSlackNotice(ctx context.Context, featureID, eventID, text string) error {
	if !safeText(eventID, 256) || !safeText(text, 16*1024) {
		return wireError(400, "invalid_notice")
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		f, err := feature(ctx, tx, featureID)
		if err != nil {
			return err
		}
		key := Digest([]byte(featureID + "\x00" + eventID))
		id := key[:8] + "-" + key[8:12] + "-4" + key[13:16] + "-a" + key[17:20] + "-" + key[20:32]
		var b []byte
		err = tx.QueryRowContext(ctx, "SELECT data FROM slack_outbox WHERE id=?", id).Scan(&b)
		if err == nil {
			var old SlackDelivery
			if err = json.Unmarshal(b, &old); err != nil {
				return err
			}
			if old.Text != text {
				return wireError(409, "idempotency_conflict")
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		d := SlackDelivery{ID: id, FeatureID: featureID, ChannelID: f.ChannelID, ThreadTS: f.ThreadTS, Text: text, Status: "queued"}
		_, err = tx.ExecContext(ctx, "INSERT INTO slack_outbox VALUES(?,?,NULL,?,?)", id, featureID, d.Status, mustJSON(d))
		return err
	})
}
