package swarm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// LocalFixtureReceipt is an offline preview receipt. Fixture operations are
// available only through the coordinator's --local-fixture commands.
type LocalFixtureReceipt struct {
	EventID    string    `json:"event_id"`
	Status     string    `json:"status"`
	Duplicate  bool      `json:"duplicate"`
	MessageID  string    `json:"message_id,omitempty"`
	MailboxSeq int64     `json:"mailbox_seq,omitempty"`
	Released   []Receipt `json:"released,omitempty"`
}

type LocalFixtureDecisionReceipt struct {
	SourceID          string `json:"source_id"`
	RequestID         string `json:"request_id"`
	OperationID       string `json:"operation_id"`
	Duplicate         bool   `json:"duplicate"`
	BackendSyncStatus string `json:"backend_sync_status"`
	ApplicationStatus string `json:"application_status"`
}

const localFixtureSchema = `CREATE TABLE IF NOT EXISTS local_fixture_events (
	feature_id TEXT NOT NULL, event_id TEXT NOT NULL, payload BLOB NOT NULL,
	status TEXT NOT NULL, receipt BLOB,
	PRIMARY KEY(feature_id,event_id)
)`

func (s *Store) localFixtureEvent(ctx context.Context, featureID, eventID string, payload []byte) (LocalFixtureReceipt, bool, error) {
	var result LocalFixtureReceipt
	var prior, raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT payload,status,receipt FROM local_fixture_events WHERE feature_id=? AND event_id=?`, featureID, eventID).Scan(&prior, &result.Status, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return result, false, nil
	}
	if err != nil {
		return result, false, err
	}
	if string(prior) != string(payload) {
		return result, true, wireError(409, "source_conflict")
	}
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &result); err != nil {
			return result, true, err
		}
	}
	result.EventID = eventID
	result.Duplicate = true
	return result, true, nil
}

func (s *Store) saveLocalFixtureEvent(ctx context.Context, featureID, eventID string, payload []byte, receipt LocalFixtureReceipt) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO local_fixture_events(feature_id,event_id,payload,status,receipt)
		VALUES(?,?,?,?,?) ON CONFLICT(feature_id,event_id) DO UPDATE SET status=excluded.status,receipt=excluded.receipt
		WHERE local_fixture_events.payload=excluded.payload`, featureID, eventID, payload, receipt.Status, mustJSON(receipt))
	return err
}

func (s *Store) localFixtureReady(ctx context.Context, featureID, eventID string) (Feature, error) {
	if !uuid(featureID) || !safeText(eventID, 256) {
		return Feature{}, wireError(400, "invalid_fixture_source")
	}
	if _, err := s.db.ExecContext(ctx, localFixtureSchema); err != nil {
		return Feature{}, err
	}
	var raw []byte
	if err := s.db.QueryRowContext(ctx, "SELECT data FROM features WHERE id=?", featureID).Scan(&raw); err != nil {
		return Feature{}, err
	}
	var f Feature
	if err := json.Unmarshal(raw, &f); err != nil {
		return Feature{}, err
	}
	return f, nil
}

// InjectLocalFixture retains a stopped feature's input until an explicit
// fixture continue, using the same source key for every replay.
func (s *Store) InjectLocalFixture(ctx context.Context, featureID string, input InputPayload) (LocalFixtureReceipt, error) {
	f, err := s.localFixtureReady(ctx, featureID, input.Source.EventID)
	if err != nil {
		return LocalFixtureReceipt{}, err
	}
	if input.Source.Kind != "test" || input.Source.ChannelID != f.ChannelID || input.Source.ThreadTS != f.ThreadTS || !allowedActor(f, input.Source.ActorID) || !safeText(input.Text, 64*1024) {
		return LocalFixtureReceipt{}, wireError(403, "untrusted_fixture_input")
	}
	payload := mustJSON(struct {
		Kind  string       `json:"kind"`
		Input InputPayload `json:"input"`
	}{"input", input})
	old, found, err := s.localFixtureEvent(ctx, featureID, input.Source.EventID, payload)
	if err != nil {
		return LocalFixtureReceipt{}, err
	}
	if found {
		if old.Status == "buffered" && !f.Stopped {
			if _, err := s.releaseLocalFixtureInputs(ctx, featureID); err != nil {
				return LocalFixtureReceipt{}, err
			}
			old, _, err = s.localFixtureEvent(ctx, featureID, input.Source.EventID, payload)
			if err != nil {
				return LocalFixtureReceipt{}, err
			}
		}
		return old, nil
	}
	result := LocalFixtureReceipt{EventID: input.Source.EventID}
	if f.Stopped {
		result.Status = "buffered"
	} else {
		receipt, duplicate, err := s.Ingest(ctx, featureID, input)
		if err != nil {
			return LocalFixtureReceipt{}, err
		}
		result.Status, result.Duplicate = "delivered", duplicate
		result.MessageID, result.MailboxSeq = receipt.MessageID, receipt.MailboxSeq
	}
	if err := s.saveLocalFixtureEvent(ctx, featureID, input.Source.EventID, payload, result); err != nil {
		return LocalFixtureReceipt{}, err
	}
	return result, nil
}

// ControlLocalFixture applies an offline stop or continue from a configured
// actor. Repeated source IDs return their original result; a continue also
// drains any inputs retained while stopped.
func (s *Store) ControlLocalFixture(ctx context.Context, featureID, eventID, kind, actor string) (LocalFixtureReceipt, error) {
	f, err := s.localFixtureReady(ctx, featureID, eventID)
	if err != nil {
		return LocalFixtureReceipt{}, err
	}
	if !allowedActor(f, actor) || kind != "stop" && kind != "continue" {
		return LocalFixtureReceipt{}, wireError(403, "untrusted_fixture_control")
	}
	payload := mustJSON(struct {
		Kind  string `json:"kind"`
		Actor string `json:"actor"`
	}{kind, actor})
	old, found, err := s.localFixtureEvent(ctx, featureID, eventID, payload)
	if err != nil {
		return LocalFixtureReceipt{}, err
	}
	if found && kind == "stop" {
		return old, nil
	}
	result := LocalFixtureReceipt{EventID: eventID, Duplicate: found}
	if kind == "stop" {
		if !f.Stopped {
			if err := s.StopFeature(ctx, featureID, actor, "Local fixture stop"); err != nil {
				return result, err
			}
		}
		result.Status = "stopped"
	} else {
		if f.Stopped {
			if err := s.ContinueFeature(ctx, featureID, actor); err != nil {
				return result, err
			}
		}
		result.Status = "continued"
	}
	if err := s.saveLocalFixtureEvent(ctx, featureID, eventID, payload, result); err != nil {
		return result, err
	}
	if kind == "continue" {
		released, err := s.releaseLocalFixtureInputs(ctx, featureID)
		if err != nil {
			return result, err
		}
		result.Released = released
	}
	return result, nil
}

func (s *Store) releaseLocalFixtureInputs(ctx context.Context, featureID string) ([]Receipt, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT event_id,payload FROM local_fixture_events WHERE feature_id=? AND status='buffered' ORDER BY rowid`, featureID)
	if err != nil {
		return nil, err
	}
	type pending struct {
		id      string
		payload []byte
	}
	var inputs []pending
	for rows.Next() {
		var item pending
		if err := rows.Scan(&item.id, &item.payload); err != nil {
			rows.Close()
			return nil, err
		}
		inputs = append(inputs, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var released []Receipt
	for _, item := range inputs {
		var record struct {
			Input InputPayload `json:"input"`
		}
		if err := json.Unmarshal(item.payload, &record); err != nil {
			return nil, err
		}
		receipt, _, err := s.Ingest(ctx, featureID, record.Input)
		if err != nil {
			return nil, err
		}
		result := LocalFixtureReceipt{EventID: item.id, Status: "delivered", MessageID: receipt.MessageID, MailboxSeq: receipt.MailboxSeq}
		if err := s.saveLocalFixtureEvent(ctx, featureID, item.id, item.payload, result); err != nil {
			return nil, err
		}
		released = append(released, receipt)
	}
	return released, nil
}

// DecideLocalFixture uses the real wire-v2 request and decision pipeline. A
// request must already exist from an owner human action; this method cannot
// synthesize a backend request or bypass its responder and source checks.
func (s *Store) DecideLocalFixture(ctx context.Context, featureID, requestID, sourceID, kind, text, optionID, actor string) (LocalFixtureDecisionReceipt, error) {
	result := LocalFixtureDecisionReceipt{SourceID: sourceID, RequestID: requestID}
	if s.cfg.WireVersion != 2 || s.cfg.Human == nil || !uuid(requestID) || !validSlackTS(sourceID) || kind != "answer" && kind != "deny" {
		return result, wireError(400, "invalid_fixture_decision")
	}
	f, err := s.localFixtureReady(ctx, featureID, sourceID)
	if err != nil {
		return result, err
	}
	if !allowedActor(f, actor) {
		return result, wireError(403, "untrusted_fixture_actor")
	}
	var requestFeature, dependencyState, backendState string
	var revision int
	if err := s.db.QueryRowContext(ctx, `SELECT d.feature_id,d.state,p.state,p.revision
		FROM human_projections p JOIN dependencies d ON d.id=p.dependency_id WHERE p.request_id=?`, requestID).Scan(&requestFeature, &dependencyState, &backendState, &revision); err != nil {
		return result, wireError(404, "fixture_request_not_found")
	}
	if requestFeature != featureID {
		return result, wireError(409, "fixture_request_not_open")
	}
	var priorSource int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM source_ingress WHERE workspace_id=? AND channel_id=? AND message_ts=?`, s.cfg.Human.WorkspaceID, f.ChannelID, sourceID).Scan(&priorSource); err != nil {
		return result, err
	}
	if priorSource == 0 && (dependencyState != "human_waiting" || backendState != "open" || revision != 1) {
		return result, wireError(409, "fixture_request_not_open")
	}
	source := SlackSource{WorkspaceID: s.cfg.Human.WorkspaceID, ChannelID: f.ChannelID, MessageTS: sourceID, ThreadTS: f.ThreadTS, FeatureID: featureID, ActorID: actor, Text: text, EventID: sourceID}
	if _, err := s.CommitSlackSource(ctx, source); err != nil {
		return result, err
	}
	operationID, duplicate, err := s.RecordHumanAnswer(ctx, HumanAnswerInput{RequestID: requestID, Kind: kind, OptionID: optionID, Text: text, WorkspaceID: source.WorkspaceID, ChannelID: source.ChannelID, ThreadTS: source.ThreadTS, MessageTS: source.MessageTS, ActorID: actor, EventID: sourceID})
	if err != nil {
		return result, err
	}
	result.OperationID, result.Duplicate = operationID, duplicate
	if err := s.SettleSlackSource(ctx, source); err != nil {
		return result, err
	}
	if err := s.SyncHuman(ctx); err != nil {
		return result, err
	}
	if err := s.ApplyPendingHuman(ctx); err != nil {
		return result, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM backend_sync_operations WHERE operation_id=?`, operationID).Scan(&result.BackendSyncStatus); err != nil {
		return result, err
	}
	var raw []byte
	if err := s.db.QueryRowContext(ctx, `SELECT data FROM human_projections WHERE request_id=?`, requestID).Scan(&raw); err != nil {
		return result, err
	}
	var projection HumanProjection
	if err := json.Unmarshal(raw, &projection); err != nil {
		return result, err
	}
	result.ApplicationStatus = projection.ApplicationStatus
	return result, nil
}
