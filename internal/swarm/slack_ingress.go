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
	WorkspaceID string `json:"workspace_id"`
	ChannelID   string `json:"channel_id"`
	MessageTS   string `json:"message_ts"`
	ThreadTS    string `json:"thread_ts"`
	FeatureID   string `json:"feature_id"`
	ActorID     string `json:"actor_id"`
	Text        string `json:"text"`
	EventID     string `json:"event_id,omitempty"`
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
	if s.cfg.WireVersion != 2 || !uuid(in.FeatureID) || in.WorkspaceID != s.cfg.Human.WorkspaceID || in.ChannelID == "" || in.ActorID == "" || !validSlackTS(in.MessageTS) || !validSlackTS(in.ThreadTS) || len(in.Text) > 16*1024 {
		return false, wireError(400, "invalid_slack_source")
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
		// The event ID is a delivery alias, not part of immutable source content.
		canonical := in
		canonical.EventID = ""
		payload := mustJSON(canonical)
		var prior []byte
		err = tx.QueryRowContext(ctx, "SELECT payload FROM slack_sources WHERE workspace_id=? AND channel_id=? AND message_ts=?", in.WorkspaceID, in.ChannelID, in.MessageTS).Scan(&prior)
		if err == nil {
			if !bytes.Equal(prior, payload) {
				return wireError(409, "source_conflict")
			}
			duplicate = true
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO slack_sources(workspace_id,channel_id,message_ts,feature_id,payload,status) VALUES(?,?,?,?,?,'pending')", in.WorkspaceID, in.ChannelID, in.MessageTS, in.FeatureID, payload)
		return err
	})
	return duplicate, err
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
