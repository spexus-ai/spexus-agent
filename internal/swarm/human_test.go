package swarm

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newHumanFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, instances: map[string]string{}, tokens: map[string]string{}}
	tenant, project := NewID(), NewID()
	f.feature = Feature{FeatureID: NewID(), TenantID: tenant, ProjectID: project, OwnerAgentID: "orchestrator", ChannelID: "channel", ThreadTS: "123.4", AllowedActorIDs: []string{"human"}}
	path := filepath.Join(t.TempDir(), "gateway.json")
	if err := os.WriteFile(path, mustJSON(gatewayToken{Token: "test-access"}), 0600); err != nil {
		t.Fatal(err)
	}
	f.cfg = Config{TenantID: tenant, ProjectID: project, WireVersion: 2, Human: &HumanConfig{BaseURL: "https://example.invalid", TokenFile: path, EpicID: NewID(), WriterID: NewID(), WorkspaceID: "workspace"}, Features: []Feature{f.feature}}
	for _, id := range []string{"orchestrator", "worker-a", "worker-b"} {
		role := "worker"
		if id == "orchestrator" {
			role = "owner"
		}
		token := NewID() + NewID()
		f.tokens[id] = token
		f.instances[id] = NewID()
		f.cfg.Agents = append(f.cfg.Agents, AgentConfig{AgentID: id, Role: role, CredentialSHA256: Digest([]byte(token)), ProfileID: id})
		f.cfg.Profiles = append(f.cfg.Profiles, ProfileSnapshot{Bytes: mustJSON(TextProfile{ID: id, Model: "openai-codex/gpt-6-luna", Reasoning: "minimal", Prompt: "Return JSON", Tools: []string{}, Extensions: []string{}})})
	}
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.s = s
	f.server = httptest.NewTLSServer(s.Handler())
	f.server.TLS.MinVersion = tls.VersionTLS12
	t.Cleanup(func() { f.server.Close(); _ = s.Close() })
	for id := range f.tokens {
		f.call(id, "POST", "/agents/self/heartbeat", HeartbeatRequest{InstanceID: f.instances[id]}, 200)
	}
	if err = s.SetRecoveryBarrier(context.Background(), f.feature.FeatureID, ""); err != nil {
		t.Fatal(err)
	}
	return f
}
func humanBlocker() Blocker {
	return Blocker{Reason: "Cannot choose without approval", Context: "Candidate change affects public API", Question: "Which option should be used?", Options: []HumanOption{{ID: "a", Label: "Option A"}, {ID: "b", Label: "Option B"}}, Recommendation: "a, because it is smaller", Kind: "choice"}
}

// Test: blocked dependency requires the canonical human decision and allows B to finish.
// Validates: AC-463, AC-464, AC-465 and AC-450 (durable single continuation).
func TestHumanGateAndSingleContinuation(t *testing.T) {
	f := newHumanFixture(t)
	ctx := context.Background()
	turn := f.ownerTurn()
	a := f.dispatch(turn, "worker-a")
	a.ProtocolVersion = 2
	f.post("orchestrator", a, 201)
	accepted := f.event(a, "task.accepted", AcceptedPayload{DispatchMessageID: a.MessageID, ProfileRevision: f.s.profiles["worker-a"].Revision}, a.MessageID)
	accepted.ProtocolVersion = 2
	f.post("worker-a", accepted, 201)
	started := f.event(a, "task.started", StartedPayload{AcceptedMessageID: accepted.MessageID}, accepted.MessageID)
	started.ProtocolVersion = 2
	f.post("worker-a", started, 201)
	blocked := f.event(a, "task.result", ResultPayload{Outcome: "blocked", Summary: "Need owner decision", Evidence: []Evidence{}, Origin: "worker", Blocker: ptrBlocker(humanBlocker())}, started.MessageID)
	blocked.ProtocolVersion = 2
	resultReceipt := f.post("worker-a", blocked, 201)
	f.post("worker-a", blocked, 200)
	h := f.history()
	if len(h.Dependencies) != 1 || h.Jobs[0].Attempts[0].State != "blocked" {
		t.Fatalf("blocked transition not durable: %+v", h.Dependencies)
	}
	d := h.Dependencies[0]
	b := f.dispatch(turn, "worker-b")
	b.ProtocolVersion = 2
	f.post("orchestrator", b, 201)
	if len(f.history().Jobs) != 2 {
		t.Fatal("independent work was blocked")
	}
	bypass := f.dispatch(turn, "worker-a")
	bypass.ProtocolVersion = 2
	bypass.JobID = a.JobID
	f.post("orchestrator", bypass, 409)
	f.finish(turn, "", []ActionReceipt{{MessageID: a.MessageID, Status: "stored"}, {MessageID: b.MessageID, Status: "stored"}}, 201)
	f.call("orchestrator", "POST", "/acks", AckRequest{MailboxSeqs: []int64{resultReceipt.MailboxSeq}}, 200)
	ownerTurn := NewID()
	f.call("orchestrator", "POST", "/owner-turns/start", OwnerStartRequest{TurnID: ownerTurn, FeatureID: f.feature.FeatureID, InputMailboxSeq: resultReceipt.MailboxSeq}, 201)
	request := Envelope{ProtocolVersion: 2, MessageID: NewID(), Type: "human.request", TenantID: f.cfg.TenantID, ProjectID: f.cfg.ProjectID, FeatureID: f.feature.FeatureID, FromAgentID: "orchestrator", ToAgentID: "coordinator", OwnerTurnID: ownerTurn, SentAt: f.s.stamp(), Payload: mustJSON(HumanRequestPayload{DependencyID: d.ID, Blocker: humanBlocker()})}
	f.post("orchestrator", request, 201)
	select {
	case <-f.s.HumanWake():
	default:
		t.Fatal("durable human request did not wake backend sync")
	}
	f.post("orchestrator", request, 200)
	select {
	case <-f.s.HumanWake():
		t.Fatal("duplicate human request woke backend sync")
	default:
	}
	h = f.history()
	d = h.Dependencies[0]
	if d.State != "human_pending" || d.RequestID == "" {
		t.Fatalf("human request not durably pending: %+v", d)
	}
	resolve := request
	resolve.MessageID = NewID()
	resolve.Type = "dependency.resolve"
	resolve.Payload = mustJSON(ResolveDependencyPayload{DependencyID: d.ID, Resolution: "Skip approval", Evidence: []Evidence{}})
	f.post("orchestrator", resolve, 409)
	code := "dependency_not_resolvable"
	f.finish(ownerTurn, "", []ActionReceipt{{MessageID: request.MessageID, Status: "stored"}, {MessageID: resolve.MessageID, Status: "rejected", ErrorCode: &code}}, 201)
	open := humanBackendView{ID: d.RequestID, TenantID: f.cfg.TenantID, ProjectID: f.cfg.ProjectID, FeatureID: f.feature.FeatureID, State: "open", Revision: 1, AllowedResponders: []string{"human"}, Options: humanBlocker().Options}
	open.Dependency.ID = d.ID
	open.Slack.WorkspaceID = "workspace"
	open.Slack.ChannelID = f.feature.ChannelID
	open.Slack.ThreadTS = f.feature.ThreadTS
	if err := f.s.acceptHumanEnvelope(ctx, d.RequestID, mustJSON(humanEnvelope{SchemaVersion: 1, Data: mustJSON(open)})); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.s.SlackWake():
	default:
		t.Fatal("committed question did not wake Slack publication")
	}
	if len(f.history().SlackOutbox) != 1 || f.history().Dependencies[0].State != "human_waiting" {
		t.Fatal("question was not queued after provider create")
	}
	if !strings.Contains(f.history().SlackOutbox[0].Text, "Compute result") {
		t.Fatal("human question omitted blocked work")
	}
	answer := HumanAnswerInput{RequestID: d.RequestID, Kind: "answer", OptionID: "a", Text: "Approved", WorkspaceID: "workspace", ChannelID: f.feature.ChannelID, ThreadTS: f.feature.ThreadTS, MessageTS: "124.1", ActorID: "human", EventID: "evt"}
	op, dup, err := f.s.RecordHumanAnswer(ctx, answer)
	if err != nil || dup || op == "" {
		t.Fatalf("answer not accepted: %v", err)
	}
	op2, dup, err := f.s.RecordHumanAnswer(ctx, answer)
	if err != nil || !dup || op2 != op {
		t.Fatal("duplicate answer changed operation")
	}
	terminal := open
	terminal.State = "answered"
	terminal.Revision = 2
	decisionID := NewID()
	terminal.Terminal = &struct {
		ID       string          `json:"id"`
		Kind     string          `json:"kind"`
		Response json.RawMessage `json:"response"`
		Source   json.RawMessage `json:"source"`
	}{ID: decisionID, Kind: "answer", Response: mustJSON(map[string]any{"kind": "answer", "option_id": "a", "text": "Approved"}), Source: mustJSON(map[string]any{"actor_id": "human", "workspace_id": "workspace", "channel_id": "channel", "thread_ts": "123.4", "message_ts": "124.1"})}
	env := mustJSON(humanEnvelope{SchemaVersion: 1, Data: mustJSON(terminal)})
	if err = f.s.acceptHumanEnvelope(ctx, d.RequestID, env); err != nil {
		t.Fatal(err)
	}
	if err = f.s.acceptHumanEnvelope(ctx, d.RequestID, env); err != nil {
		t.Fatal(err)
	}
	h = f.history()
	d = h.Dependencies[0]
	if d.State != "resolved" || d.DecisionID != decisionID {
		t.Fatalf("decision not applied: %+v", d)
	}
	decisionMessages := 0
	var trigger Delivery
	for _, m := range h.Messages {
		if m.Type == "human.decision" {
			decisionMessages++
			trigger = m
		}
	}
	if decisionMessages != 1 {
		t.Fatalf("expected one owner input, got %d", decisionMessages)
	}
	decisionTurn := NewID()
	f.call("orchestrator", "POST", "/owner-turns/start", OwnerStartRequest{TurnID: decisionTurn, FeatureID: f.feature.FeatureID, InputMailboxSeq: trigger.MailboxSeq}, 201)
	var dispatch DispatchPayload
	if err = json.Unmarshal(a.Payload, &dispatch); err != nil {
		t.Fatal(err)
	}
	dispatch.AcceptBy = time.Now().UTC().Add(60 * time.Second).Format(time.RFC3339Nano)
	resume := Envelope{ProtocolVersion: 2, MessageID: NewID(), Type: "task.resume", TenantID: a.TenantID, ProjectID: a.ProjectID, FeatureID: a.FeatureID, FromAgentID: "orchestrator", ToAgentID: "worker-a", OwnerTurnID: decisionTurn, JobID: a.JobID, AttemptID: NewID(), SentAt: f.s.stamp(), Payload: mustJSON(ResumeTaskPayload{DependencyID: d.ID, DecisionID: decisionID, WorkerAgentID: "worker-a", Dispatch: dispatch})}
	first := f.post("orchestrator", resume, 201)
	if again := f.post("orchestrator", resume, 200); again != first {
		t.Fatal("resume receipt changed")
	}
	h = f.history()
	if h.Jobs[0].CurrentAttemptID != resume.AttemptID || len(h.Jobs[0].Attempts) != 2 || h.Dependencies[0].State != "continuation_scheduled" {
		t.Fatal("new attempt was not uniquely scheduled")
	}
	other := resume
	other.MessageID = NewID()
	other.AttemptID = NewID()
	f.post("orchestrator", other, 409)
}
func ptrBlocker(b Blocker) *Blocker { return &b }

// Test: the owner can resolve dependency identity without exposing it to a worker or stale instance.
// Validates: SP-STD-022 §5 trusted identity for resume_task and P2 instance binding.
func TestDependencyLookupOwnerScopeAndInstance(t *testing.T) {
	f := newHumanFixture(t)
	ctx := context.Background()
	d := Dependency{ID: NewID(), FeatureID: f.feature.FeatureID, Kind: "job", JobID: NewID(), AttemptID: NewID(), SourceMessageID: NewID(), State: "owner_resolution", Blocker: humanBlocker(), CreatedAt: f.s.stamp(), UpdatedAt: f.s.stamp()}
	if err := f.s.transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO dependencies(id,feature_id,job_id,source_message_id,state,data) VALUES(?,?,?,?,?,?)", d.ID, d.FeatureID, d.JobID, d.SourceMessageID, d.State, mustJSON(d))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var got Dependency
	if err := json.Unmarshal(f.call("orchestrator", "GET", "/dependencies/"+d.ID, nil, 200), &got); err != nil {
		t.Fatal(err)
	}
	if got.JobID != d.JobID || got.AttemptID != d.AttemptID || got.State != d.State {
		t.Fatal("owner did not receive trusted dependency identity")
	}
	f.call("worker-a", "GET", "/dependencies/"+d.ID, nil, 404)
	f.call("orchestrator", "GET", "/dependencies/"+NewID(), nil, 404)
	old := f.instances["orchestrator"]
	f.instances["orchestrator"] = NewID()
	f.call("orchestrator", "GET", "/dependencies/"+d.ID, nil, 409)
	f.instances["orchestrator"] = old
}

// Test: a stopped feature tombstones the dependency even after !continue.
// Validates: AC-449 and AC-465 (late/denied continuation never resumes).
func TestHumanStopTombstone(t *testing.T) {
	f := newHumanFixture(t)
	ctx := context.Background()
	d := Dependency{ID: NewID(), FeatureID: f.feature.FeatureID, Kind: "owner_step", StepKey: "approval", OriginTurnID: NewID(), SourceMessageID: NewID(), State: "human_waiting", RequestID: NewID(), Blocker: humanBlocker(), CreatedAt: f.s.stamp(), UpdatedAt: f.s.stamp()}
	err := f.s.transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO dependencies(id,feature_id,job_id,source_message_id,state,data) VALUES(?,?,?,?,?,?)", d.ID, d.FeatureID, "", d.SourceMessageID, d.State, mustJSON(d))
		if err != nil {
			return err
		}
		q := SlackDelivery{ID: d.RequestID, FeatureID: d.FeatureID, ChannelID: f.feature.ChannelID, ThreadTS: f.feature.ThreadTS, Text: questionText(d), Status: "queued"}
		_, err = tx.ExecContext(ctx, "INSERT INTO slack_outbox(id,feature_id,turn_id,status,data) VALUES(?,?,NULL,?,?)", q.ID, q.FeatureID, q.Status, mustJSON(q))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.s.StopFeature(ctx, f.feature.FeatureID, "human", "Stop feature"); err != nil {
		t.Fatal(err)
	}
	if err = f.s.ContinueFeature(ctx, f.feature.FeatureID, "human"); err != nil {
		t.Fatal(err)
	}
	if f.history().Dependencies[0].State != "cancelled" {
		t.Fatal("continue reopened cancelled dependency")
	}
	if f.history().SlackOutbox[0].Status != "suppressed" {
		t.Fatal("stop left cancelled question queued")
	}
	if next, err := f.s.ClaimSlack(ctx); err != nil || next != nil {
		t.Fatal("cancelled question was claimable")
	}
}

// Test: a terminal answer needs both the request snapshot and current allowlist.
// Validates: SP-STD-022 §8 responder intersection and safe allowlist rotation.
func TestHumanAnswerSnapshotAndCurrentAllowlist(t *testing.T) {
	for _, tc := range []struct {
		name              string
		snapshot, current []string
	}{
		{"revoked_current", []string{"human"}, []string{"other"}},
		{"outside_snapshot", []string{"other"}, []string{"human"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHumanFixture(t)
			ctx := context.Background()
			d := Dependency{ID: NewID(), FeatureID: f.feature.FeatureID, Kind: "owner_step", StepKey: "approval", OriginTurnID: NewID(), SourceMessageID: NewID(), State: "human_waiting", RequestID: NewID(), Blocker: humanBlocker(), CreatedAt: f.s.stamp(), UpdatedAt: f.s.stamp()}
			if err := f.s.transaction(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, "INSERT INTO dependencies(id,feature_id,job_id,source_message_id,state,data) VALUES(?,?,?,?,?,?)", d.ID, d.FeatureID, "", d.SourceMessageID, d.State, mustJSON(d))
				return err
			}); err != nil {
				t.Fatal(err)
			}
			updated := f.feature
			updated.AllowedActorIDs = tc.current
			if err := f.s.RegisterFeature(ctx, updated); err != nil {
				t.Fatal(err)
			}
			v := humanBackendView{ID: d.RequestID, TenantID: f.cfg.TenantID, ProjectID: f.cfg.ProjectID, FeatureID: d.FeatureID, AllowedResponders: tc.snapshot, State: "answered", Revision: 2}
			v.Dependency.ID = d.ID
			v.Terminal = &struct {
				ID       string          `json:"id"`
				Kind     string          `json:"kind"`
				Response json.RawMessage `json:"response"`
				Source   json.RawMessage `json:"source"`
			}{ID: NewID(), Kind: "answer", Response: mustJSON(map[string]any{"kind": "answer", "option_id": "a", "text": ""}), Source: mustJSON(map[string]string{"actor_id": "human"})}
			if err := f.s.acceptHumanEnvelope(ctx, d.RequestID, mustJSON(humanEnvelope{SchemaVersion: 1, Data: mustJSON(v)})); err != nil {
				t.Fatal(err)
			}
			h := f.history()
			if h.Dependencies[0].State != "cancelled" || h.HumanRequests[0].ApplicationStatus != "suppressed" {
				t.Fatal("answer bypassed responder intersection")
			}
			for _, m := range h.Messages {
				if m.Type == "human.decision" {
					t.Fatal("suppressed answer created owner input")
				}
			}
		})
	}
}

// Test: a different gateway JWT is rejected before any backend mutation.
// Validates: SP-STD-022 §8 writer pin across credential replacement.
func TestGatewayWriterPinned(t *testing.T) {
	f := newHumanFixture(t)
	ctx := context.Background()
	var pinned string
	if err := f.s.db.QueryRowContext(ctx, "SELECT writer_id FROM human_gateway_writer WHERE id=1").Scan(&pinned); err != nil || pinned != f.cfg.Human.WriterID {
		t.Fatal("writer not pinned in state")
	}
	requests := 0
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; w.WriteHeader(200) }))
	defer provider.Close()
	ca := filepath.Join(t.TempDir(), "provider.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: provider.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	f.s.cfg.Human.BaseURL = provider.URL
	f.s.cfg.Human.CAFile = ca
	if err := writeGatewayToken(f.s.cfg.Human.TokenFile, gatewayToken{Token: testJWT(NewID())}); err != nil {
		t.Fatal(err)
	}
	client, err := f.s.backendClient()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = f.s.backendRequest(ctx, client, http.MethodPut, "/api/v1/human-requests/"+NewID(), mustJSON(map[string]string{"probe": "x"}))
	if !errors.Is(err, errGatewayAuth) || requests != 0 {
		t.Fatal("swapped writer reached backend")
	}
	original := f.s.cfg.Human.WriterID
	f.s.cfg.Human.WriterID = NewID()
	if err := f.s.bootstrap(ctx); err == nil {
		t.Fatal("state accepted changed gateway writer")
	}
	f.s.cfg.Human.WriterID = original
}

// Test: an unknown refresh response is durable and never blindly retried.
// Validates: SP-STD-022 §8 (revoke-before-response gateway credential safety).
func TestGatewayRefreshUnknownFailsClosed(t *testing.T) {
	f := newHumanFixture(t)
	refreshCalls := 0
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/refresh" {
			refreshCalls++
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(401)
	}))
	defer provider.Close()
	ca := filepath.Join(t.TempDir(), "provider.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: provider.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	f.s.cfg.Human.BaseURL = provider.URL
	f.s.cfg.Human.CAFile = ca
	if err := writeGatewayToken(f.s.cfg.Human.TokenFile, gatewayToken{Token: testJWT(f.s.cfg.Human.WriterID), RefreshToken: "one-use"}); err != nil {
		t.Fatal(err)
	}
	client, err := f.s.backendClient()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = f.s.backendRequest(context.Background(), client, http.MethodGet, "/api/v1/human-requests", nil)
	if !errors.Is(err, errGatewayAuth) || refreshCalls != 1 {
		t.Fatalf("unexpected refresh result: calls=%d err=%v", refreshCalls, err)
	}
	token, err := readGatewayToken(f.s.cfg.Human.TokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if token.RefreshState != "unknown" {
		t.Fatal("refresh uncertainty was not persisted")
	}
	_, _, err = f.s.backendRequest(context.Background(), client, http.MethodGet, "/api/v1/human-requests", nil)
	if !errors.Is(err, errGatewayAuth) || refreshCalls != 1 {
		t.Fatal("unknown refresh was retried")
	}
}

func TestHumanRetryAfter(t *testing.T) {
	f := newHumanFixture(t)
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer provider.Close()
	ca := filepath.Join(t.TempDir(), "provider.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: provider.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	f.s.cfg.Human.BaseURL = provider.URL
	f.s.cfg.Human.CAFile = ca
	if err := writeGatewayToken(f.s.cfg.Human.TokenFile, gatewayToken{Token: testJWT(f.s.cfg.Human.WriterID)}); err != nil {
		t.Fatal(err)
	}
	client, err := f.s.backendClient()
	if err != nil {
		t.Fatal(err)
	}
	op := humanOperation{ID: NewID(), RequestID: NewID(), Kind: "create", Payload: []byte("{}")}
	if _, err = f.s.db.Exec("INSERT INTO backend_sync_operations(operation_id,request_id,kind,status,payload) VALUES(?,?,?,?,?)", op.ID, op.RequestID, op.Kind, "inflight", op.Payload); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	if err = f.s.syncHumanOperation(context.Background(), client, op); err != nil {
		t.Fatal(err)
	}
	var status, next string
	if err = f.s.db.QueryRow("SELECT status,next_at FROM backend_sync_operations WHERE operation_id=?", op.ID).Scan(&status, &next); err != nil {
		t.Fatal(err)
	}
	readyAt, err := time.Parse(time.RFC3339Nano, next)
	if err != nil {
		t.Fatal(err)
	}
	if status != "retry" || readyAt.Before(before.Add(17*time.Second)) {
		t.Fatalf("Retry-After ignored: status=%s next=%s", status, next)
	}
}
func testJWT(writer string) string {
	return "e30." + base64.RawURLEncoding.EncodeToString(mustJSON(map[string]string{"user_id": writer})) + ".signature"
}
