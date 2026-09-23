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
