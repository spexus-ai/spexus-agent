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
		selector, err := f.s.humanSelector(ctx, tx, f.feature.FeatureID, requestID)
		if err != nil {
			return err
		}
		q.ShortSelector = selector
		_, err = tx.ExecContext(ctx, "INSERT INTO slack_outbox(id,feature_id,turn_id,status,data) VALUES(?,?,NULL,?,?)", requestID, f.feature.FeatureID, status, mustJSON(q))
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
	bound, err := f.s.BindContextualHumanAnswer(ctx, in, "answer", 0, "Подробно объяснить риск")
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
	if rebound, err := f.s.BindContextualHumanAnswer(ctx, in, "answer", 0, answer.Text); err != nil || rebound != requestID {
		t.Fatalf("replay binding=%q err=%v", rebound, err)
	}
	if same, duplicate, err := f.s.RecordHumanAnswer(ctx, answer); err != nil || !duplicate || same != op {
		t.Fatalf("decision replay=%q duplicate=%t err=%v", same, duplicate, err)
	}
	if _, err := f.s.BindContextualHumanAnswer(ctx, in, "deny", 0, answer.Text); err == nil {
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
			got, err := f.s.BindContextualHumanAnswer(ctx, in, tc.kind, 0, tc.text)
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

func TestHumanSelectorsDisambiguateAndNeverReuse(t *testing.T) {
	f := newHumanFixture(t)
	ctx := context.Background()
	first := publishedHumanRequest(t, f, nil, "sent")
	second := publishedHumanRequest(t, f, nil, "sent")
	var n1, n2 int
	if err := f.s.db.QueryRowContext(ctx, `SELECT selector FROM human_request_selectors WHERE request_id=?`, first).Scan(&n1); err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.QueryRowContext(ctx, `SELECT selector FROM human_request_selectors WHERE request_id=?`, second).Scan(&n2); err != nil {
		t.Fatal(err)
	}
	if n1 != 1 || n2 != 2 {
		t.Fatalf("selectors %d, %d", n1, n2)
	}
	in := contextualSlackSource(f, "124.000003", "Ответ #2: второй вопрос")
	if _, err := f.s.CommitSlackSource(ctx, in); err != nil {
		t.Fatal(err)
	}
	if selected, err := f.s.BindContextualHumanAnswer(ctx, in, "answer", 2, "второй вопрос"); err != nil || selected != second {
		t.Fatalf("selected %q, err=%v", selected, err)
	}
	in2 := contextualSlackSource(f, "124.000004", "Ответ #3: неизвестный вопрос")
	if _, err := f.s.CommitSlackSource(ctx, in2); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.BindContextualHumanAnswer(ctx, in2, "answer", 3, "неизвестный вопрос"); err == nil {
		t.Fatal("unknown selector was accepted")
	}
	if _, err := f.s.db.ExecContext(ctx, `UPDATE human_projections SET state='answered' WHERE request_id=?`, first); err != nil {
		t.Fatal(err)
	}
	third := publishedHumanRequest(t, f, nil, "sent")
	var n3 int
	if err := f.s.db.QueryRowContext(ctx, `SELECT selector FROM human_request_selectors WHERE request_id=?`, third).Scan(&n3); err != nil || n3 != 3 {
		t.Fatalf("third selector=%d err=%v", n3, err)
	}
}

func TestNewReplyToClosedSelectorBindsCanonicalRequestWithoutNewDecision(t *testing.T) {
	f := newHumanFixture(t)
	ctx := context.Background()
	_ = publishedHumanRequest(t, f, nil, "sent")
	closed := publishedHumanRequest(t, f, []HumanOption{{ID: "no", Label: "Нет"}}, "sent")
	if err := f.s.transaction(ctx, func(tx *sql.Tx) error {
		var raw []byte
		if err := tx.QueryRowContext(ctx, `SELECT data FROM human_projections WHERE request_id=?`, closed).Scan(&raw); err != nil {
			return err
		}
		var p HumanProjection
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		p.BackendState = "answered"
		p.Revision = 2
		var view humanBackendView
		if err := json.Unmarshal(p.View, &view); err != nil {
			return err
		}
		view.State = "answered"
		view.Revision = 2
		view.Terminal = &struct {
			ID       string          `json:"id"`
			Kind     string          `json:"kind"`
			Response json.RawMessage `json:"response"`
			Source   json.RawMessage `json:"source"`
		}{ID: NewID(), Kind: "answer", Response: json.RawMessage(`{"option_id":"no"}`), Source: json.RawMessage(`{"actor_id":"human"}`)}
		p.View = mustJSON(view)
		if _, err := tx.ExecContext(ctx, `UPDATE human_projections SET state='answered',revision=2,data=? WHERE request_id=?`, mustJSON(p), closed); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	in := contextualSlackSource(f, "124.000006", "ответ 2: отказ потому что не хочу")
	if _, err := f.s.CommitSlackSource(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, err := f.s.BindContextualHumanAnswer(ctx, in, "deny", 2, "потому что не хочу")
	if err != nil || got != closed {
		t.Fatalf("closed selector bound=%q err=%v", got, err)
	}
	var count int
	if err := f.s.db.QueryRowContext(ctx, `SELECT count(*) FROM backend_sync_operations WHERE request_id=? AND kind='decision'`, closed).Scan(&count); err != nil || count != 0 {
		t.Fatalf("closed selector enqueued decision count=%d err=%v", count, err)
	}
}

func TestSelectorBackfillAndBoundReplayAfterRevocation(t *testing.T) {
	f := newHumanFixture(t)
	ctx := context.Background()
	first := publishedHumanRequest(t, f, nil, "sent")
	second := publishedHumanRequest(t, f, nil, "sent")
	// Simulate an old database whose questions predate short selectors.
	if err := f.s.transaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM human_request_selectors`); err != nil {
			return err
		}
		for _, id := range []string{first, second} {
			var raw []byte
			if err := tx.QueryRowContext(ctx, `SELECT data FROM slack_outbox WHERE id=?`, id).Scan(&raw); err != nil {
				return err
			}
			var q SlackDelivery
			if err := json.Unmarshal(raw, &q); err != nil {
				return err
			}
			q.ShortSelector = 0
			if _, err := tx.ExecContext(ctx, `UPDATE slack_outbox SET data=? WHERE id=?`, mustJSON(q), id); err != nil {
				return err
			}
		}
		return f.s.backfillHumanSelectors(ctx, tx)
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.transaction(ctx, func(tx *sql.Tx) error { return f.s.backfillHumanSelectors(ctx, tx) }); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{first, second} {
		var raw []byte
		if err := f.s.db.QueryRowContext(ctx, `SELECT data FROM slack_outbox WHERE id=?`, id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var q SlackDelivery
		if err := json.Unmarshal(raw, &q); err != nil || q.ShortSelector != i+1 {
			t.Fatalf("backfill question %d: %+v err=%v", i, q, err)
		}
	}
	in := contextualSlackSource(f, "124.000005", "Отказ 2: недостаточно данных")
	if _, err := f.s.CommitSlackSource(ctx, in); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := f.s.CommitSlackSource(ctx, in); err != nil || !duplicate {
		t.Fatalf("source redelivery duplicate=%t err=%v", duplicate, err)
	}
	if got, err := f.s.BindContextualHumanAnswer(ctx, in, "deny", 2, "недостаточно данных"); err != nil || got != second {
		t.Fatalf("bind=%q err=%v", got, err)
	}
	if _, err := f.s.db.ExecContext(ctx, `UPDATE human_projections SET state='denied' WHERE request_id=?`, second); err != nil {
		t.Fatal(err)
	}
	revoked := f.feature
	revoked.AllowedActorIDs = []string{"someone-else"}
	if err := f.s.RegisterFeature(ctx, revoked); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.BindContextualHumanAnswer(ctx, in, "deny", 2, "недостаточно данных"); err != nil || got != second {
		t.Fatalf("replay after revocation=%q err=%v", got, err)
	}
	if err := f.s.StopFeature(ctx, f.feature.FeatureID, "someone-else", "operator stop"); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.BindContextualHumanAnswer(ctx, in, "deny", 2, "недостаточно данных"); err != nil || got != second {
		t.Fatalf("replay after stop=%q err=%v", got, err)
	}
	var dbPath string
	if err := f.s.db.QueryRowContext(ctx, `SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&dbPath); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	f.cfg.Features[0] = revoked
	reopened, err := Open(ctx, dbPath, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got, err := reopened.BindContextualHumanAnswer(ctx, in, "deny", 2, "недостаточно данных"); err != nil || got != second {
		t.Fatalf("replay after restart=%q err=%v", got, err)
	}
	var retained int
	if err := reopened.db.QueryRowContext(ctx, `SELECT selector FROM human_request_selectors WHERE request_id=?`, second).Scan(&retained); err != nil || retained != 2 {
		t.Fatalf("retained selector=%d err=%v", retained, err)
	}
}
