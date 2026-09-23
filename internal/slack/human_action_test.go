package slack

import (
	"encoding/json"
	"testing"
)

// Test: only a scoped Block Kit click carries an answer; a forged container,
// workspace, action identity, or value cannot become a trusted Slack event.
// Validates: AC-431/464 (REQ-349/391 - durable provenance and authorization).
func TestHumanBlockActionEnvelope(t *testing.T) {
	value, _ := json.Marshal(map[string]string{"request_id": "123e4567-e89b-42d3-a456-426614174000", "option_id": "short"})
	base := map[string]any{
		"type": "block_actions", "team": map[string]string{"id": "T"}, "user": map[string]string{"id": "U", "team_id": "T"},
		"channel": map[string]string{"id": "C"}, "container": map[string]string{"type": "message", "channel_id": "C", "message_ts": "9.000001"},
		"message": map[string]string{"ts": "9.000001", "thread_ts": "1.000001"},
		"actions": []any{map[string]string{"type": "button", "action_id": HumanAnswerActionID + ":short", "value": string(value), "action_ts": "10.000001"}},
	}
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
		valid  bool
	}{
		{"valid", func(map[string]any) {}, true},
		{"thread omitted", func(m map[string]any) { m["message"] = map[string]string{"ts": "9.000001"} }, true},
		{"optional channel and message omitted", func(m map[string]any) { delete(m, "channel"); delete(m, "message") }, true},
		{"foreign channel", func(m map[string]any) { m["channel"] = map[string]string{"id": "OTHER"} }, false},
		{"foreign workspace", func(m map[string]any) { m["user"] = map[string]string{"id": "U", "team_id": "OTHER"} }, false},
		{"wrong question", func(m map[string]any) { m["message"] = map[string]string{"ts": "8.000001", "thread_ts": "1.000001"} }, false},
		{"mismatched action", func(m map[string]any) {
			m["actions"] = []any{map[string]string{"type": "button", "action_id": HumanAnswerActionID + ":other", "value": string(value), "action_ts": "10.000001"}}
		}, false},
		{"oversize value", func(m map[string]any) {
			m["actions"] = []any{map[string]string{"type": "button", "action_id": HumanAnswerActionID + ":short", "value": string(make([]byte, 2001)), "action_ts": "10.000001"}}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := map[string]any{}
			for k, v := range base {
				m[k] = v
			}
			tc.change(m)
			b, _ := json.Marshal(m)
			got, ok, err := eventFromSocketModeEnvelope(socketModeEnvelope{EnvelopeID: "delivery", Type: "interactive", Payload: b})
			if tc.valid {
				wantThread := "1.000001"
				if tc.name == "thread omitted" || tc.name == "optional channel and message omitted" {
					wantThread = ""
				}
				if !ok || err != nil || got.WorkspaceID != "T" || got.ChannelID != "C" || got.ThreadTS != wantThread || got.Timestamp != "10.000001" || got.HumanAction == nil || got.HumanAction.OptionID != "short" {
					t.Fatalf("got=%+v ok=%t err=%v", got, ok, err)
				}
			} else if ok || err == nil {
				t.Fatalf("forged action accepted: got=%+v ok=%t err=%v", got, ok, err)
			}
		})
	}
}

func TestHumanControlBlockActionEnvelope(t *testing.T) {
	requestID := "123e4567-e89b-42d3-a456-426614174000"
	for _, control := range []string{"details", "stop"} {
		value, _ := json.Marshal(map[string]string{"request_id": requestID, "control_id": control})
		payload := map[string]any{
			"type": "block_actions", "team": map[string]string{"id": "T"}, "user": map[string]string{"id": "U", "team_id": "T"},
			"channel": map[string]string{"id": "C"}, "container": map[string]string{"type": "message", "channel_id": "C", "message_ts": "9.000001"},
			"message": map[string]string{"ts": "9.000001", "thread_ts": "1.000001"},
			"actions": []any{map[string]string{"type": "button", "action_id": HumanControlActionID + ":" + control, "value": string(value), "action_ts": "10.000001"}},
		}
		b, _ := json.Marshal(payload)
		got, ok, err := eventFromSocketModeEnvelope(socketModeEnvelope{EnvelopeID: "delivery", Type: "interactive", Payload: b})
		if err != nil || !ok || got.HumanAction == nil || got.HumanAction.ControlID != control || got.HumanAction.OptionID != "" {
			t.Fatalf("control %s: got=%+v ok=%t err=%v", control, got, ok, err)
		}
		value, _ = json.Marshal(map[string]string{"request_id": requestID, "control_id": "continue"})
		payload["actions"] = []any{map[string]string{"type": "button", "action_id": HumanControlActionID + ":continue", "value": string(value), "action_ts": "10.000001"}}
		b, _ = json.Marshal(payload)
		if _, ok, err := eventFromSocketModeEnvelope(socketModeEnvelope{EnvelopeID: "delivery", Type: "interactive", Payload: b}); ok || err == nil {
			t.Fatalf("unapproved control accepted: ok=%t err=%v", ok, err)
		}
	}
}

func TestFeatureStopControlRequiresRootMessage(t *testing.T) {
	value, _ := json.Marshal(map[string]string{"feature_id": "123e4567-e89b-42d3-a456-426614174000"})
	base := map[string]any{
		"type": "block_actions", "team": map[string]string{"id": "T"}, "user": map[string]string{"id": "U", "team_id": "T"},
		"channel": map[string]string{"id": "C"}, "container": map[string]string{"type": "message", "channel_id": "C", "message_ts": "1.000001"},
		"message": map[string]string{"ts": "1.000001"},
		"actions": []any{map[string]string{"type": "button", "action_id": FeatureControlActionID + ":stop", "value": string(value), "action_ts": "2.000001"}},
	}
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		valid  bool
	}{
		{"root", func(map[string]any) {}, true},
		{"root thread metadata", func(m map[string]any) { m["message"] = map[string]string{"ts": "1.000001", "thread_ts": "1.000001"} }, true},
		{"reply", func(m map[string]any) { m["message"] = map[string]string{"ts": "1.000001", "thread_ts": "0.000001"} }, false},
		{"wrong channel", func(m map[string]any) { m["channel"] = map[string]string{"id": "other"} }, false},
		{"wrong workspace", func(m map[string]any) { m["user"] = map[string]string{"id": "U", "team_id": "other"} }, false},
		{"missing feature", func(m map[string]any) {
			m["actions"] = []any{map[string]string{"type": "button", "action_id": FeatureControlActionID + ":stop", "value": "{}", "action_ts": "2.000001"}}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := map[string]any{}
			for key, v := range base {
				m[key] = v
			}
			tc.mutate(m)
			payload, _ := json.Marshal(m)
			got, ok, err := eventFromSocketModeEnvelope(socketModeEnvelope{EnvelopeID: "delivery", Type: "interactive", Payload: payload})
			if tc.valid {
				if err != nil || !ok || got.FeatureControl == nil || got.FeatureControl.FeatureID != "123e4567-e89b-42d3-a456-426614174000" || got.ThreadTS != "1.000001" || got.Timestamp != "2.000001" {
					t.Fatalf("root control: got=%+v ok=%t err=%v", got, ok, err)
				}
			} else if err == nil || ok {
				t.Fatalf("invalid root control accepted: got=%+v ok=%t err=%v", got, ok, err)
			}
		})
	}
}
