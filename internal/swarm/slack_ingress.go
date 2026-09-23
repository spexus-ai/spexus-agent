package swarm

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// SlackSource is trusted transport metadata. EventID is an optional alias:
// Socket Mode and conversations.replies identify the same source by message TS.
type SlackSource struct {
	WorkspaceID        string               `json:"workspace_id"`
	ChannelID          string               `json:"channel_id"`
	MessageTS          string               `json:"message_ts"`
	ThreadTS           string               `json:"thread_ts"`
	FeatureID          string               `json:"feature_id"`
	ActorID            string               `json:"actor_id"`
	Text               string               `json:"text"`
	EventID            string               `json:"event_id,omitempty"`
	SourceKind         string               `json:"source_kind,omitempty"`
	QuestionTS         string               `json:"question_ts,omitempty"`
	RequestID          string               `json:"request_id,omitempty"`
	OptionID           string               `json:"option_id,omitempty"`
	DuringCatchup      bool                 `json:"during_catchup,omitempty"`
	ActiveHumanRequest *HumanRequestContext `json:"active_human_request,omitempty"`
}

func validSlackTS(ts string) bool {
	parts := strings.Split(ts, ".")
	if len(parts) != 2 || len(parts[0]) == 0 || len(parts[1]) == 0 || len(parts[1]) > 6 {
		return false
	}
	for _, r := range ts {
		if r != '.' && (r < '0' || r > '9') {
			return false
		}
	}
	_, err := strconv.ParseInt(parts[0], 10, 64)
	return err == nil
}

func slackTSCompare(a, b string) int {
	left := strings.Split(a, ".")
	right := strings.Split(b, ".")
	as, _ := strconv.ParseInt(left[0], 10, 64)
	bs, _ := strconv.ParseInt(right[0], 10, 64)
	if as < bs {
		return -1
	}
	if as > bs {
		return 1
	}
	return strings.Compare(left[1]+strings.Repeat("0", 6-len(left[1])), right[1]+strings.Repeat("0", 6-len(right[1])))
}

func (s *Store) CommitSlackSource(ctx context.Context, in SlackSource) (bool, error) {
	if s.cfg.WireVersion != 2 || !uuid(in.FeatureID) || in.WorkspaceID != s.cfg.Human.WorkspaceID || in.ChannelID == "" || in.ActorID == "" || !validSlackTS(in.MessageTS) || !validSlackTS(in.ThreadTS) || len(in.Text) > 16*1024 || in.SourceKind != "" && in.SourceKind != "block_action" {
		return false, wireError(400, "invalid_slack_source")
	}
	if in.SourceKind == "block_action" && (!uuid(in.RequestID) || !validSlackTS(in.QuestionTS) || in.OptionID == "" || len(in.OptionID) > 64 || in.Text != "") {
		return false, wireError(400, "invalid_slack_action")
	}
	duplicate := false
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		f, err := feature(ctx, tx, in.FeatureID)
		if err != nil {
			return err
		}
		if f.ChannelID != in.ChannelID || f.ThreadTS != in.ThreadTS || !allowedActor(f, in.ActorID) {
			return wireError(403, "untrusted_slack_source")
		}
		if in.SourceKind == "block_action" {
			var raw []byte
			if err := tx.QueryRowContext(ctx, "SELECT data FROM slack_outbox WHERE id=? AND feature_id=?", in.RequestID, in.FeatureID).Scan(&raw); err != nil {
				return wireError(403, "unknown_slack_question")
			}
			var q SlackDelivery
			if json.Unmarshal(raw, &q) != nil || q.Status != "sent" || q.ChannelID != in.ChannelID || q.ThreadTS != in.ThreadTS || q.SlackTS != in.QuestionTS {
				return wireError(403, "untrusted_slack_question")
			}
		}
		// The event ID is a delivery alias, not part of immutable source content.
		canonical := in
		canonical.EventID = ""
		canonical.DuringCatchup = false
		canonical.ActiveHumanRequest = nil
		payload := mustJSON(canonical)
		var prior []byte
		err = tx.QueryRowContext(ctx, "SELECT payload FROM slack_sources WHERE workspace_id=? AND channel_id=? AND message_ts=?", in.WorkspaceID, in.ChannelID, in.MessageTS).Scan(&prior)
		if err == nil {
			var stored SlackSource
			if json.Unmarshal(prior, &stored) != nil {
				return wireError(409, "source_conflict")
			}
			stored.DuringCatchup = false
			stored.ActiveHumanRequest = nil
			if !bytes.Equal(mustJSON(stored), payload) {
				return wireError(409, "source_conflict")
			}
			duplicate = true
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var barrierCount int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM recovery_barriers WHERE feature_id=?", in.FeatureID).Scan(&barrierCount); err != nil {
			return err
		}
		canonical.DuringCatchup = barrierCount != 0
		var activeQuestion SlackDelivery
		canonical.ActiveHumanRequest, activeQuestion, err = activeHumanRequestTx(ctx, tx, in.FeatureID)
		if err != nil {
			return err
		}
		if canonical.ActiveHumanRequest != nil && slackTSCompare(in.MessageTS, activeQuestion.SlackTS) <= 0 {
			canonical.ActiveHumanRequest = nil
		}
		payload = mustJSON(canonical)
		_, err = tx.ExecContext(ctx, "INSERT INTO slack_sources(workspace_id,channel_id,message_ts,feature_id,payload,status) VALUES(?,?,?,?,?,'pending')", in.WorkspaceID, in.ChannelID, in.MessageTS, in.FeatureID, payload)
		return err
	})
	return duplicate, err
}

// CommittedSlackSource returns the source with the active-question snapshot
// captured when it was first committed. Rebuilding that snapshot on redelivery
// would make the same Slack message produce a different owner input.
func (s *Store) CommittedSlackSource(ctx context.Context, workspace, channel, messageTS string) (SlackSource, error) {
	var out SlackSource
	if workspace == "" || channel == "" || !validSlackTS(messageTS) {
		return out, wireError(400, "invalid_slack_source")
	}
	var raw []byte
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM slack_sources WHERE workspace_id=? AND channel_id=? AND message_ts=?`, workspace, channel, messageTS).Scan(&raw); err != nil {
		return out, err
	}
	err := json.Unmarshal(raw, &out)
	return out, err
}

func (s *Store) SlackSourceExists(ctx context.Context, workspace, channel, messageTS string) (bool, error) {
	if workspace == "" || channel == "" || !validSlackTS(messageTS) {
		return false, wireError(400, "invalid_slack_source")
	}
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM slack_sources WHERE workspace_id=? AND channel_id=? AND message_ts=?", workspace, channel, messageTS).Scan(&n)
	return n != 0, err
}

// PublishedHumanQuestion resolves the feature thread from the bot's recorded
// message. Slack does not always include message.thread_ts in block_actions.
func (s *Store) PublishedHumanQuestion(ctx context.Context, requestID, questionTS string) (string, string, error) {
	if !uuid(requestID) || !validSlackTS(questionTS) {
		return "", "", wireError(400, "invalid_slack_question")
	}
	var raw []byte
	if err := s.db.QueryRowContext(ctx, "SELECT data FROM slack_outbox WHERE id=?", requestID).Scan(&raw); err != nil {
		return "", "", wireError(404, "unknown_slack_question")
	}
	var d SlackDelivery
	if json.Unmarshal(raw, &d) != nil || d.Status != "sent" || d.SlackTS != questionTS {
		return "", "", wireError(403, "untrusted_slack_question")
	}
	return d.FeatureID, d.ThreadTS, nil
}

func (s *Store) PendingSlackSources(ctx context.Context, featureID string) ([]SlackSource, error) {
	if !uuid(featureID) {
		return nil, wireError(400, "invalid_feature")
	}
	rows, err := s.db.QueryContext(ctx, "SELECT payload FROM slack_sources WHERE feature_id=? AND status='pending' ORDER BY message_ts", featureID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SlackSource
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var source SlackSource
		if err = json.Unmarshal(raw, &source); err != nil {
			return nil, err
		}
		out = append(out, source)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return slackTSCompare(out[i].MessageTS, out[j].MessageTS) < 0 })
	return out, nil
}

// DeferredStopSources returns committed !stop messages which have stopped the
// feature but have not yet become owner input. They are delivered after an
// explicit !continue, never while the feature remains stopped.
func (s *Store) DeferredStopSources(ctx context.Context, featureID string) ([]SlackSource, error) {
	if !uuid(featureID) {
		return nil, wireError(400, "invalid_feature")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT ss.payload FROM slack_sources ss
		WHERE ss.feature_id=? AND ss.status='done' AND json_extract(ss.payload,'$.text')='!stop'
		AND NOT EXISTS (SELECT 1 FROM ingress i WHERE i.feature_id=ss.feature_id AND i.event_id='slack:'||ss.channel_id||':'||ss.message_ts)`, featureID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SlackSource
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var source SlackSource
		if err = json.Unmarshal(raw, &source); err != nil {
			return nil, err
		}
		out = append(out, source)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return slackTSCompare(out[i].MessageTS, out[j].MessageTS) < 0 })
	return out, nil
}

// NewerStopSource reports whether a committed stop supersedes an older
// continue. Socket stop application and continue validation are serialized by
// the transport's stopMu, so a stale continue cannot reopen after a later stop.
func (s *Store) NewerStopSource(ctx context.Context, featureID, messageTS string) (bool, error) {
	if !uuid(featureID) || !validSlackTS(messageTS) {
		return false, wireError(400, "invalid_slack_source")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT message_ts FROM slack_sources WHERE feature_id=? AND json_extract(payload,'$.text')='!stop'`, featureID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var stopTS string
		if err = rows.Scan(&stopTS); err != nil {
			return false, err
		}
		if slackTSCompare(stopTS, messageTS) > 0 {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) SettleSlackSource(ctx context.Context, in SlackSource) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "UPDATE slack_sources SET status='done' WHERE workspace_id=? AND channel_id=? AND message_ts=? AND feature_id=?", in.WorkspaceID, in.ChannelID, in.MessageTS, in.FeatureID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return wireError(404, "source_not_found")
		}
		return nil
	})
}

// SlackWatermark creates the first cutover at the moment wire v2 starts. P2
// thread history before that point is never interpreted as a new command.
func (s *Store) SlackWatermark(ctx context.Context, featureID string) (string, error) {
	if s.cfg.WireVersion != 2 || !uuid(featureID) {
		return "", wireError(400, "invalid_feature")
	}
	var watermark string
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if _, err := feature(ctx, tx, featureID); err != nil {
			return err
		}
		now := s.now()
		cutover := fmt.Sprintf("%d.%06d", now.Unix(), now.Nanosecond()/1000)
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO slack_catchup(feature_id,watermark) VALUES(?,?)", featureID, cutover); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, "SELECT watermark FROM slack_catchup WHERE feature_id=?", featureID).Scan(&watermark)
	})
	return watermark, err
}

func (s *Store) AdvanceSlackWatermark(ctx context.Context, featureID, upper string) error {
	if !validSlackTS(upper) {
		return wireError(400, "invalid_watermark")
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var old string
		if err := tx.QueryRowContext(ctx, "SELECT watermark FROM slack_catchup WHERE feature_id=?", featureID).Scan(&old); err != nil {
			return err
		}
		if slackTSCompare(upper, old) <= 0 {
			return nil
		}
		_, err := tx.ExecContext(ctx, "UPDATE slack_catchup SET watermark=? WHERE feature_id=?", upper, featureID)
		return err
	})
}
