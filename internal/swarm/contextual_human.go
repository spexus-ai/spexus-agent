package swarm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// BindContextualHumanAnswer resolves a short Slack reply once and persists
// that resolution before any backend decision is attempted. A replay cannot
// attach the same human message to a different request after a restart.
func (s *Store) BindContextualHumanAnswer(ctx context.Context, in SlackSource, kind, answerText string) (string, error) {
	if s.cfg.WireVersion != 2 || in.SourceKind != "" || !uuid(in.FeatureID) || in.WorkspaceID != s.cfg.Human.WorkspaceID || !validSlackTS(in.MessageTS) || !validSlackTS(in.ThreadTS) || (kind != "answer" && kind != "deny") || !safeText(answerText, 16*1024) {
		return "", wireError(400, "invalid_contextual_answer")
	}
	var requestID string
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		f, err := feature(ctx, tx, in.FeatureID)
		if err != nil {
			return err
		}
		if f.Stopped || f.ChannelID != in.ChannelID || f.ThreadTS != in.ThreadTS || !allowedActor(f, in.ActorID) {
			return wireError(403, "untrusted_contextual_answer")
		}
		var committed []byte
		var status string
		err = tx.QueryRowContext(ctx, `SELECT payload,status FROM slack_sources WHERE workspace_id=? AND channel_id=? AND message_ts=? AND feature_id=?`, in.WorkspaceID, in.ChannelID, in.MessageTS, in.FeatureID).Scan(&committed, &status)
		if err != nil || status != "pending" {
			return wireError(409, "source_not_pending")
		}
		var original SlackSource
		if json.Unmarshal(committed, &original) != nil || original.WorkspaceID != in.WorkspaceID || original.ChannelID != in.ChannelID || original.ThreadTS != in.ThreadTS || original.MessageTS != in.MessageTS || original.FeatureID != in.FeatureID || original.ActorID != in.ActorID || original.Text != in.Text || original.SourceKind != "" {
			return wireError(409, "source_conflict")
		}
		digest := Digest([]byte(kind + "\x00" + answerText + "\x00" + in.Text))
		var oldFeature, oldActor, oldKind, oldDigest string
		err = tx.QueryRowContext(ctx, `SELECT feature_id,request_id,actor_id,kind,text_sha256 FROM contextual_human_bindings WHERE workspace_id=? AND channel_id=? AND message_ts=?`, in.WorkspaceID, in.ChannelID, in.MessageTS).Scan(&oldFeature, &requestID, &oldActor, &oldKind, &oldDigest)
		if err == nil {
			if oldFeature != in.FeatureID || oldActor != in.ActorID || oldKind != kind || oldDigest != digest {
				return wireError(409, "contextual_binding_conflict")
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT p.request_id,p.data,o.data,d.data FROM human_projections p JOIN dependencies d ON d.id=p.dependency_id JOIN slack_outbox o ON o.id=p.request_id WHERE d.feature_id=? AND d.state='human_waiting' AND p.state='open' AND o.feature_id=? AND o.status='sent'`, in.FeatureID, in.FeatureID)
		if err != nil {
			return err
		}
		count := 0
		var selected HumanProjection
		for rows.Next() {
			var id string
			var projectionRaw, questionRaw, dependencyRaw []byte
			if err = rows.Scan(&id, &projectionRaw, &questionRaw, &dependencyRaw); err != nil {
				rows.Close()
				return err
			}
			var p HumanProjection
			var q SlackDelivery
			var d Dependency
			if err = json.Unmarshal(projectionRaw, &p); err != nil {
				rows.Close()
				return err
			}
			if err = json.Unmarshal(questionRaw, &q); err != nil {
				rows.Close()
				return err
			}
			if err = json.Unmarshal(dependencyRaw, &d); err != nil {
				rows.Close()
				return err
			}
			if p.RequestID != id || d.RequestID != id || q.ID != id || q.ChannelID != f.ChannelID || q.ThreadTS != f.ThreadTS || !validSlackTS(q.SlackTS) || slackTSCompare(in.MessageTS, q.SlackTS) <= 0 {
				continue
			}
			count++
			selected = p
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if count == 0 {
			return wireError(409, "no_open_human_request")
		}
		if count != 1 {
			return wireError(409, "ambiguous_human_request")
		}
		var view humanBackendView
		if err = json.Unmarshal(selected.View, &view); err != nil {
			return err
		}
		if view.ID != selected.RequestID || view.Slack.WorkspaceID != in.WorkspaceID || view.Slack.ChannelID != in.ChannelID || view.Slack.ThreadTS != in.ThreadTS {
			return wireError(409, "request_source_mismatch")
		}
		actorAllowed := false
		for _, actor := range view.AllowedResponders {
			actorAllowed = actorAllowed || actor == in.ActorID
		}
		if !actorAllowed {
			return wireError(403, "untrusted_contextual_answer")
		}
		if kind == "answer" && len(view.Options) != 0 {
			return wireError(409, "option_button_required")
		}
		requestID = selected.RequestID
		if _, err = tx.ExecContext(ctx, `INSERT INTO contextual_human_bindings(workspace_id,channel_id,message_ts,feature_id,request_id,actor_id,kind,text_sha256,bound_at) VALUES(?,?,?,?,?,?,?,?,?)`, in.WorkspaceID, in.ChannelID, in.MessageTS, in.FeatureID, requestID, in.ActorID, kind, digest, s.stamp()); err != nil {
			return err
		}
		return s.audit(ctx, tx, Principal{AgentID: in.ActorID}, Envelope{FeatureID: in.FeatureID}, "contextual_human_bound", kind)
	})
	return requestID, err
}
