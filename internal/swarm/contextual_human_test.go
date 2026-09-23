package swarm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
)

func publishedHumanRequest(t *testing.T, f *fixture, options []HumanOption, status string) string {
	t.Helper()
	ctx := context.Background()
	requestID := NewID()
	d := Dependency{ID: NewID(), FeatureID: f.feature.FeatureID, Kind: "owner_step", StepKey: "choice", OriginTurnID: NewID(), SourceMessageID: NewID(), State: "human_waiting", RequestID: requestID, Blocker: Blocker{Reason: "Decision needed", Context: "test", Question: "What should happen?", Options: options, Recommendation: "Review", Kind: "choice"}, CreatedAt: f.s.stamp(), UpdatedAt: f.s.stamp()}
	v := humanBackendView{ID: requestID, TenantID: f.cfg.TenantID, ProjectID: f.cfg.ProjectID, FeatureID: f.feature.FeatureID, AllowedResponders: []string{"human"}, Options: options, State: "open", Revision: 1}
	v.Slack.WorkspaceID, v.Slack.ChannelID, v.Slack.ThreadTS = "workspace", f.feature.ChannelID, f.feature.ThreadTS
	v.Dependency.ID = d.ID
	p := HumanProjection{RequestID: requestID, DependencyID: d.ID, BackendState: "open", Revision: 1, ApplicationStatus: "pending", View: mustJSON(v)}
	q := SlackDelivery{ID: requestID, FeatureID: f.feature.FeatureID, ChannelID: f.feature.ChannelID, ThreadTS: f.feature.ThreadTS, SlackTS: "123.000005", Status: status, Text: questionText(d), Question: &HumanQuestion{Options: options}}
	if err := f.s.transaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO dependencies(id,feature_id,job_id,source_message_id,state,data) VALUES(?,?,?,?,?,?)", d.ID, d.FeatureID, "", d.SourceMessageID, d.State, mustJSON(d)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO human_projections(request_id,dependency_id,state,revision,data) VALUES(?,?,?,?,?)", requestID, d.ID, p.BackendState, p.Revision, mustJSON(p)); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO slack_outbox(id,feature_id,turn_id,status,data) VALUES(?,?,NULL,?,?)", requestID, f.feature.FeatureID, status, mustJSON(q))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return requestID
}

func contextualSlackSource(f *fixture, ts, text string) SlackSource {
	return SlackSource{WorkspaceID: "workspace", ChannelID: f.feature.ChannelID, ThreadTS: f.feature.ThreadTS, MessageTS: ts, FeatureID: f.feature.FeatureID, ActorID: "human", Text: text, EventID: "socket-event"}
}

func TestContextualAnswerBindingAndBackendDecisionProvenance(t *testing.T) {
	f := newHumanFixture(t)
	ctx := context.Background()
	requestID := publishedHumanRequest(t, f, nil, "sent")
	in := contextualSlackSource(f, "124.000001", "Ответ: Подробно объяснить риск")
	if _, err := f.s.CommitSlackSource(ctx, in); err != nil {
		t.Fatal(err)
	}
	bound, err := f.s.BindContextualHumanAnswer(ctx, in, "answer", "Подробно объяснить риск")
	if err != nil || bound != requestID {
		t.Fatalf("binding=%q err=%v", bound, err)
	}
	answer := HumanAnswerInput{RequestID: bound, Kind: "answer", Text: "Подробно объяснить риск", WorkspaceID: in.WorkspaceID, ChannelID: in.ChannelID, ThreadTS: in.ThreadTS, MessageTS: in.MessageTS, ActorID: in.ActorID}
	op, duplicate, err := f.s.RecordHumanAnswer(ctx, answer)
	if err != nil || duplicate || !uuid(op) {
		t.Fatalf("decision receipt=%q duplicate=%t err=%v", op, duplicate, err)
	}
	var raw []byte
	if err := f.s.db.QueryRowContext(ctx, "SELECT payload FROM backend_sync_operations WHERE operation_id=? AND kind='decision'", op).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Decision struct {
			Kind string `json:"kind"`
			Text string `json:"text"`
		} `json:"decision"`
		Source struct {
			ActorID   string `json:"actor_id"`
			MessageTS string `json:"message_ts"`
			ThreadTS  string `json:"thread_ts"`
		} `json:"source"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Decision.Kind != "answer" || body.Decision.Text != answer.Text || body.Source.ActorID != "human" || body.Source.MessageTS != in.MessageTS || body.Source.ThreadTS != in.ThreadTS {
		t.Fatalf("canonical provider payload=%s err=%v", raw, err)
	}
	// A crash after local decision commit can be replayed after the backend
	// projection has become terminal without selecting another request.
	if _, err := f.s.db.ExecContext(ctx, "UPDATE human_projections SET state='answered' WHERE request_id=?", requestID); err != nil {
		t.Fatal(err)
	}
	if rebound, err := f.s.BindContextualHumanAnswer(ctx, in, "answer", answer.Text); err != nil || rebound != requestID {
		t.Fatalf("replay binding=%q err=%v", rebound, err)
	}
	if same, duplicate, err := f.s.RecordHumanAnswer(ctx, answer); err != nil || !duplicate || same != op {
		t.Fatalf("decision replay=%q duplicate=%t err=%v", same, duplicate, err)
	}
	if _, err := f.s.BindContextualHumanAnswer(ctx, in, "deny", answer.Text); err == nil {
		t.Fatal("changed decision intent rebound same Slack source")
	}
}

func TestContextualAnswerRequiresOnePublishedQuestionAndOptionPolicy(t *testing.T) {
	for _, tc := range []struct {
		name      string
		options   []HumanOption
		status    string
		second    bool
		kind      string
		text      string
		wantError string
	}{
		{name: "unpublished", status: "queued", kind: "answer", text: "Reply", wantError: "no_open_human_request"},
		{name: "ambiguous", status: "sent", second: true, kind: "deny", text: "No authority", wantError: "ambiguous_human_request"},
		{name: "choice needs button", options: []HumanOption{{ID: "a", Label: "A"}}, status: "sent", kind: "answer", text: "A", wantError: "option_button_required"},
		{name: "deny with reason", options: []HumanOption{{ID: "a", Label: "A"}}, status: "sent", kind: "deny", text: "No authority"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHumanFixture(t)
			ctx := context.Background()
			id := publishedHumanRequest(t, f, tc.options, tc.status)
			if tc.second {
				publishedHumanRequest(t, f, nil, "sent")
			}
			in := contextualSlackSource(f, "124.000002", "Отказ: "+tc.text)
			if _, err := f.s.CommitSlackSource(ctx, in); err != nil {
				t.Fatal(err)
			}
			got, err := f.s.BindContextualHumanAnswer(ctx, in, tc.kind, tc.text)
			if tc.wantError != "" {
				var api *APIError
				if !errors.As(err, &api) || api.Code != tc.wantError {
					t.Fatalf("binding=%q err=%v, want %s", got, err, tc.wantError)
				}
				return
			}
			if err != nil || got != id {
				t.Fatalf("binding=%q err=%v", got, err)
			}
			answer := HumanAnswerInput{RequestID: got, Kind: "deny", Text: tc.text, WorkspaceID: in.WorkspaceID, ChannelID: in.ChannelID, ThreadTS: in.ThreadTS, MessageTS: in.MessageTS, ActorID: in.ActorID}
			if _, _, err := f.s.RecordHumanAnswer(ctx, answer); err != nil {
				t.Fatal(err)
			}
		})
	}
}
