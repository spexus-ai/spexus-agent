package swarm

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// Test: isolated fixture source replays and stopped-input release.
// Validates: SP-EP-026 W06 preview evidence for stable source identity and stop/continue.
func TestLocalFixtureReplayStopBufferAndContinue(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	input := func(id, text string) InputPayload {
		return InputPayload{Text: text, Source: Source{Kind: "test", EventID: id, ChannelID: f.feature.ChannelID, ThreadTS: f.feature.ThreadTS, ActorID: "human"}}
	}
	first, err := f.s.InjectLocalFixture(ctx, f.feature.FeatureID, input("source-one", "First request"))
	if err != nil || first.Status != "delivered" || first.Duplicate || first.MessageID == "" || first.MailboxSeq == 0 {
		t.Fatalf("first input: %+v %v", first, err)
	}
	replayed, err := f.s.InjectLocalFixture(ctx, f.feature.FeatureID, input("source-one", "First request"))
	if err != nil || !replayed.Duplicate || replayed.MessageID != first.MessageID || replayed.MailboxSeq != first.MailboxSeq {
		t.Fatalf("input replay changed receipt: %+v %v", replayed, err)
	}
	if _, err := f.s.InjectLocalFixture(ctx, f.feature.FeatureID, input("source-one", "Changed request")); err == nil {
		t.Fatal("same source accepted different text")
	}
	stopped, err := f.s.ControlLocalFixture(ctx, f.feature.FeatureID, "stop-one", "stop", "human")
	if err != nil || stopped.Status != "stopped" || stopped.Duplicate {
		t.Fatalf("stop: %+v %v", stopped, err)
	}
	stoppedAgain, err := f.s.ControlLocalFixture(ctx, f.feature.FeatureID, "stop-one", "stop", "human")
	if err != nil || !stoppedAgain.Duplicate {
		t.Fatalf("stop replay: %+v %v", stoppedAgain, err)
	}
	buffered, err := f.s.InjectLocalFixture(ctx, f.feature.FeatureID, input("source-two", "Buffered request"))
	if err != nil || buffered.Status != "buffered" || buffered.MessageID != "" {
		t.Fatalf("stopped input was delivered: %+v %v", buffered, err)
	}
	var before int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM ingress WHERE feature_id=? AND event_id='source-two'`, f.feature.FeatureID).Scan(&before); err != nil || before != 0 {
		t.Fatalf("buffer entered owner mailbox: %d %v", before, err)
	}
	continued, err := f.s.ControlLocalFixture(ctx, f.feature.FeatureID, "continue-one", "continue", "human")
	if err != nil || continued.Status != "continued" || len(continued.Released) != 1 {
		t.Fatalf("continue did not release exactly once: %+v %v", continued, err)
	}
	continuedAgain, err := f.s.ControlLocalFixture(ctx, f.feature.FeatureID, "continue-one", "continue", "human")
	if err != nil || !continuedAgain.Duplicate || len(continuedAgain.Released) != 0 {
		t.Fatalf("continue replay redelivered input: %+v %v", continuedAgain, err)
	}
	replayedBuffered, err := f.s.InjectLocalFixture(ctx, f.feature.FeatureID, input("source-two", "Buffered request"))
	if err != nil || !replayedBuffered.Duplicate || replayedBuffered.Status != "delivered" || replayedBuffered.MessageID != continued.Released[0].MessageID {
		t.Fatalf("released source changed receipt: %+v %v", replayedBuffered, err)
	}
	var after int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM ingress WHERE feature_id=? AND event_id='source-two'`, f.feature.FeatureID).Scan(&after); err != nil || after != 1 {
		t.Fatalf("released source was not unique: %d %v", after, err)
	}
}

// Test: a fixture decision cannot invent a human request or use wire v1.
// Validates: SP-EP-026 W06 human preview remains gated by a genuine wire-v2 request.
func TestLocalFixtureDecisionRequiresRealWireTwoRequest(t *testing.T) {
	ctx := context.Background()
	plain := newFixture(t)
	if _, err := plain.s.DecideLocalFixture(ctx, plain.feature.FeatureID, NewID(), "125.000001", "answer", "Approved", "", "human"); err == nil {
		t.Fatal("wire-v1 fixture accepted a human decision")
	}
	human := newHumanFixture(t)
	if _, err := human.s.DecideLocalFixture(ctx, human.feature.FeatureID, NewID(), "125.000001", "answer", "Approved", "", "human"); err == nil {
		t.Fatal("fixture accepted a decision without a real pending human request")
	}
	var decisions int
	if err := human.s.db.QueryRow(`SELECT count(*) FROM backend_sync_operations WHERE kind='decision'`).Scan(&decisions); err != nil || decisions != 0 {
		t.Fatalf("rejected decision left an operation: %d %v", decisions, err)
	}
}

// Test: a trusted synthetic source uses the existing wire-v2 backend decision path.
// Validates: SP-EP-026 W06 one decision and one owner continuation on source replay.
func TestLocalFixtureDecisionReplaysOneBackendOperation(t *testing.T) {
	f := newHumanFixture(t)
	ctx := context.Background()
	requestID := NewID()
	d := Dependency{ID: NewID(), FeatureID: f.feature.FeatureID, Kind: "owner_step", StepKey: "fixture.approval", OriginTurnID: NewID(), SourceMessageID: NewID(), State: "human_pending", RequestID: requestID, Blocker: humanBlocker(), CreatedAt: f.s.stamp(), UpdatedAt: f.s.stamp()}
	if _, err := f.s.db.ExecContext(ctx, `INSERT INTO dependencies(id,feature_id,job_id,source_message_id,state,data) VALUES(?,?,?,?,?,?)`, d.ID, d.FeatureID, "", d.SourceMessageID, d.State, mustJSON(d)); err != nil {
		t.Fatal(err)
	}
	open := humanBackendView{ID: requestID, TenantID: f.cfg.TenantID, ProjectID: f.cfg.ProjectID, FeatureID: f.feature.FeatureID, AllowedResponders: []string{"human"}, Options: humanBlocker().Options, State: "open", Revision: 1}
	open.Dependency.ID = d.ID
	open.Slack.WorkspaceID, open.Slack.ChannelID, open.Slack.ThreadTS = "workspace", f.feature.ChannelID, f.feature.ThreadTS
	if err := f.s.acceptHumanEnvelope(ctx, requestID, mustJSON(humanEnvelope{SchemaVersion: 1, Data: mustJSON(open)})); err != nil {
		t.Fatal(err)
	}
	terminal := open
	terminal.State, terminal.Revision = "answered", 2
	terminal.Terminal = &struct {
		ID       string          `json:"id"`
		Kind     string          `json:"kind"`
		Response json.RawMessage `json:"response"`
		Source   json.RawMessage `json:"source"`
	}{ID: NewID(), Kind: "answer", Response: mustJSON(map[string]string{"kind": "answer", "option_id": "a", "text": "Approved"}), Source: mustJSON(map[string]string{"actor_id": "human", "workspace_id": "workspace", "channel_id": f.feature.ChannelID, "thread_ts": f.feature.ThreadTS, "message_ts": "125.000001"})}
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/human-requests/"+requestID+"/decision" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustJSON(humanEnvelope{SchemaVersion: 1, Data: mustJSON(terminal)}))
	}))
	defer backend.Close()
	certificate, err := x509.ParseCertificate(backend.Certificate().Raw)
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(t.TempDir(), "backend-ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	f.s.cfg.Human.BaseURL, f.s.cfg.Human.CAFile = backend.URL, caPath
	token := "e30." + base64.RawURLEncoding.EncodeToString(mustJSON(map[string]string{"user_id": f.s.cfg.Human.WriterID})) + ".sig"
	if err := os.WriteFile(f.s.cfg.Human.TokenFile, mustJSON(gatewayToken{Token: token}), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := f.s.DecideLocalFixture(ctx, f.feature.FeatureID, requestID, "125.000001", "answer", "Approved", "a", "human")
	if err != nil || first.Duplicate || first.OperationID == "" || first.BackendSyncStatus != "done" || first.ApplicationStatus != "applied" {
		t.Fatalf("first decision: %+v %v", first, err)
	}
	replayed, err := f.s.DecideLocalFixture(ctx, f.feature.FeatureID, requestID, "125.000001", "answer", "Approved", "a", "human")
	if err != nil || !replayed.Duplicate || replayed.OperationID != first.OperationID || replayed.ApplicationStatus != "applied" {
		t.Fatalf("decision replay: %+v %v", replayed, err)
	}
	var operations, ownerInputs int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM backend_sync_operations WHERE kind='decision'`).Scan(&operations); err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.QueryRow(`SELECT count(*) FROM messages WHERE kind='human.decision'`).Scan(&ownerInputs); err != nil {
		t.Fatal(err)
	}
	if operations != 1 || ownerInputs != 1 {
		t.Fatalf("source replay duplicated work: operations=%d owner_inputs=%d", operations, ownerInputs)
	}
}
