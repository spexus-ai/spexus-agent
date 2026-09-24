package swarm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// ActiveHumanRequest is the one question currently presented in this feature's
// Slack thread. Publication order is durable in slack_outbox, so a restart does
// not change which question the owner should discuss with the human.
func (s *Store) ActiveHumanRequest(ctx context.Context, featureID string) (*HumanRequestContext, error) {
	if !uuid(featureID) {
		return nil, wireError(400, "invalid_feature")
	}
	var out *HumanRequestContext
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		var err error
		out, _, err = activeHumanRequestTx(ctx, tx, featureID)
		return err
	})
	return out, err
}

func activeHumanRequestTx(ctx context.Context, tx *sql.Tx, featureID string) (*HumanRequestContext, SlackDelivery, error) {
	var requestID string
	var questionRaw, dependencyRaw []byte
	err := tx.QueryRowContext(ctx, `SELECT o.id,o.data,d.data FROM slack_outbox o
		JOIN human_projections p ON p.request_id=o.id
		JOIN dependencies d ON d.id=p.dependency_id
		WHERE o.feature_id=? AND o.status='sent' AND p.state='open'
		ORDER BY o.rowid LIMIT 1`, featureID).Scan(&requestID, &questionRaw, &dependencyRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, SlackDelivery{}, nil
	}
	if err != nil {
		return nil, SlackDelivery{}, err
	}
	var delivery SlackDelivery
	var dependency Dependency
	if err = json.Unmarshal(questionRaw, &delivery); err != nil {
		return nil, SlackDelivery{}, err
	}
	if err = json.Unmarshal(dependencyRaw, &dependency); err != nil {
		return nil, SlackDelivery{}, err
	}
	if delivery.ID != requestID || delivery.Status != "sent" || dependency.RequestID != requestID {
		return nil, SlackDelivery{}, errors.New("human question projection mismatch")
	}
	options := dependency.Blocker.Options
	if options == nil {
		options = []HumanOption{}
	}
	return &HumanRequestContext{
		RequestID: requestID, Question: dependency.Blocker.Question,
		Reason: dependency.Blocker.Reason, Context: dependency.Blocker.Context,
		Options: options, Recommendation: dependency.Blocker.Recommendation,
		BlockedWork: dependency.BlockedWork, Kind: dependency.Blocker.Kind,
	}, delivery, nil
}

// decideHumanTx is called only after owner-turn authorization by applyMessage.
// The model chooses the interpretation, while provenance and active-question
// authority are reconstructed from immutable local transport records.
func (s *Store) decideHumanTx(ctx context.Context, tx *sql.Tx, e Envelope, p HumanRespondPayload) error {
	f, err := feature(ctx, tx, e.FeatureID)
	if err != nil {
		return err
	}
	if s.cfg.Human == nil || f.Stopped {
		return wireError(409, "human_response_unavailable")
	}
	active, question, err := activeHumanRequestTx(ctx, tx, f.FeatureID)
	if err != nil {
		return err
	}
	if active == nil || active.RequestID != p.RequestID {
		return wireError(409, "request_not_active")
	}
	if !validSlackTS(question.SlackTS) || slackTSCompare(p.SourceMessageTS, question.SlackTS) <= 0 {
		return wireError(409, "source_precedes_question")
	}
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT payload FROM slack_sources WHERE workspace_id=? AND channel_id=? AND message_ts=? AND feature_id=?`, s.cfg.Human.WorkspaceID, f.ChannelID, p.SourceMessageTS, f.FeatureID).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return wireError(403, "untrusted_human_source")
		}
		return err
	}
	var source SlackSource
	if err = json.Unmarshal(raw, &source); err != nil {
		return err
	}
	if source.FeatureID != f.FeatureID || source.ThreadTS != f.ThreadTS || source.ChannelID != f.ChannelID || source.WorkspaceID != s.cfg.Human.WorkspaceID || !allowedActor(f, source.ActorID) {
		return wireError(403, "untrusted_human_source")
	}
	// Details and stop buttons are controls, never an answer. The owner model
	// may explain a question after a details click, but cannot turn that click
	// into a human decision even if it emits human.respond by mistake.
	if source.SourceKind != "" && source.SourceKind != "block_action" {
		return wireError(409, "control_is_not_human_answer")
	}
	if source.SourceKind == "block_action" && (source.RequestID != p.RequestID || p.Kind != "answer" || p.OptionID != source.OptionID || p.Text != "") {
		return wireError(409, "human_action_mismatch")
	}
	// The owner must have actually received the Slack source in an agent.input.
	// A source merely stored by the socket cannot be cited by a model action.
	var inputRaw []byte
	var inputSeq int64
	var acked, superseded int
	eventID := "slack:" + source.ChannelID + ":" + source.MessageTS
	if err = tx.QueryRowContext(ctx, `SELECT m.canonical,d.seq,d.acked,d.superseded FROM ingress i
		JOIN messages m ON m.message_id=json_extract(i.receipt,'$.message_id') AND m.sender='coordinator'
		JOIN mailbox_delivery d ON d.message_row=m.id AND d.agent_id=?
		WHERE i.feature_id=? AND i.event_id=?`, f.OwnerAgentID, f.FeatureID, eventID).Scan(&inputRaw, &inputSeq, &acked, &superseded); err != nil {
		return wireError(403, "source_not_delivered")
	}
	var input Envelope
	var inputPayload InputPayload
	if json.Unmarshal(inputRaw, &input) != nil || json.Unmarshal(input.Payload, &inputPayload) != nil || input.Type != "agent.input" || inputPayload.Source.MessageTS != source.MessageTS || inputPayload.Source.ActorID != source.ActorID || inputPayload.Source.ChannelID != source.ChannelID || inputPayload.Source.ThreadTS != source.ThreadTS || inputPayload.ActiveHumanRequest == nil || inputPayload.ActiveHumanRequest.RequestID != p.RequestID || acked != 1 || superseded != 0 {
		return wireError(403, "source_not_delivered")
	}
	t, err := turn(ctx, tx, e.OwnerTurnID)
	if err != nil {
		return err
	}
	if inputSeq > t.InputMailboxSeq {
		return wireError(403, "source_not_delivered")
	}
	answer := HumanAnswerInput{RequestID: p.RequestID, Kind: p.Kind, OptionID: p.OptionID, Text: p.Text, WorkspaceID: source.WorkspaceID, ChannelID: source.ChannelID, ThreadTS: source.ThreadTS, MessageTS: source.MessageTS, ActorID: source.ActorID}
	_, _, err = s.recordHumanAnswerTx(ctx, tx, answer)
	return err
}
