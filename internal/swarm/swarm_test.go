package swarm

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	t         *testing.T
	s         *Store
	server    *httptest.Server
	cfg       Config
	feature   Feature
	instances map[string]string
	tokens    map[string]string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	f := &fixture{t: t, instances: map[string]string{}, tokens: map[string]string{}}
	tenant, project := NewID(), NewID()
	f.feature = Feature{FeatureID: NewID(), TenantID: tenant, ProjectID: project, OwnerAgentID: "orchestrator", ChannelID: "channel", ThreadTS: "123.4", AllowedActorIDs: []string{"human"}}
	f.cfg = Config{TenantID: tenant, ProjectID: project, Features: []Feature{f.feature}}
	for _, id := range []string{"orchestrator", "worker-a", "worker-b"} {
		role := "worker"
		if id == "orchestrator" {
			role = "owner"
		}
		token := NewID() + NewID()
		f.tokens[id] = token
		f.instances[id] = NewID()
		f.cfg.Agents = append(f.cfg.Agents, AgentConfig{AgentID: id, Role: role, CredentialSHA256: Digest([]byte(token)), ProfileID: id})
		f.cfg.Profiles = append(f.cfg.Profiles, ProfileSnapshot{Bytes: mustJSON(TextProfile{ID: id, Model: "openai-codex/gpt-6-luna", Reasoning: "minimal", Prompt: "Return only JSON", Tools: []string{}, Extensions: []string{}})})
	}
	s, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"), f.cfg)
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
	return f
}
func (f *fixture) call(agent, method, path string, v any, status int) []byte {
	f.t.Helper()
	var body io.Reader
	if v != nil {
		body = bytes.NewReader(mustJSON(v))
	}
	req, err := http.NewRequest(method, f.server.URL+APIPrefix+path, body)
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.tokens[agent])
	req.Header.Set("X-Agent-Instance-ID", f.instances[agent])
	req.Header.Set("Content-Type", "application/json")
	res, err := f.server.Client().Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	if res.StatusCode != status {
		f.t.Fatalf("%s %s got %d want %d: %s", method, path, res.StatusCode, status, b)
	}
	return b
}
func (f *fixture) post(agent string, e Envelope, status int) Receipt {
	f.t.Helper()
	b := f.call(agent, "POST", "/messages", e, status)
	var r Receipt
	_ = json.Unmarshal(b, &r)
	return r
}
func (f *fixture) history() History {
	f.t.Helper()
	h, err := f.s.History(context.Background(), f.feature.FeatureID)
	if err != nil {
		f.t.Fatal(err)
	}
	return h
}
func (f *fixture) ownerTurn() string {
	f.t.Helper()
	r, _, err := f.s.Ingest(context.Background(), f.feature.FeatureID, InputPayload{Text: "Delegate two parts", Source: Source{Kind: "slack", EventID: NewID(), ChannelID: f.feature.ChannelID, ThreadTS: f.feature.ThreadTS, ActorID: "human"}})
	if err != nil {
		f.t.Fatal(err)
	}
	id := NewID()
	f.call("orchestrator", "POST", "/acks", AckRequest{[]int64{r.MailboxSeq}}, 200)
	f.call("orchestrator", "POST", "/owner-turns/start", OwnerStartRequest{id, f.feature.FeatureID, r.MailboxSeq}, 201)
	return id
}
func (f *fixture) dispatch(turn, worker string) Envelope {
	f.t.Helper()
	return Envelope{ProtocolVersion: 1, MessageID: NewID(), Type: "task.dispatch", TenantID: f.cfg.TenantID, ProjectID: f.cfg.ProjectID, FeatureID: f.feature.FeatureID, FromAgentID: "orchestrator", ToAgentID: worker, OwnerTurnID: turn, JobID: NewID(), AttemptID: NewID(), SentAt: f.s.stamp(), Payload: mustJSON(DispatchPayload{Goal: "Compute result", Scope: "Only supplied text", ExpectedResult: []string{"Correct sum"}, Context: TaskContext{Text: worker + "-secret", Refs: []ContextRef{}}, Profile: f.s.profiles[worker], AcceptBy: f.s.now().UTC().Add(60 * time.Second).Format(time.RFC3339Nano), RunTimeoutSeconds: 600})}
}
func (f *fixture) event(d Envelope, kind string, payload any, causeID string) Envelope {
	f.t.Helper()
	return Envelope{ProtocolVersion: 1, MessageID: NewID(), Type: kind, TenantID: d.TenantID, ProjectID: d.ProjectID, FeatureID: d.FeatureID, FromAgentID: d.ToAgentID, ToAgentID: "orchestrator", JobID: d.JobID, AttemptID: d.AttemptID, CausationID: cause(causeID), SentAt: f.s.stamp(), Payload: mustJSON(payload)}
}
func (f *fixture) started(d Envelope) (Envelope, Envelope) {
	f.t.Helper()
	a := f.event(d, "task.accepted", AcceptedPayload{d.MessageID, f.s.profiles[d.ToAgentID].Revision}, d.MessageID)
	f.post(d.ToAgentID, a, 201)
	start := f.event(d, "task.started", StartedPayload{a.MessageID}, a.MessageID)
	f.post(d.ToAgentID, start, 201)
	return a, start
}
func resultPayload() ResultPayload {
	return ResultPayload{Outcome: "succeeded", Summary: "Result: 42", Evidence: []Evidence{{Kind: "text", Label: "Arithmetic", ContentOrRef: "40+2=42"}}, Origin: "worker"}
}
func (f *fixture) finish(id, reply string, actions []ActionReceipt, status int) {
	f.t.Helper()
	f.call("orchestrator", "POST", "/owner-turns/"+id+"/finish", OwnerFinishRequest{Outcome: "succeeded", Reply: reply, Actions: actions, Observation: json.RawMessage("null")}, status)
}

func TestHTTPSLifecycleTwoWorkersAndDurableHistory(t *testing.T) {
	f := newFixture(t)
	turn := f.ownerTurn()
	a, b := f.dispatch(turn, "worker-a"), f.dispatch(turn, "worker-b")
	ra := f.post("orchestrator", a, 201)
	f.post("orchestrator", b, 201)
	if again := f.post("orchestrator", a, 200); again != ra {
		t.Fatal("duplicate receipt changed")
	}
	duplicate := a
	duplicate.MessageID = NewID()
	if again := f.post("orchestrator", duplicate, 200); again != ra {
		t.Fatal("business duplicate receipt changed")
	}
	_, as := f.started(a)
	_, bs := f.started(b)
	ar := f.event(a, "task.result", resultPayload(), as.MessageID)
	br := f.event(b, "task.result", resultPayload(), bs.MessageID)
	// Equal message UUIDs from different sender principals must have different mailbox sequences.
	br.MessageID = ar.MessageID
	arr := f.post("worker-a", ar, 201)
	brr := f.post("worker-b", br, 201)
	if arr.MailboxSeq == brr.MailboxSeq {
		t.Fatal("mailbox sequence collision")
	}
	f.post("worker-a", ar, 200)
	alias := ar
	alias.MessageID = NewID()
	f.post("worker-a", alias, 200)
	f.call("orchestrator", "POST", "/acks", AckRequest{[]int64{arr.MailboxSeq, brr.MailboxSeq}}, 200)
	f.call("orchestrator", "POST", "/acks", AckRequest{[]int64{arr.MailboxSeq, brr.MailboxSeq}}, 200)
	review := Envelope{ProtocolVersion: 1, MessageID: NewID(), Type: "task.review", TenantID: a.TenantID, ProjectID: a.ProjectID, FeatureID: a.FeatureID, FromAgentID: "orchestrator", ToAgentID: "worker-a", OwnerTurnID: turn, JobID: a.JobID, AttemptID: a.AttemptID, CausationID: cause(ar.MessageID), SentAt: f.s.stamp(), Payload: mustJSON(ReviewPayload{ResultMessageID: ar.MessageID, Verdict: "accepted", Reason: "Evidence checked", Evidence: []Evidence{}})}
	f.post("orchestrator", review, 201)
	actions := []ActionReceipt{{MessageID: a.MessageID, Status: "stored"}, {MessageID: b.MessageID, Status: "stored"}, {MessageID: review.MessageID, Status: "stored"}, {MessageID: duplicate.MessageID, Status: "stored"}}
	f.finish(turn, "Both results verified in original thread", actions, 201)
	f.finish(turn, "Both results verified in original thread", actions, 200)
	h := f.history()
	if len(h.Jobs) != 2 || len(h.Turns) != 1 || len(h.SlackOutbox) != 1 || h.Feature.OwnerAgentID != "orchestrator" {
		t.Fatalf("bad history: %+v", h)
	}
	for _, job := range h.Jobs {
		if len(job.Attempts) != 1 || job.Attempts[0].State != "succeeded" || job.Attempts[0].Result == nil {
			t.Fatal("outcomes not durable")
		}
	}
	if len(h.Messages) != 10 {
		t.Fatalf("duplicate generated delivery: %d", len(h.Messages))
	}
	live, err := ReadHistory(context.Background(), f.s.dbPath(), f.feature.FeatureID)
	if err != nil || len(live.Jobs) != 2 {
		t.Fatalf("coherent live history: %v", err)
	}
	delivery, err := f.s.ClaimSlack(context.Background())
	if err != nil || delivery.ThreadTS != f.feature.ThreadTS {
		t.Fatalf("wrong anchor: %v", err)
	}
	if err = f.s.SettleSlack(context.Background(), delivery.ID, "delivery_unknown", ""); err != nil {
		t.Fatal(err)
	}
	if again, err := f.s.ClaimSlack(context.Background()); err != nil || again != nil {
		t.Fatal("unknown delivery replayed")
	}
	if err = f.s.SettleSlack(context.Background(), delivery.ID, "sent", "123.5"); err != nil {
		t.Fatal(err)
	}
	f.finish(turn, "Both results verified in original thread", actions, 200)

	if err = f.s.SettleSlack(context.Background(), delivery.ID, "queued", ""); err == nil {
		t.Fatal("sent delivery replay allowed")
	}
}
func (s *Store) dbPath() string {
	var seq int
	var name, path string
	_ = s.db.QueryRow("PRAGMA database_list").Scan(&seq, &name, &path)
	return path
}

func TestAuthorizationStaleAttemptsAndBoundInstances(t *testing.T) {
	f := newFixture(t)
	turn := f.ownerTurn()
	d := f.dispatch(turn, "worker-a")
	spoof := d
	spoof.FromAgentID = "worker-b"
	f.post("orchestrator", spoof, 403)
	foreign := d
	foreign.ProjectID = NewID()
	f.post("orchestrator", foreign, 403)
	unavailable := d
	var p DispatchPayload
	_ = json.Unmarshal(d.Payload, &p)
	p.Profile.Revision = strings.Repeat("0", 64)
	unavailable.Payload = mustJSON(p)
	f.post("orchestrator", unavailable, 422)
	f.post("orchestrator", d, 201)
	_, started := f.started(d)
	result := f.event(d, "task.result", resultPayload(), started.MessageID)
	result.FromAgentID = "worker-b"
	f.post("worker-b", result, 403)
	result.FromAgentID = "worker-a"
	payload := resultPayload()
	payload.Origin = "coordinator"
	result.Payload = mustJSON(payload)
	f.post("worker-a", result, 403)
	result.Payload = mustJSON(resultPayload())
	f.post("worker-a", result, 201)
	next := f.dispatch(turn, "worker-b")
	next.JobID = d.JobID
	f.post("orchestrator", next, 201)
	stale := result
	stale.MessageID = NewID()
	f.post("worker-a", stale, 409)
	f.call("worker-b", "GET", "/jobs/"+d.JobID, nil, 200)
	// Another worker must not see a job never assigned to it.
	separate := f.dispatch(turn, "worker-a")
	f.post("orchestrator", separate, 201)
	f.call("worker-b", "GET", "/jobs/"+separate.JobID, nil, 404)
	original := f.instances["worker-a"]
	f.instances["worker-a"] = NewID()
	f.call("worker-a", "POST", "/agents/self/heartbeat", HeartbeatRequest{InstanceID: f.instances["worker-a"]}, 409)
	f.call("worker-a", "GET", "/mailbox?wait_seconds=0", nil, 409)
	f.instances["worker-a"] = original
	h := f.history()
	if len(h.Jobs) != 2 || h.Jobs[0].CurrentAttemptID != next.AttemptID || h.Jobs[0].Attempts[1].State != "queued" {
		t.Fatal("foreign/stale message mutated job")
	}
}
func TestCancelRaceRetainsObservationAndHistoricalStartReceipt(t *testing.T) {
	f := newFixture(t)
	turn := f.ownerTurn()
	d := f.dispatch(turn, "worker-a")
	f.post("orchestrator", d, 201)
	_, started := f.started(d)
	if err := f.s.StopFeature(context.Background(), f.feature.FeatureID, "human", "Stop all"); err != nil {
		t.Fatal(err)
	}
	f.post("worker-a", started, 200) // This is historical, not permission to spawn again.
	var j JobView
	_ = json.Unmarshal(f.call("worker-a", "GET", "/jobs/"+d.JobID, nil, 200), &j)
	a := j.Attempts[0]
	if !a.CancelRequested || a.State != "running" {
		t.Fatal("cancel falsely terminal")
	}
	success := f.event(d, "task.result", resultPayload(), started.MessageID)
	f.post("worker-a", success, 409)
	interrupted := resultPayload()
	interrupted.Outcome = "interrupted"
	interrupted.Error = &TaskError{Code: "cancel_after_completion", Message: "Output completed before cancellation was observed"}
	interrupted.Observation = &Observation{Outcome: "succeeded", Summary: "Original 42", Evidence: []Evidence{}}
	r := f.event(d, "task.result", interrupted, a.CancelMessageID)
	f.post("worker-a", r, 201)
	h := f.history()
	if h.Jobs[0].Attempts[0].Result.Observation.Summary != "Original 42" {
		t.Fatal("observation lost")
	}
	f.call("orchestrator", "POST", "/owner-turns/"+turn+"/finish", OwnerFinishRequest{Outcome: "interrupted", Reply: "Stopped", Actions: []ActionReceipt{{MessageID: d.MessageID, Status: "stored"}}, Error: &TaskError{Code: "cancel_after_completion", Message: "Stopped"}, Observation: json.RawMessage(`{"reply":"completed before stop"}`)}, 201)
	if err := f.s.ContinueFeature(context.Background(), f.feature.FeatureID, "human"); err != nil {
		t.Fatal(err)
	}
}
func TestDeadlinesFailUnavailableAndLateStartSeparately(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UTC()
	f.s.now = func() time.Time { return now }
	turn := f.ownerTurn()
	a, b := f.dispatch(turn, "worker-a"), f.dispatch(turn, "worker-b")
	f.post("orchestrator", a, 201)
	f.post("orchestrator", b, 201)
	accepted := f.event(b, "task.accepted", AcceptedPayload{b.MessageID, f.s.profiles["worker-b"].Revision}, b.MessageID)
	f.post("worker-b", accepted, 201)
	now = now.Add(31 * time.Second)
	late := f.event(b, "task.started", StartedPayload{accepted.MessageID}, accepted.MessageID)
	f.post("worker-b", late, 409)
	if err := f.s.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Second)
	if err := f.s.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := f.history()
	if h.Jobs[0].Attempts[0].Error.Code != "recipient_unavailable" || h.Jobs[1].Attempts[0].Error.Code != "start_deadline_expired" {
		t.Fatal("deadlines conflated")
	}
	count := 0
	for _, e := range h.Messages {
		if e.Type == "task.result" {
			count++
		}
	}
	if count != 2 {
		t.Fatal("deadline duplicated terminal events")
	}
	lateAccepted := f.event(a, "task.accepted", AcceptedPayload{a.MessageID, f.s.profiles["worker-a"].Revision}, a.MessageID)
	f.post("worker-a", lateAccepted, 409)
}
func TestCommitFailureRollsBackJobAndDoesNotReturnReceipt(t *testing.T) {
	f := newFixture(t)
	turn := f.ownerTurn()
	d := f.dispatch(turn, "worker-a")
	if _, err := f.s.db.Exec(`CREATE TRIGGER fail_delivery BEFORE INSERT ON mailbox_delivery BEGIN SELECT RAISE(ABORT,'simulated disk failure'); END;`); err != nil {
		t.Fatal(err)
	}
	b := f.call("orchestrator", "POST", "/messages", d, 503)
	if bytes.Contains(b, []byte(`"receipt":"stored"`)) {
		t.Fatal("false success")
	}
	h := f.history()
	if len(h.Jobs) != 0 || len(h.Messages) != 1 {
		t.Fatal("transaction partly persisted")
	}
	if _, err := f.s.db.Exec("DROP TRIGGER fail_delivery"); err != nil {
		t.Fatal(err)
	}
	f.post("orchestrator", d, 201)
}
func TestOwnerPartialActionFinishRequiresDefinitiveProof(t *testing.T) {
	f := newFixture(t)
	turn := f.ownerTurn()
	d := f.dispatch(turn, "worker-a")
	f.post("orchestrator", d, 201)
	rejected := f.dispatch(turn, "worker-a")
	f.post("orchestrator", rejected, 429)
	f.finish(turn, "Conceals partial success", []ActionReceipt{}, 409)
	fakeCode := "not_found"
	f.finish(turn, "Fake rejection", []ActionReceipt{{MessageID: d.MessageID, Status: "stored"}, {MessageID: rejected.MessageID, Status: "rejected", ErrorCode: &fakeCode}}, 409)
	realCode := "capacity_exceeded"
	receipts := []ActionReceipt{{MessageID: d.MessageID, Status: "stored"}, {MessageID: rejected.MessageID, Status: "rejected", ErrorCode: &realCode}}
	f.finish(turn, "One queued; other rejected", receipts, 201)
	f.finish(turn, "Different text", receipts, 409)
	h := f.history()
	if len(h.Jobs) != 1 || len(h.SlackOutbox) != 1 {
		t.Fatal("partial action semantics lost")
	}
}
func TestMailboxAtomicACKBoundsAndTerminalReserve(t *testing.T) {
	f := newFixture(t)
	turn := f.ownerTurn()
	d := f.dispatch(turn, "worker-a")
	f.post("orchestrator", d, 201)
	f.s.mailboxLimit = 1
	accepted := f.event(d, "task.accepted", AcceptedPayload{d.MessageID, f.s.profiles["worker-a"].Revision}, d.MessageID)
	ar := f.post("worker-a", accepted, 201)
	// A full ordinary mailbox rejects the whole transition, including its state change.
	started := f.event(d, "task.started", StartedPayload{accepted.MessageID}, accepted.MessageID)
	f.post("worker-a", started, 429)
	if f.history().Jobs[0].Attempts[0].State != "accepted" {
		t.Fatal("failed delivery committed state")
	}
	f.call("orchestrator", "POST", "/acks", AckRequest{[]int64{ar.MailboxSeq, 9999}}, 404)
	var mailbox MailboxResponse
	_ = json.Unmarshal(f.call("orchestrator", "GET", "/mailbox?wait_seconds=0", nil, 200), &mailbox)
	if len(mailbox.Messages) != 1 {
		t.Fatal("partial ACK")
	}
	f.call("orchestrator", "POST", "/acks", AckRequest{[]int64{ar.MailboxSeq}}, 200)
	f.post("worker-a", started, 201)
	result := f.event(d, "task.result", resultPayload(), started.MessageID)
	f.post("worker-a", result, 201) // Terminal reserve even while started fills normal quota.
	if f.history().Jobs[0].Attempts[0].State != "succeeded" {
		t.Fatal("terminal blocked by normal backlog")
	}
	f.call("worker-b", "POST", "/acks", AckRequest{[]int64{ar.MailboxSeq}}, 404)
	f.call("orchestrator", "GET", "/mailbox?limit=21&wait_seconds=0", nil, 400)
}
func TestProfileAndProtocolStrictness(t *testing.T) {
	f := newFixture(t)
	turn := f.ownerTurn()
	d := f.dispatch(turn, "worker-a")
	invalid := d
	invalid.ProtocolVersion = 2
	f.post("orchestrator", invalid, 400)
	raw := map[string]any{}
	_ = json.Unmarshal(mustJSON(d), &raw)
	raw["bypass"] = true
	f.call("orchestrator", "POST", "/messages", raw, 400)
	delete(raw, "bypass")
	delete(raw, "causation_id")
	f.call("orchestrator", "POST", "/messages", raw, 400)
	raw["causation_id"] = nil
	raw["payload"] = map[string]any{"goal": strings.Repeat("a", MaxEnvelopeBytes)}
	f.call("orchestrator", "POST", "/messages", raw, 413)
	unsafe := TextProfile{ID: "bad", Model: "provider/model", Reasoning: "minimal", Prompt: "Text", Tools: []string{"network"}, Extensions: []string{}}
	if _, err := ValidateTextProfile(mustJSON(unsafe)); err == nil {
		t.Fatal("tool-enabled profile accepted")
	}
	if len(f.history().Jobs) != 0 {
		t.Fatal("invalid wire mutated state")
	}
	req, _ := http.NewRequest("GET", f.server.URL+APIPrefix+"/mailbox?wait_seconds=0", nil)
	res, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatal("missing auth accepted")
	}
}
func TestOfflineReconcileRequiresLockEvidenceAndInvalidatesOldInstance(t *testing.T) {
	f := newFixture(t)
	turn := f.ownerTurn()
	d := f.dispatch(turn, "worker-a")
	f.post("orchestrator", d, 201)
	f.started(d)
	path := f.s.dbPath()
	old := f.instances["worker-a"]
	fresh := NewID()
	r := ReconcileRequest{AgentID: "worker-a", OldInstanceID: old, NewInstanceID: fresh, Reason: "Stopped container verified", Actor: "operator", ContainerID: "worker-a-123", ContainerStopped: true, CheckedAt: time.Now()}
	if err := ReconcileOffline(context.Background(), path, f.cfg, r); err == nil {
		t.Fatal("online reconcile accepted")
	}
	f.server.Close()
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	noEvidence := r
	noEvidence.ContainerStopped = false
	if err := ReconcileOffline(context.Background(), path, f.cfg, noEvidence); err == nil {
		t.Fatal("missing cessation accepted")
	}
	if err := ReconcileOffline(context.Background(), path, f.cfg, r); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), path, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.s = s
	f.server = httptest.NewTLSServer(s.Handler())
	t.Cleanup(func() { f.server.Close(); _ = s.Close() })
	f.call("worker-a", "GET", "/mailbox?wait_seconds=0", nil, 409)
	f.instances["worker-a"] = fresh
	f.call("worker-a", "POST", "/agents/self/heartbeat", HeartbeatRequest{InstanceID: fresh}, 200)
	h := f.history()
	if h.Jobs[0].Attempts[0].State != "interrupted" || h.Jobs[0].Attempts[0].Error.Code != "operator_reconciled" {
		t.Fatal("unknown work replayed")
	}
	var m MailboxResponse
	_ = json.Unmarshal(f.call("worker-a", "GET", "/mailbox?wait_seconds=0", nil, 200), &m)
	if len(m.Messages) != 0 {
		t.Fatal("superseded attempt delivered after reset")
	}
}
func TestSlackNoticeIdempotencyAndReopenUnknown(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.s.QueueSlackNotice(ctx, f.feature.FeatureID, "status-event", "Status"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.QueueSlackNotice(ctx, f.feature.FeatureID, "status-event", "Status"); err != nil {
		t.Fatal(err)
	}
	notice, err := f.s.ClaimSlack(ctx)
	if err != nil || notice == nil {
		t.Fatal(err)
	}
	path := f.s.dbPath()
	f.server.Close()
	_ = f.s.Close()
	s, err := Open(ctx, path, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if d, err := s.ClaimSlack(ctx); err != nil || d != nil {
		t.Fatal("uncertain send blindly replayed")
	}
	h, err := s.History(ctx, f.feature.FeatureID)
	if err != nil || len(h.SlackOutbox) != 1 || h.SlackOutbox[0].Status != "delivery_unknown" {
		t.Fatalf("unknown not durable: %v", err)
	}
	if err = s.SettleSlack(ctx, notice.ID, "sent", "123.7"); err != nil {
		t.Fatal(err)
	}
}

func TestPendingTerminalNotificationKeepsSequenceAndFIFO(t *testing.T) {
	f := newFixture(t)
	owner := f.ownerTurn()
	d := f.dispatch(owner, "worker-a")
	f.post("orchestrator", d, 201)
	accepted := f.event(d, "task.accepted", AcceptedPayload{d.MessageID, f.s.profiles["worker-a"].Revision}, d.MessageID)
	ar := f.post("worker-a", accepted, 201)
	f.call("orchestrator", "POST", "/acks", AckRequest{[]int64{ar.MailboxSeq}}, 200)
	start := f.event(d, "task.started", StartedPayload{accepted.MessageID}, accepted.MessageID)
	sr := f.post("worker-a", start, 201)
	f.s.mailboxLimit = 1
	f.s.mailboxReserve = 0 // Exhaust the reserved quota deterministically.
	result := f.event(d, "task.result", resultPayload(), start.MessageID)
	rr := f.post("worker-a", result, 201)
	if f.history().Jobs[0].Attempts[0].State != "succeeded" {
		t.Fatal("outcome lost when reserve exhausted")
	}
	var pending int
	if err := f.s.db.QueryRow("SELECT pending_notification FROM mailbox_delivery WHERE agent_id=? AND seq=?", "orchestrator", rr.MailboxSeq).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("notification not durable: %v", err)
	}
	var m MailboxResponse
	_ = json.Unmarshal(f.call("orchestrator", "GET", "/mailbox?wait_seconds=0", nil, 200), &m)
	if len(m.Messages) != 1 || m.Messages[0].MailboxSeq != sr.MailboxSeq {
		t.Fatal("pending notification bypassed FIFO")
	}
	f.call("orchestrator", "POST", "/acks", AckRequest{[]int64{sr.MailboxSeq}}, 200)
	_ = json.Unmarshal(f.call("orchestrator", "GET", "/mailbox?wait_seconds=0", nil, 200), &m)
	if len(m.Messages) != 1 || m.Messages[0].MailboxSeq != rr.MailboxSeq {
		t.Fatal("notification lost or sequence changed after promotion")
	}
}

func TestControlLaneAndOwnerStartAreIndependentOfNormalBacklog(t *testing.T) {
	f := newFixture(t)
	turn := f.ownerTurn()
	d := f.dispatch(turn, "worker-a")
	f.post("orchestrator", d, 201)
	h := f.history()
	input := h.Messages[0]
	f.call("orchestrator", "POST", "/owner-turns/start", OwnerStartRequest{turn, f.feature.FeatureID, input.MailboxSeq}, 200)
	f.call("orchestrator", "POST", "/owner-turns/start", OwnerStartRequest{NewID(), f.feature.FeatureID, input.MailboxSeq}, 409)
	f.call("worker-a", "POST", "/owner-turns/start", OwnerStartRequest{NewID(), f.feature.FeatureID, input.MailboxSeq}, 403)
	f.s.mailboxLimit = 0
	f.s.mailboxReserve = 0 // Exhausted normal/terminal capacity must not hide control.
	if err := f.s.StopFeature(context.Background(), f.feature.FeatureID, "human", "Stop"); err != nil {
		t.Fatal(err)
	}
	var controls MailboxResponse
	_ = json.Unmarshal(f.call("worker-a", "GET", "/mailbox?lane=control&wait_seconds=0", nil, 200), &controls)
	if len(controls.Messages) != 1 || controls.Messages[0].Type != "task.cancel" {
		t.Fatal("control blocked by unacked dispatch")
	}
	_ = json.Unmarshal(f.call("orchestrator", "GET", "/mailbox?lane=control&wait_seconds=0", nil, 200), &controls)
	if len(controls.Messages) != 1 || controls.Messages[0].Type != "turn.cancel" {
		t.Fatal("owner cancel missing")
	}
	if err := f.s.ContinueFeature(context.Background(), f.feature.FeatureID, "human"); err == nil {
		t.Fatal("continued unresolved work")
	}
	var state OwnerTurn
	_ = json.Unmarshal(f.call("orchestrator", "GET", "/owner-turns/"+turn, nil, 200), &state)
	if !state.CancelRequested || state.State != "running" {
		t.Fatal("owner cancelled without process proof")
	}
}

func TestRequiredNestedFieldsAndNullAreRejected(t *testing.T) {
	f := newFixture(t)
	owner := f.ownerTurn()
	d := f.dispatch(owner, "worker-a")
	var payload map[string]any
	_ = json.Unmarshal(d.Payload, &payload)
	delete(payload["context"].(map[string]any), "text")
	d.Payload = mustJSON(payload)
	f.post("orchestrator", d, 400)
	d = f.dispatch(owner, "worker-a")
	f.post("orchestrator", d, 201)
	r := f.event(d, "task.result", ResultPayload{Outcome: "failed", Summary: "Missing profile", Evidence: []Evidence{}, Error: &TaskError{Code: "profile_unavailable", Message: "Unavailable"}, Origin: "worker"}, d.MessageID)
	_ = json.Unmarshal(r.Payload, &payload)
	delete(payload["error"].(map[string]any), "retryable")
	r.Payload = mustJSON(payload)
	f.post("worker-a", r, 400)
	payload["error"].(map[string]any)["retryable"] = nil
	r.Payload = mustJSON(payload)
	f.post("worker-a", r, 400)
}
