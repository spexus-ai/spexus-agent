package swarmrunner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

const tenant = "10000000-0000-4000-8000-000000000001"
const project = "20000000-0000-4000-8000-000000000001"
const feature = "30000000-0000-4000-8000-000000000001"
const job = "40000000-0000-4000-8000-000000000001"
const attempt = "50000000-0000-4000-8000-000000000001"

type modelFunc func(context.Context, string, string) (string, bool, error)

func (f modelFunc) Run(ctx context.Context, key, input string) (string, bool, error) {
	return f(ctx, key, input)
}
func profileFixture(t *testing.T) (profile, string) {
	t.Helper()
	p := swarm.TextProfile{ID: "worker-a", Model: "test/model", Reasoning: "minimal", Prompt: "Return JSON", Tools: []string{}, Extensions: []string{}}
	b, _ := json.Marshal(p)
	file := filepath.Join(t.TempDir(), "profile.json")
	if e := os.WriteFile(file, b, 0600); e != nil {
		t.Fatal(e)
	}
	return profile{TextProfile: p, Revision: swarm.Digest(b), Generation: 1, Bytes: b}, file
}
func runnerFixture(t *testing.T, handler http.Handler) (*Runner, *httptest.Server) {
	t.Helper()
	p, file := profileFixture(t)
	claimID := swarm.NewID()
	wrapper := http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		path := q.URL.Path
		if path == swarm.APIPrefix+"/agent-profiles/worker-a/active" {
			writeJSON(w, swarm.ActiveProfile{Slot: "worker-a", Role: "worker", ProfileID: "worker-a", Enabled: true, Generation: 1, ActiveRevision: p.Revision, SnapshotJSON: p.Bytes, SnapshotBytesBase64: base64.StdEncoding.EncodeToString(p.Bytes), AllowedModels: []string{p.Model}})
			return
		}
		if path == swarm.APIPrefix+"/agent-profiles/worker-a/launch-claims" {
			var request swarm.LaunchClaimRequest
			if err := json.NewDecoder(q.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			writeJSON(w, swarm.LaunchClaim{ClaimID: claimID, ExecutionRef: request.ExecutionRef, ProfileID: "worker-a", Generation: 1, Revision: p.Revision, SnapshotJSON: p.Bytes, SnapshotBytesBase64: base64.StdEncoding.EncodeToString(p.Bytes), State: "starting"})
			return
		}
		if path == swarm.APIPrefix+"/agent-profiles/worker-a/observations" {
			writeJSON(w, map[string]any{"profile_id": "worker-a", "claim_id": claimID, "revision": p.Revision, "outcome": "launched"})
			return
		}
		handler.ServeHTTP(w, q)
	})
	s := httptest.NewTLSServer(wrapper)
	t.Cleanup(s.Close)
	j, e := OpenJournal(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { j.Close() })
	r := &Runner{cfg: Config{TenantID: tenant, ProjectID: project, AgentID: "worker-a", InstanceID: swarm.NewID(), Role: "worker", ProfileID: "worker-a", AvailableModels: []string{p.Model}, ProfileFile: file}, profile: p, journal: j, client: &Client{http: s.Client(), base: s.URL + swarm.APIPrefix, token: "fixture", instance: swarm.NewID()}}
	return r, s
}
func dispatchFixture(r *Runner) swarm.Delivery {
	p := swarm.DispatchPayload{Goal: "Add 1 and 2", Scope: "arithmetic", ExpectedResult: []string{"3"}, Context: swarm.TaskContext{Text: "private-A", Refs: []swarm.ContextRef{}}, Profile: r.profile.wire(), AcceptBy: time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano), RunTimeoutSeconds: 60}
	raw, _ := json.Marshal(p)
	return swarm.Delivery{Envelope: swarm.Envelope{ProtocolVersion: 1, MessageID: swarm.NewID(), Type: "task.dispatch", TenantID: tenant, ProjectID: project, FeatureID: feature, FromAgentID: "owner", ToAgentID: "worker-a", OwnerTurnID: swarm.NewID(), JobID: job, AttemptID: attempt, SentAt: time.Now().UTC().Format(time.RFC3339Nano), Payload: raw}, MailboxSeq: 1, ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano)}
}
func storeInput(t *testing.T, r *Runner, d swarm.Delivery) {
	t.Helper()
	if e := r.journal.receive(d); e != nil {
		t.Fatal(e)
	}
}
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestWorkerRejectsMalformedBlockedResultWithoutSecondLaunch(t *testing.T) {
	var acceptedID string
	var published []swarm.ResultPayload
	handler := http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Path == swarm.APIPrefix+"/messages" {
			var message swarm.Envelope
			if err := json.NewDecoder(q.Body).Decode(&message); err != nil {
				t.Error(err)
			}
			if message.Type == "task.accepted" {
				acceptedID = message.MessageID
			}
			if message.Type == "task.result" {
				var result swarm.ResultPayload
				if err := json.Unmarshal(message.Payload, &result); err != nil {
					t.Error(err)
				}
				published = append(published, result)
			}
			writeJSON(w, swarm.Receipt{MessageID: message.MessageID, Receipt: "stored", MailboxSeq: 2})
			return
		}
		if strings.Contains(q.URL.Path, "/jobs/") {
			writeJSON(w, swarm.JobView{JobID: job, FeatureID: feature, CurrentAttemptID: attempt, Attempts: []swarm.Attempt{{AttemptID: attempt, AssignedAgentID: "worker-a", State: "running", AcceptedMessageID: acceptedID}}})
			return
		}
		t.Errorf("unexpected request %s", q.URL.Path)
		w.WriteHeader(404)
	})
	r, _ := runnerFixture(t, handler)
	r.cfg.WireVersion = 2
	d := dispatchFixture(r)
	d.ProtocolVersion = 2
	storeInput(t, r, d)
	launches := 0
	r.model = modelFunc(func(_ context.Context, _, input string) (string, bool, error) {
		launches++
		base := `{"outcome":"blocked","summary":"Нужен формат","evidence":[],"error":null,"blocker":{"reason":"Формат выбирает человек","context":"A ждёт","question":"Короткий или подробный?","options":[{"id":"short","label":"Короткий"},{"id":"detailed","label":"Подробный"}],"recommendation":"short","kind":"choice"`
		return base + `,"blocked_work":"A"}}`, false, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.process(ctx, d); err != nil {
		t.Fatal(err)
	}
	if launches != 1 || len(published) != 1 || published[0].Outcome != "failed" {
		t.Fatalf("launches=%d results=%+v", launches, published)
	}
}
func httpError(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	writeJSON(w, map[string]any{"error": map[string]any{"code": code}})
}

func TestWorkerDeliveryLostStartReceiptAndLateCancel(t *testing.T) {
	for _, scenario := range []string{"duplicate", "lost_start_cancel", "start_rejected_cancel", "completed_cancel"} {
		t.Run(scenario, func(t *testing.T) {
			var mu sync.Mutex
			acceptedID := ""
			startedID := ""
			cancelID := swarm.NewID()
			cancelled := false
			startRequests := 0
			results := []swarm.ResultPayload{}
			var launches atomic.Int32
			var r *Runner
			handler := http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if q.URL.Path == swarm.APIPrefix+"/messages" {
					var m swarm.Envelope
					_ = json.NewDecoder(q.Body).Decode(&m)
					switch m.Type {
					case "task.accepted":
						acceptedID = m.MessageID
					case "task.started":
						if startedID != "" && startedID != m.MessageID {
							t.Error("retry changed message ID")
						}
						startedID = m.MessageID
						startRequests++
						if scenario == "start_rejected_cancel" {
							cancelled = true
							httpError(w, 409, "cancel_requested")
							return
						}
						if scenario == "lost_start_cancel" && startRequests == 1 {
							cancelled = true
							httpError(w, 503, "storage_unavailable")
							return
						}
					case "task.result":
						var p swarm.ResultPayload
						_ = json.Unmarshal(m.Payload, &p)
						if cancelled && p.Outcome == "succeeded" {
							httpError(w, 409, "cancel_requested")
							return
						}
						results = append(results, p)
						if cancelled && m.CausationID != nil && *m.CausationID != cancelID {
							t.Error("wrong cancellation causation")
						}
					}
					writeJSON(w, swarm.Receipt{MessageID: m.MessageID, Receipt: "stored", MailboxSeq: 2})
					return
				}
				if strings.Contains(q.URL.Path, "/jobs/") {
					writeJSON(w, swarm.JobView{JobID: job, FeatureID: feature, CurrentAttemptID: attempt, Attempts: []swarm.Attempt{{AttemptID: attempt, AssignedAgentID: "worker-a", State: "running", CancelRequested: cancelled, CancelMessageID: cancelID, AcceptedMessageID: acceptedID}}})
					return
				}
				t.Errorf("unexpected request %s", q.URL.Path)
				w.WriteHeader(404)
			})
			r, _ = runnerFixture(t, handler)
			d := dispatchFixture(r)
			storeInput(t, r, d)
			r.model = modelFunc(func(ctx context.Context, key, input string) (string, bool, error) {
				launches.Add(1)
				if !strings.Contains(input, "private-A") {
					t.Error("missing explicit context")
				}
				if scenario == "completed_cancel" {
					mu.Lock()
					cancelled = true
					mu.Unlock()
					r.mu.Lock()
					r.cancel()
					r.mu.Unlock()
				}
				return `{"outcome":"succeeded","summary":"3","evidence":[{"kind":"text","label":"sum","content_or_ref":"1+2=3"}],"error":null}`, false, nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if e := r.process(ctx, d); e != nil {
				t.Fatal(e)
			}
			if e := r.journal.receive(d); e != nil {
				t.Fatal(e)
			}
			if _, ok, e := r.journal.next(); e != nil || ok {
				t.Fatalf("duplicate scheduled: %v %v", ok, e)
			}
			if len(results) != 1 {
				t.Fatalf("terminal results %d", len(results))
			}
			switch scenario {
			case "duplicate":
				if launches.Load() != 1 || results[0].Outcome != "succeeded" {
					t.Fatal("wrong normal execution")
				}
			case "lost_start_cancel":
				if launches.Load() != 0 || startRequests != 2 || results[0].Outcome != "cancelled" {
					t.Fatal("historical started receipt replayed execution")
				}
			case "start_rejected_cancel":
				if launches.Load() != 0 || results[0].Outcome != "cancelled" {
					t.Fatal("cancel-before-start not settled without spawning")
				}
			case "completed_cancel":
				if launches.Load() != 1 || results[0].Outcome != "interrupted" || results[0].Observation == nil || results[0].Observation.Summary != "3" {
					t.Fatalf("completed output lost: %+v", results)
				}
			}
		})
	}
}
func TestJournalDurabilityAndProcessGeneration(t *testing.T) {
	dir := t.TempDir()
	j, e := OpenJournal(dir)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := OpenJournal(dir); e == nil {
		t.Fatal("second runner acquired active volume")
	}
	instance := swarm.NewID()
	if e = j.bind(instance); e != nil {
		t.Fatal(e)
	}
	d := swarm.Delivery{Envelope: swarm.Envelope{Type: "task.dispatch", MessageID: swarm.NewID()}, MailboxSeq: 1}
	if e = j.receive(d); e != nil {
		t.Fatal(e)
	}
	if e = j.starting(1, ""); e != nil {
		t.Fatal(e)
	}
	if e = j.pin(1, profile{TextProfile: swarm.TextProfile{ID: "worker-a", Model: "test/model", Reasoning: "minimal"}, Revision: "fixture", Generation: 1, Bytes: []byte("fixture")}, swarm.LaunchClaim{ClaimID: swarm.NewID()}, swarm.ExecutionRef{WorkerAttemptID: swarm.NewID()}); e != nil {
		t.Fatal(e)
	}
	if e = j.launch(1); e != nil {
		t.Fatal(e)
	}
	j.Close()
	j, e = OpenJournal(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer j.Close()
	if e = j.bind(instance); e == nil {
		t.Fatal("same process generation reused")
	}
	if e = j.bind(swarm.NewID()); e != nil {
		t.Fatal(e)
	}
	if e = j.bind(instance); e == nil {
		t.Fatal("earlier generation reused after a newer generation")
	}
	if e = j.recover(); e != nil {
		t.Fatal(e)
	}
	if _, ok, e := j.next(); e != nil || ok {
		t.Fatal("unknown start automatically replayed")
	}
	h, e := j.History()
	if e != nil || h.Inputs[0].State != "interrupted" || h.Inputs[0].Launches != 1 {
		t.Fatalf("history %+v %v", h, e)
	}
}
func TestDurableOutboxRetainsIDsWhenCoordinatorUnavailable(t *testing.T) {
	r, _ := runnerFixture(t, http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) { httpError(w, 503, "storage_unavailable") }))
	d := dispatchFixture(r)
	storeInput(t, r, d)
	if e := r.journal.starting(1, ""); e != nil {
		t.Fatal(e)
	}
	m := r.envelope(d, "task.result", "owner", failedResult("model_execution_failed"), ptr(swarm.NewID()))
	if e := r.journal.output(1, "saved", []swarm.Envelope{m}, nil, ""); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if e := r.flush(ctx); e == nil {
		t.Fatal("unavailable coordinator accepted")
	}
	p, ok, e := r.journal.pending()
	if e != nil || !ok {
		t.Fatal("outbox lost")
	}
	var saved swarm.Envelope
	_ = json.Unmarshal(p.Body, &saved)
	if saved.MessageID != m.MessageID {
		t.Fatal("retry ID changed")
	}
	h, _ := r.journal.History()
	if h.Inputs[0].State != "output" || h.Inputs[0].Launches != 0 {
		t.Fatal("unsent result replayed model")
	}
}
func TestBookkeepingAndSessionIsolation(t *testing.T) {
	r, _ := runnerFixture(t, http.NotFoundHandler())
	var launches atomic.Int32
	r.model = modelFunc(func(context.Context, string, string) (string, bool, error) {
		launches.Add(1)
		return "", false, errors.New("should not run")
	})
	for i, typ := range []string{"task.accepted", "task.started", "task.review"} {
		d := dispatchFixture(r)
		d.Type = typ
		d.MailboxSeq = int64(i + 1)
		d.CausationID = ptr(swarm.NewID())
		switch typ {
		case "task.accepted":
			d.OwnerTurnID = ""
			d.Payload, _ = json.Marshal(swarm.AcceptedPayload{DispatchMessageID: swarm.NewID(), ProfileRevision: r.profile.Revision})
		case "task.started":
			d.OwnerTurnID = ""
			d.Payload, _ = json.Marshal(swarm.StartedPayload{AcceptedMessageID: swarm.NewID()})
		case "task.review":
			d.Payload, _ = json.Marshal(swarm.ReviewPayload{ResultMessageID: swarm.NewID(), Verdict: "accepted", Reason: "verified", Evidence: []swarm.Evidence{}})
		}
		storeInput(t, r, d)
		if e := r.process(context.Background(), d); e != nil {
			t.Fatal(e)
		}
	}
	if launches.Load() != 0 {
		t.Fatal("bookkeeping invoked model")
	}
	a := dispatchFixture(r)
	b := a
	b.AttemptID = swarm.NewID()
	if r.cfg.session(a) == r.cfg.session(b) {
		t.Fatal("worker attempt reused session")
	}
	b = a
	b.JobID = swarm.NewID()
	if r.cfg.session(a) == r.cfg.session(b) {
		t.Fatal("worker job reused session")
	}
	r.cfg.Role = "owner"
	if r.cfg.session(a) != r.cfg.session(b) {
		t.Fatal("same feature owner lost history")
	}
	b.FeatureID = swarm.NewID()
	if r.cfg.session(a) == r.cfg.session(b) {
		t.Fatal("owner feature mixed")
	}
}
func TestOwnerWholeOutputValidatedBeforeAnyAction(t *testing.T) {
	r, _ := runnerFixture(t, http.NotFoundHandler())
	r.cfg.Role = "owner"
	r.cfg.AgentID = "owner"
	r.cfg.Targets = []Target{{AgentID: "worker-a", ProfileID: "worker-a"}}
	r.targets = []targetProfile{{"worker-a", r.profile.wire()}}
	d := dispatchFixture(r)
	data := dispatchAction{WorkerAgentID: "worker-a", DispatchPayload: swarm.DispatchPayload{Goal: "sum", Scope: "only text", ExpectedResult: []string{"3"}, Context: swarm.TaskContext{Text: "A", Refs: []swarm.ContextRef{}}, Profile: r.profile.wire()}}
	raw, _ := json.Marshal(data)
	good := action{Kind: "dispatch", Data: raw}
	output, _ := json.Marshal(ownerOutput{Actions: []action{good, {Kind: "shell", Data: json.RawMessage(`{}`)}}, Reply: "waiting"})
	if _, messages, e := r.ownerActions(d, swarm.NewID(), string(output)); e == nil || messages != nil {
		t.Fatal("partial invalid intent admitted")
	}
	h, _ := r.journal.History()
	if h.Pending != 0 {
		t.Fatal("validation created side effects")
	}
	output, _ = json.Marshal(ownerOutput{Actions: []action{good}, Reply: "waiting"})
	_, messages, e := r.ownerActions(d, swarm.NewID(), string(output))
	if e != nil || len(messages) != 1 {
		t.Fatalf("valid action rejected: %v", e)
	}
}
func TestProfileAndOutputValidation(t *testing.T) {
	r, _ := runnerFixture(t, http.NotFoundHandler())
	d := dispatchFixture(r)
	for _, s := range []string{`{"outcome":"succeeded","summary":"ok","evidence":[]}`, "```json\n{}\n```", `{"outcome":"succeeded","summary":"ok","evidence":[],"error":null,"command":"bash"}`} {
		if _, e := r.workerResult(d, s); e == nil {
			t.Fatalf("bad worker output accepted: %s", s)
		}
	}
	for _, tools := range []any{nil, []string{"network"}, []string{"read", "read"}} {
		p := r.profile.TextProfile
		b, _ := json.Marshal(p)
		var v map[string]any
		_ = json.Unmarshal(b, &v)
		v["tools"] = tools
		b, _ = json.Marshal(v)
		if e := os.WriteFile(r.cfg.ProfileFile, b, 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := loadProfile(r.cfg.ProfileFile); e == nil {
			t.Fatal("unsafe profile admitted")
		}
	}
}
func TestInboxCommittedBeforeACKAndControlNotBlockedByACK(t *testing.T) {
	var r *Runner
	var d swarm.Delivery
	var cancelled atomic.Bool
	served := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		switch {
		case strings.Contains(q.URL.Path, "/mailbox"):
			if served {
				writeJSON(w, swarm.MailboxResponse{Messages: []swarm.Delivery{}})
				return
			}
			served = true
			writeJSON(w, swarm.MailboxResponse{Messages: []swarm.Delivery{d}})
		case strings.HasSuffix(q.URL.Path, "/acks"):
			var count int
			if e := r.journal.db.QueryRow(`SELECT count(*) FROM inbox WHERE seq=?`, d.MailboxSeq).Scan(&count); e != nil || count != 1 {
				t.Error("ACK before durable inbox")
			}
			if !cancelled.Load() {
				t.Error("ACK waited before control interruption")
			}
			writeJSON(w, swarm.AckResponse{Acked: []int64{d.MailboxSeq}})
		default:
			w.WriteHeader(404)
		}
	})
	r, _ = runnerFixture(t, handler)
	d = dispatchFixture(r)
	d.Type = "task.cancel"
	d.CausationID = ptr(swarm.NewID())
	d.Payload, _ = json.Marshal(swarm.CancelPayload{Reason: "stop", RequestedBy: "owner"})
	r.active = d
	r.cancel = func() { cancelled.Store(true) }
	if e := r.poll(context.Background(), "control"); e != nil {
		t.Fatal(e)
	}
}

func TestOwnerTurnControlCancelsActivePiBeforeACK(t *testing.T) {
	var r *Runner
	var d swarm.Delivery
	var cancelled atomic.Bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		switch {
		case strings.Contains(q.URL.Path, "/mailbox"):
			writeJSON(w, swarm.MailboxResponse{Messages: []swarm.Delivery{d}})
		case strings.HasSuffix(q.URL.Path, "/acks"):
			var count int
			if e := r.journal.db.QueryRow(`SELECT count(*) FROM inbox WHERE seq=?`, d.MailboxSeq).Scan(&count); e != nil || count != 1 {
				t.Error("owner control was ACKed before durable inbox commit")
			}
			if !cancelled.Load() {
				t.Error("owner Pi was not interrupted before ACK")
			}
			writeJSON(w, swarm.AckResponse{Acked: []int64{d.MailboxSeq}})
		default:
			w.WriteHeader(404)
		}
	})
	r, _ = runnerFixture(t, handler)
	r.cfg.Role, r.cfg.AgentID = "owner", "owner"
	turnID := swarm.NewID()
	d = swarm.Delivery{Envelope: swarm.Envelope{ProtocolVersion: 1, MessageID: swarm.NewID(), Type: "turn.cancel", TenantID: tenant, ProjectID: project, FeatureID: feature, FromAgentID: "coordinator", ToAgentID: "owner", OwnerTurnID: turnID, SentAt: time.Now().UTC().Format(time.RFC3339Nano)}, MailboxSeq: 1}
	d.Payload, _ = json.Marshal(swarm.CancelPayload{Reason: "urgent Slack message", RequestedBy: "human"})
	r.active = swarm.Delivery{Envelope: swarm.Envelope{FeatureID: feature, OwnerTurnID: turnID}}
	r.cancel = func() { cancelled.Store(true) }
	if e := r.poll(context.Background(), "control"); e != nil {
		t.Fatal(e)
	}
}

func TestUrgentInputWinsRunnerJournalAndDeferredOrdinaryIsRetained(t *testing.T) {
	var startCalls atomic.Int32
	r, _ := runnerFixture(t, http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if strings.HasSuffix(q.URL.Path, "/owner-turns/start") {
			startCalls.Add(1)
			httpError(w, 409, "urgent_input_pending")
			return
		}
		w.WriteHeader(404)
	}))
	r.cfg.Role, r.cfg.AgentID = "owner", "owner"
	delivery := func(seq int64, text string) swarm.Delivery {
		payload, _ := json.Marshal(swarm.InputPayload{Text: text, Source: swarm.Source{Kind: "slack", EventID: fmt.Sprintf("source-%d", seq), ChannelID: "C", ThreadTS: "1.1", ActorID: "U"}})
		return swarm.Delivery{Envelope: swarm.Envelope{ProtocolVersion: 1, MessageID: swarm.NewID(), Type: "agent.input", TenantID: tenant, ProjectID: project, FeatureID: feature, FromAgentID: "coordinator", ToAgentID: "owner", SentAt: time.Now().UTC().Format(time.RFC3339Nano), Payload: payload}, MailboxSeq: seq}
	}
	ordinary := delivery(1, "buffered ordinary input")
	urgent := delivery(2, "!urgent correction")
	storeInput(t, r, ordinary)
	storeInput(t, r, urgent)
	first, ok, err := r.journal.next()
	if err != nil || !ok || first.MailboxSeq != urgent.MailboxSeq {
		t.Fatalf("runner did not prioritize urgent input: %+v, %t, %v", first, ok, err)
	}
	if err := r.journal.state(urgent.MailboxSeq, "applied", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.owner(context.Background(), ordinary); err != nil {
		t.Fatal(err)
	}
	if startCalls.Load() != 1 {
		t.Fatalf("ordinary input made %d start attempts", startCalls.Load())
	}
	var state string
	if err := r.journal.db.QueryRow(`SELECT state FROM inbox WHERE seq=?`, ordinary.MailboxSeq).Scan(&state); err != nil || state != "received" {
		t.Fatalf("deferred ordinary input was lost: %q, %v", state, err)
	}
	remaining, ok, err := r.journal.next()
	if err != nil || !ok || remaining.MailboxSeq != ordinary.MailboxSeq {
		t.Fatalf("deferred ordinary input not available for next turn: %+v, %t, %v", remaining, ok, err)
	}
}

func TestOwnerFinishCancellationBetweenReadAndCommit(t *testing.T) {
	var r *Runner
	turn := swarm.NewID()
	cancelled := false
	finishes := 0
	var accepted swarm.OwnerFinishRequest
	handler := http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Method == "GET" {
			writeJSON(w, swarm.OwnerTurn{TurnID: turn, FeatureID: feature, State: "running", CancelRequested: cancelled})
			return
		}
		if strings.HasSuffix(q.URL.Path, "/finish") {
			finishes++
			var f swarm.OwnerFinishRequest
			_ = json.NewDecoder(q.Body).Decode(&f)
			if finishes == 1 {
				cancelled = true
				httpError(w, 409, "cancel_requested")
				return
			}
			accepted = f
			writeJSON(w, swarm.OwnerFinishReceipt{TurnID: turn, State: f.Outcome, ReplyStatus: "queued"})
			return
		}
		w.WriteHeader(404)
	})
	r, _ = runnerFixture(t, handler)
	d := dispatchFixture(r)
	d.Type = "agent.input"
	d.JobID = ""
	d.AttemptID = ""
	storeInput(t, r, d)
	if e := r.journal.starting(1, turn); e != nil {
		t.Fatal(e)
	}
	original := `{"actions":[],"reply":"complete"}`
	if e := r.journal.modelOutput(1, original); e != nil {
		t.Fatal(e)
	}
	finish := swarm.OwnerFinishRequest{Outcome: "succeeded", Reply: "complete", Actions: []swarm.ActionReceipt{}, Observation: json.RawMessage("null")}
	if e := r.ownerFinish(context.Background(), d, turn, finish, nil); e != nil {
		t.Fatal(e)
	}
	if finishes != 2 || accepted.Outcome != "interrupted" || accepted.Error.Code != "cancel_after_completion" || !strings.Contains(string(accepted.Observation), "complete") {
		t.Fatalf("lost completed owner output: %+v", accepted)
	}
	h, _ := r.journal.History()
	if h.Inputs[0].State != "applied" {
		t.Fatal("accepted reconciliation not applied")
	}
}

func TestOwnerReviewEvidenceRequiresObjects(t *testing.T) {
	var gets atomic.Int32
	resultID := swarm.NewID()
	r, _ := runnerFixture(t, http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		gets.Add(1)
		if !strings.Contains(q.URL.Path, "/jobs/") {
			t.Errorf("unexpected network action %s", q.URL.Path)
		}
		writeJSON(w, swarm.JobView{JobID: job, FeatureID: feature, CurrentAttemptID: attempt, Attempts: []swarm.Attempt{{AttemptID: attempt, AssignedAgentID: "worker-a", State: "succeeded", Review: "pending", ResultMessageID: resultID}}})
	}))
	r.cfg.Role = "owner"
	r.cfg.AgentID = "owner"
	d := dispatchFixture(r)
	d.Type, d.MessageID = "task.result", resultID
	data := map[string]any{"job_id": job, "attempt_id": attempt, "result_message_id": resultID, "verdict": "accepted", "reason": "17+25+38=80", "evidence": []string{"Сумма: 80", "ALPHA-619"}}
	encode := func() string {
		raw, _ := json.Marshal(data)
		output, _ := json.Marshal(ownerOutput{Actions: []action{{Kind: "review", Data: raw}}, Reply: "Reviewed"})
		return string(output)
	}
	if _, actions, e := r.ownerActions(d, swarm.NewID(), encode()); e == nil || actions != nil {
		t.Fatal("live invalid string evidence was accepted or coerced")
	}
	if gets.Load() != 0 {
		t.Fatal("invalid evidence reached dependent API before strict decode")
	}
	data["evidence"] = []swarm.Evidence{{Kind: "text", Label: "sum", ContentOrRef: "17+25+38=80"}}
	_, actions, e := r.ownerActions(d, swarm.NewID(), encode())
	if e != nil || len(actions) != 1 || actions[0].Type != "task.review" {
		t.Fatalf("schema-valid evidence object rejected: %v", e)
	}
	h, _ := r.journal.History()
	if h.Pending != 0 {
		t.Fatal("validation had publish side effect")
	}
}

func TestOwnerCannotSkipSuccessfulResultReview(t *testing.T) {
	resultID := swarm.NewID()
	r, _ := runnerFixture(t, http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		writeJSON(w, swarm.JobView{JobID: job, FeatureID: feature, CurrentAttemptID: attempt, Attempts: []swarm.Attempt{{AttemptID: attempt, AssignedAgentID: "worker-a", State: "succeeded", Review: "pending", ResultMessageID: resultID, Result: &swarm.ResultPayload{Outcome: "succeeded", Summary: "file changed", Evidence: []swarm.Evidence{}}}}})
	}))
	r.cfg.Role, r.cfg.AgentID = "owner", "owner"
	d := dispatchFixture(r)
	d.Type, d.MessageID = "task.result", resultID
	if _, actions, err := r.ownerActions(d, swarm.NewID(), `{"actions":[],"reply":"Done."}`); err == nil || actions != nil {
		t.Fatal("action-free reply skipped a successful result's review")
	}
}

// Test: an owner model may remember and re-review B while handling A's new
// task.result. Preflight rejects the whole first output, then one correction
// publishes only A. Cancellation or a second bad output publishes nothing.
// Validates: AC-437/465 (REQ-355/392 - no partial action and durable output).
func TestOwnerCorrectsStaleReviewBeforePublication(t *testing.T) {
	for _, mode := range []string{"corrected", "invalid_again", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			resultA, jobB, attemptB, resultB := swarm.NewID(), swarm.NewID(), swarm.NewID(), swarm.NewID()
			var published []swarm.Envelope
			var finished swarm.OwnerFinishRequest
			var launches int
			var mu sync.Mutex
			handler := http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
				switch {
				case q.Method == http.MethodPost && strings.HasSuffix(q.URL.Path, "/owner-turns/start"):
					writeJSON(w, swarm.OwnerStartReceipt{TurnID: swarm.NewID(), State: "running"})
				case q.Method == http.MethodGet && strings.Contains(q.URL.Path, "/owner-turns/"):
					writeJSON(w, swarm.OwnerTurn{State: "running", FeatureID: feature})
				case q.Method == http.MethodGet && strings.Contains(q.URL.Path, "/jobs/"):
					if !strings.Contains(q.URL.Path, job) {
						t.Errorf("stale B review reached job API: %s", q.URL.Path)
					}
					writeJSON(w, swarm.JobView{JobID: job, FeatureID: feature, CurrentAttemptID: attempt, Attempts: []swarm.Attempt{{AttemptID: attempt, AssignedAgentID: "worker-a", State: "succeeded", Review: "pending", ResultMessageID: resultA}}})
				case q.Method == http.MethodPost && strings.HasSuffix(q.URL.Path, "/messages"):
					var m swarm.Envelope
					if err := json.NewDecoder(q.Body).Decode(&m); err != nil {
						t.Error(err)
					}
					mu.Lock()
					published = append(published, m)
					mu.Unlock()
					writeJSON(w, swarm.Receipt{MessageID: m.MessageID, Receipt: "stored"})
				case q.Method == http.MethodPost && strings.HasSuffix(q.URL.Path, "/finish"):
					if err := json.NewDecoder(q.Body).Decode(&finished); err != nil {
						t.Error(err)
					}
					writeJSON(w, swarm.OwnerFinishReceipt{State: finished.Outcome, ReplyStatus: "queued"})
				default:
					t.Errorf("unexpected %s %s", q.Method, q.URL.Path)
					http.NotFound(w, q)
				}
			})
			r, _ := runnerFixture(t, handler)
			r.cfg.Role, r.cfg.AgentID = "owner", "owner"
			r.targets = []targetProfile{{AgentID: "worker-a", Profile: r.profile.wire()}}
			d := dispatchFixture(r)
			d.Type, d.MessageID, d.ToAgentID = "task.result", resultA, "owner"
			d.Payload, _ = json.Marshal(swarm.ResultPayload{Outcome: "succeeded", Summary: "A done", Evidence: []swarm.Evidence{}, Origin: "worker"})
			storeInput(t, r, d)
			makeReview := func(idJob, idAttempt, idResult string) action {
				b, _ := json.Marshal(reviewAction{JobID: idJob, AttemptID: idAttempt, ReviewPayload: swarm.ReviewPayload{ResultMessageID: idResult, Verdict: "accepted", Reason: "verified", Evidence: []swarm.Evidence{}}})
				return action{Kind: "review", Data: b}
			}
			first, _ := json.Marshal(ownerOutput{Actions: []action{makeReview(job, attempt, resultA), makeReview(jobB, attemptB, resultB)}, Reply: "A and B done"})
			corrected, _ := json.Marshal(ownerOutput{Actions: []action{makeReview(job, attempt, resultA)}, Reply: "A done; B was already reviewed"})
			r.model = modelFunc(func(_ context.Context, _, input string) (string, bool, error) {
				launches++
				if launches == 1 {
					return string(first), false, nil
				}
				if !strings.Contains(input, "preceding FINAL JSON was rejected") {
					t.Fatal("missing corrective guidance")
				}
				mu.Lock()
				count := len(published)
				mu.Unlock()
				if count != 0 {
					t.Fatal("partial review published before correction")
				}
				switch mode {
				case "corrected":
					return string(corrected), false, nil
				case "invalid_again":
					return string(first), false, nil
				default:
					return "", true, nil
				}
			})
			if err := r.owner(context.Background(), d); err != nil {
				t.Fatal(err)
			}
			if launches != 2 {
				t.Fatalf("Pi launches=%d", launches)
			}
			wantMessages, wantOutcome := 0, "failed"
			if mode == "corrected" {
				wantMessages, wantOutcome = 1, "succeeded"
			}
			if mode == "cancelled" {
				wantOutcome = "cancelled"
			}
			if len(published) != wantMessages || finished.Outcome != wantOutcome {
				t.Fatalf("published=%d finish=%+v", len(published), finished)
			}
			if wantMessages == 1 && (published[0].JobID != job || published[0].AttemptID != attempt || published[0].Type != "task.review") {
				t.Fatalf("wrong published review: %+v", published)
			}
			var modelOutput string
			if err := r.journal.db.QueryRow("SELECT model_output FROM inbox WHERE seq=?", d.MailboxSeq).Scan(&modelOutput); err != nil {
				t.Fatal(err)
			}
			if mode == "cancelled" && modelOutput != string(first) {
				t.Fatal("completed first model output lost on corrective cancellation")
			}
		})
	}
}

// A recovery delivery of an immutable task.result may arrive after its review
// was already accepted. That turn can report the outcome but cannot repeat or
// start work from an old trigger.
func TestAcceptedTaskResultRecoveryIsSummaryOnly(t *testing.T) {
	for _, firstHasAction := range []bool{false, true} {
		for _, plainText := range []bool{false, true} {
			t.Run(fmt.Sprint("first_has_action=", firstHasAction, "/plain_text=", plainText), func(t *testing.T) {
				resultID, priorJob, priorAttempt, priorResult := swarm.NewID(), swarm.NewID(), swarm.NewID(), swarm.NewID()
				var published []swarm.Envelope
				var finished swarm.OwnerFinishRequest
				handler := http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
					switch {
					case q.Method == http.MethodPost && strings.HasSuffix(q.URL.Path, "/owner-turns/start"):
						writeJSON(w, swarm.OwnerStartReceipt{TurnID: swarm.NewID(), State: "running"})
					case q.Method == http.MethodGet && strings.Contains(q.URL.Path, "/owner-turns/"):
						writeJSON(w, swarm.OwnerTurn{State: "running", FeatureID: feature})
					case q.Method == http.MethodGet && strings.Contains(q.URL.Path, "/jobs/"):
						if strings.Contains(q.URL.Path, priorJob) {
							writeJSON(w, swarm.JobView{JobID: priorJob, FeatureID: feature, CurrentAttemptID: priorAttempt, Attempts: []swarm.Attempt{{AttemptID: priorAttempt, AssignedAgentID: "worker-b", State: "succeeded", Review: "accepted", ResultMessageID: priorResult, Result: &swarm.ResultPayload{Outcome: "succeeded", Summary: "B recorded", Evidence: []swarm.Evidence{}, Origin: "worker"}}}})
						} else {
							writeJSON(w, swarm.JobView{JobID: job, FeatureID: feature, CurrentAttemptID: attempt, Attempts: []swarm.Attempt{{AttemptID: attempt, AssignedAgentID: "worker-a", State: "succeeded", Review: "accepted", ResultMessageID: resultID, Result: &swarm.ResultPayload{Outcome: "succeeded", Summary: "A done", Evidence: []swarm.Evidence{}, Origin: "worker"}}}})
						}
					case q.Method == http.MethodPost && strings.HasSuffix(q.URL.Path, "/messages"):
						var m swarm.Envelope
						_ = json.NewDecoder(q.Body).Decode(&m)
						published = append(published, m)
						writeJSON(w, swarm.Receipt{MessageID: m.MessageID, Receipt: "stored"})
					case q.Method == http.MethodPost && strings.HasSuffix(q.URL.Path, "/finish"):
						if err := json.NewDecoder(q.Body).Decode(&finished); err != nil {
							t.Error(err)
						}
						writeJSON(w, swarm.OwnerFinishReceipt{State: finished.Outcome, ReplyStatus: "queued"})
					default:
						t.Errorf("unexpected %s %s", q.Method, q.URL.Path)
						http.NotFound(w, q)
					}
				})
				r, _ := runnerFixture(t, handler)
				r.cfg.Role, r.cfg.AgentID = "owner", "owner"
				d := dispatchFixture(r)
				d.Type, d.MessageID, d.ToAgentID = "task.result", resultID, "owner"
				d.MailboxSeq = 2
				d.Payload, _ = json.Marshal(swarm.ResultPayload{Outcome: "succeeded", Summary: "A done", Evidence: []swarm.Evidence{}, Origin: "worker"})
				prior := d
				prior.MailboxSeq, prior.MessageID, prior.JobID, prior.AttemptID = 1, priorResult, priorJob, priorAttempt
				prior.Payload, _ = json.Marshal(swarm.ResultPayload{Outcome: "succeeded", Summary: "B recorded", Evidence: []swarm.Evidence{}, Origin: "worker"})
				storeInput(t, r, prior)
				storeInput(t, r, d)
				dispatch, _ := json.Marshal(dispatchAction{WorkerAgentID: "worker-a", DispatchPayload: swarm.DispatchPayload{Goal: "repeat", Scope: "stale", ExpectedResult: []string{"done"}, Context: swarm.TaskContext{Text: "", Refs: []swarm.ContextRef{}}, Profile: r.profile.wire()}})
				bad, _ := json.Marshal(ownerOutput{Actions: []action{{Kind: "dispatch", Data: dispatch}}, Reply: "starting again"})
				good, _ := json.Marshal(ownerOutput{Actions: []action{}, Reply: "Recorded work is complete"})
				launches := 0
				r.model = modelFunc(func(_ context.Context, _, input string) (string, bool, error) {
					launches++
					if !strings.Contains(input, "This task.result was already accepted") || !strings.Contains(input, "actions:[]") || !strings.Contains(input, `"summary":"B recorded"`) {
						t.Fatal("missing summary-only guidance")
					}
					if firstHasAction && launches == 1 {
						return string(bad), false, nil
					}
					if firstHasAction && launches == 2 && !strings.Contains(input, "preceding FINAL JSON was rejected") {
						t.Fatal("missing corrective guidance")
					}
					if plainText {
						return "Recorded work is complete", false, nil
					}
					return string(good), false, nil
				})
				if err := r.owner(context.Background(), d); err != nil {
					t.Fatal(err)
				}
				wantLaunches := 1
				if firstHasAction {
					wantLaunches = 2
				}
				if launches != wantLaunches || len(published) != 0 || finished.Outcome != "succeeded" || finished.Reply != "Recorded work is complete" {
					t.Fatalf("launches=%d published=%d finish=%+v", launches, len(published), finished)
				}
			})
		}
	}
}
