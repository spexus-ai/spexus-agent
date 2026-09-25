package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This opt-in test uses the isolated Spexus/PostgreSQL provider fixture. It
// mutates that fixture with unique request IDs; it never runs in ordinary CI.
func TestHumanActualProviderContract(t *testing.T) {
	fixturePath := os.Getenv("SPEXUS_HR_PROVIDER_FIXTURE")
	if fixturePath == "" {
		t.Skip("set SPEXUS_HR_PROVIDER_FIXTURE for the isolated provider contract test")
	}
	var fixture struct {
		BackendHTTPSURL string `json:"backend_https_url"`
		CACertPath      string `json:"ca_cert_path"`
		TokenFilePath   string `json:"token_file_path"`
		TenantID        string `json:"tenant_id"`
		ProjectID       string `json:"project_id"`
		EpicID          string `json:"epic_id"`
		GatewayWriterID string `json:"gateway_writer_id"`
	}
	metadata, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(metadata, &fixture); err != nil {
		t.Fatal(err)
	}
	if !uuid(fixture.TenantID) || !uuid(fixture.ProjectID) || !uuid(fixture.EpicID) || !uuid(fixture.GatewayWriterID) {
		t.Fatal("invalid isolated provider fixture metadata")
	}
	featureID := NewID()
	const actor = "U_P3_HR_CONTRACT"
	const workspace = "T_P3_HR_CONTRACT"
	const channel = "C_P3_HR_CONTRACT"
	const thread = "1790000000.000001"
	cfg := Config{TenantID: fixture.TenantID, ProjectID: fixture.ProjectID, WireVersion: 2,
		Human:    &HumanConfig{BaseURL: fixture.BackendHTTPSURL, CAFile: fixture.CACertPath, TokenFile: fixture.TokenFilePath, EpicID: fixture.EpicID, WriterID: fixture.GatewayWriterID, WorkspaceID: workspace},
		Features: []Feature{{FeatureID: featureID, TenantID: fixture.TenantID, ProjectID: fixture.ProjectID, OwnerAgentID: "orchestrator", ChannelID: channel, ThreadTS: thread, AllowedActorIDs: []string{actor}}},
	}
	profileBytes := map[string][]byte{}
	for _, id := range []string{"orchestrator", "worker-a", "worker-b"} {
		role := "worker"
		if id == "orchestrator" {
			role = "owner"
		}
		cfg.Agents = append(cfg.Agents, AgentConfig{AgentID: id, Role: role, CredentialSHA256: Digest([]byte(NewID())), ProfileID: id})
		profileBytes[id] = mustJSON(TextProfile{ID: id, Model: "openai-codex/gpt-6-luna", Reasoning: "minimal", Prompt: "Return JSON", Tools: []string{}, Extensions: []string{}})
	}
	profileBackendFixture(t, &cfg, profileBytes)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err = s.HumanPreflight(ctx); err != nil {
		t.Fatalf("provider preflight: %v", err)
	}
	client, err := s.backendClient()
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetRecoveryBarrier(ctx, featureID, ""); err != nil {
		t.Fatal(err)
	}

	create := func(step string) (humanOperation, []byte) {
		t.Helper()
		requestID, dependencyID, turnID, sourceID := NewID(), NewID(), NewID(), NewID()
		body := mustJSON(map[string]any{
			"epic_id": fixture.EpicID, "feature_id": featureID, "owner_agent_id": "owner",
			"dependency": map[string]any{"id": dependencyID, "kind": "owner_step", "step_key": step, "origin_turn_id": turnID},
			"reason":     "Approval required", "context": "The planned work waits for an explicit decision", "question": "Approve the planned change?",
			"kind": "choice", "options": []HumanOption{{ID: "a", Label: "Approve"}, {ID: "b", Label: "Decline"}},
			"recommendation": "Approve the smaller change", "slack": map[string]string{"workspace_id": workspace, "channel_id": channel, "thread_ts": thread},
			"allowed_responders": []string{actor}, "source": map[string]string{"kind": "owner", "turn_id": turnID},
		})
		d := Dependency{ID: dependencyID, FeatureID: featureID, Kind: "owner_step", StepKey: step, OriginTurnID: turnID, SourceMessageID: sourceID, State: "human_pending", RequestID: requestID, CreatedAt: s.stamp(), UpdatedAt: s.stamp()}
		if _, err := s.db.ExecContext(ctx, "INSERT INTO dependencies(id,feature_id,job_id,source_message_id,state,data) VALUES(?,?,?,?,?,?)", d.ID, d.FeatureID, "", d.SourceMessageID, d.State, mustJSON(d)); err != nil {
			t.Fatal(err)
		}
		op := humanOperation{ID: requestID, RequestID: requestID, Kind: "create", Payload: body}
		if _, err := s.db.ExecContext(ctx, "INSERT INTO backend_sync_operations(operation_id,request_id,kind,status,payload) VALUES(?,?,?,?,?)", op.ID, op.RequestID, op.Kind, "inflight", op.Payload); err != nil {
			t.Fatal(err)
		}
		return op, body
	}
	read := func(id, state string) {
		t.Helper()
		status, body, err := s.backendRequest(ctx, client, http.MethodGet, "/api/v1/human-requests/"+id, nil)
		if err != nil || status != 200 {
			t.Fatalf("provider read: HTTP %d: %v", status, err)
		}
		var env humanEnvelope
		var view humanBackendView
		if json.Unmarshal(body, &env) != nil || env.SchemaVersion != 1 || json.Unmarshal(env.Data, &view) != nil || view.ID != id || view.State != state || view.TenantID != fixture.TenantID || view.ProjectID != fixture.ProjectID {
			t.Fatalf("provider read contract mismatch for %s", state)
		}
	}

	createOp, createBody := create("contract.answer")
	// The provider commits the create, but the transport drops its response.
	// The SQLite operation retains its exact ID and payload for a safe replay.
	uncertain := *client
	uncertain.Transport = &loseFirstCreateResponse{base: client.Transport}
	if err = s.syncHumanOperation(ctx, &uncertain, createOp); err != nil {
		t.Fatal(err)
	}
	var status string
	var stored []byte
	if err = s.db.QueryRowContext(ctx, "SELECT status,payload FROM backend_sync_operations WHERE operation_id=?", createOp.ID).Scan(&status, &stored); err != nil || status != "retry" || string(stored) != string(createBody) {
		t.Fatalf("uncertain create did not retain the exact operation: status=%s err=%v", status, err)
	}
	if err = s.syncHumanOperation(ctx, client, createOp); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRowContext(ctx, "SELECT status FROM backend_sync_operations WHERE operation_id=?", createOp.ID).Scan(&status); err != nil || status != "done" {
		t.Fatalf("create replay did not settle: status=%s err=%v", status, err)
	}
	if code, _, err := s.backendRequest(ctx, client, http.MethodPut, "/api/v1/human-requests/"+createOp.RequestID, createBody); err != nil || code != 200 {
		t.Fatalf("exact create replay: HTTP %d: %v", code, err)
	}
	read(createOp.RequestID, "open")

	messageTS := fmt.Sprintf("%d.%06d", time.Now().Unix(), time.Now().Nanosecond()/1000)
	decisionID, duplicate, err := s.RecordHumanAnswer(ctx, HumanAnswerInput{RequestID: createOp.RequestID, Kind: "answer", OptionID: "a", Text: "Approved", WorkspaceID: workspace, ChannelID: channel, ThreadTS: thread, MessageTS: messageTS, ActorID: actor, EventID: NewID()})
	if err != nil || duplicate || decisionID == "" {
		t.Fatalf("recorded answer: duplicate=%v err=%v", duplicate, err)
	}
	decisionOp, err := s.claimHumanOperation(ctx)
	if err != nil || decisionOp.ID != decisionID || decisionOp.Kind != "decision" {
		t.Fatalf("durable decision operation: id=%s err=%v", decisionOp.ID, err)
	}
	// The provider commits the decision while the response is lost. A service
	// restart must recover the same operation and defer local application until
	// Slack source catchup opens the barrier.
	uncertainDecision := *client
	uncertainDecision.Transport = &loseFirstDecisionResponse{base: client.Transport}
	if err = s.syncHumanOperation(ctx, &uncertainDecision, decisionOp); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRowContext(ctx, "SELECT status FROM backend_sync_operations WHERE operation_id=?", decisionOp.ID).Scan(&status); err != nil || status != "retry" {
		t.Fatalf("lost decision response did not retain retry: status=%s err=%v", status, err)
	}
	read(createOp.RequestID, "answered")
	statePath := s.dbPath()
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, statePath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	client, err = s.backendClient()
	if err != nil {
		t.Fatal(err)
	}
	if err = s.syncHumanOperation(ctx, client, decisionOp); err != nil {
		t.Fatal(err)
	}
	var applications int
	if err = s.db.QueryRowContext(ctx, "SELECT count(*) FROM decision_applications WHERE request_id=?", createOp.RequestID).Scan(&applications); err != nil || applications != 0 {
		t.Fatalf("decision applied before source catchup: applications=%d err=%v", applications, err)
	}
	if err = s.SetRecoveryBarrier(ctx, featureID, ""); err != nil {
		t.Fatal(err)
	}
	if err = s.ApplyPendingHuman(ctx); err != nil {
		t.Fatal(err)
	}
	var applicationStatus string
	if err = s.db.QueryRowContext(ctx, "SELECT count(*),coalesce(max(status),'') FROM decision_applications WHERE request_id=?", createOp.RequestID).Scan(&applications, &applicationStatus); err != nil || applications != 1 || applicationStatus != "applied" {
		t.Fatalf("decision did not apply exactly once: applications=%d status=%s err=%v", applications, applicationStatus, err)
	}
	var ownerInputs int
	if err = s.db.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE kind='human.decision' AND feature_id=?", featureID).Scan(&ownerInputs); err != nil || ownerInputs != 1 {
		t.Fatalf("decision delivered %d owner inputs after recovery: %v", ownerInputs, err)
	}
	var reason string
	if err = s.db.QueryRowContext(ctx, "SELECT status,reason FROM backend_sync_operations WHERE operation_id=?", decisionOp.ID).Scan(&status, &reason); err != nil || status != "done" {
		t.Fatalf("decision operation did not settle: status=%s reason=%s err=%v", status, reason, err)
	}
	if code, body, err := s.backendRequest(ctx, client, http.MethodPost, "/api/v1/human-requests/"+createOp.RequestID+"/decision", decisionOp.Payload); err != nil || code != 200 {
		t.Fatalf("exact decision replay: HTTP %d body=%s err=%v", code, body, err)
	}
	read(createOp.RequestID, "answered")

	cancelOp, _ := create("contract.cancel")
	if err = s.syncHumanOperation(ctx, client, cancelOp); err != nil {
		t.Fatal(err)
	}
	read(cancelOp.RequestID, "open")
	if err = s.StopFeature(ctx, featureID, actor, "Fixture cancellation"); err != nil {
		t.Fatal(err)
	}
	cancelOperation, err := s.claimHumanOperation(ctx)
	if err != nil || cancelOperation.Kind != "cancel" || cancelOperation.RequestID != cancelOp.RequestID {
		t.Fatalf("durable cancel operation: kind=%s err=%v", cancelOperation.Kind, err)
	}
	if err = s.syncHumanOperation(ctx, client, cancelOperation); err != nil {
		t.Fatal(err)
	}
	if code, _, err := s.backendRequest(ctx, client, http.MethodPost, "/api/v1/human-requests/"+cancelOp.RequestID+"/cancel", cancelOperation.Payload); err != nil || code != 200 {
		t.Fatalf("exact cancel replay: HTTP %d: %v", code, err)
	}
	read(cancelOp.RequestID, "cancelled")
}

type loseFirstCreateResponse struct {
	base http.RoundTripper
	lost bool
}

type loseFirstDecisionResponse struct {
	base http.RoundTripper
	lost bool
}

func (t *loseFirstDecisionResponse) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err != nil || t.lost || r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/decision") {
		return resp, err
	}
	t.lost = true
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return nil, errors.New("simulated lost decision response after commit")
}

func (t *loseFirstCreateResponse) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err != nil || t.lost || r.Method != http.MethodPut {
		return resp, err
	}
	t.lost = true
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return nil, errors.New("simulated lost provider response after commit")
}
