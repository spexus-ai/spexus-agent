package slack

import (
	"encoding/json"
	"errors"
	"strings"
)

const HumanAnswerActionID = "spexus_human_answer_v1"

type humanActionPayload struct {
	Type string `json:"type"`
	Team struct {
		ID string `json:"id"`
	} `json:"team"`
	User struct {
		ID     string `json:"id"`
		TeamID string `json:"team_id"`
	} `json:"user"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	Container struct {
		Type      string `json:"type"`
		ChannelID string `json:"channel_id"`
		MessageTS string `json:"message_ts"`
	} `json:"container"`
	Message struct {
		TS       string `json:"ts"`
		ThreadTS string `json:"thread_ts"`
	} `json:"message"`
	Actions []struct {
		Type     string `json:"type"`
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
		ActionTS string `json:"action_ts"`
	} `json:"actions"`
}

func humanActionFromSocketModeEnvelope(envelope socketModeEnvelope) (Event, bool, error) {
	var p humanActionPayload
	if err := json.Unmarshal(envelope.Payload, &p); err != nil {
		return Event{}, false, err
	}
	if p.Type != "block_actions" || len(p.Actions) != 1 || !strings.HasPrefix(p.Actions[0].ActionID, HumanAnswerActionID+":") {
		return Event{}, false, nil
	}
	a := p.Actions[0]
	if a.Type != "button" || p.Team.ID == "" || p.User.ID == "" || p.Container.Type != "message" || p.Container.ChannelID == "" || p.Container.MessageTS == "" || p.Message.TS != "" && p.Message.TS != p.Container.MessageTS || p.Channel.ID != "" && p.Channel.ID != p.Container.ChannelID || p.User.TeamID != "" && p.User.TeamID != p.Team.ID || a.ActionTS == "" {
		return Event{}, false, errors.New("invalid human action provenance")
	}
	var value struct {
		RequestID string `json:"request_id"`
		OptionID  string `json:"option_id"`
	}
	if len(a.Value) > 2000 || json.Unmarshal([]byte(a.Value), &value) != nil || value.RequestID == "" || value.OptionID == "" || strings.ContainsAny(value.OptionID, "\n\r") || a.ActionID != HumanAnswerActionID+":"+value.OptionID {
		return Event{}, false, errors.New("invalid human action value")
	}
	return Event{ID: envelope.EnvelopeID, WorkspaceID: p.Team.ID, ChannelID: p.Container.ChannelID, ThreadTS: p.Message.ThreadTS, Timestamp: a.ActionTS, UserID: p.User.ID, HumanAction: &HumanAction{RequestID: value.RequestID, OptionID: value.OptionID, QuestionTS: p.Container.MessageTS}}, true, nil
}
