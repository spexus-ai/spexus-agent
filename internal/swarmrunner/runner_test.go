package swarmrunner

import (
	"context"
	"encoding/json"
	"errors"
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
	p := swarm.TextProfile{ID: "worker", Model: "test/model", Reasoning: "minimal", Prompt: "Return JSON", Tools: []string{}, Extensions: []string{}}
	b, _ := json.Marshal(p)
	file := filepath.Join(t.TempDir(), "profile.json")
	if e := os.WriteFile(file, b, 0600); e != nil {
		t.Fatal(e)
	}
	return profile{p, swarm.Digest(b)}, file
}
func runnerFixture(t *testing.T, handler http.Handler) (*Runner, *httptest.Server) {
	t.Helper()
	s := httptest.NewTLSServer(handler)
	t.Cleanup(s.Close)
	p, file := profileFixture(t)
	j, e := OpenJournal(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { j.Close() })
	r := &Runner{cfg: Config{TenantID: tenant, ProjectID: project, AgentID: "worker-a", InstanceID: swarm.NewID(), Role: "worker", ProfileFile: file}, profile: p, journal: j, client: &Client{http: s.Client(), base: s.URL + swarm.APIPrefix, token: "fixture", instance: swarm.NewID()}}
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
	for _, tools := range []any{nil, []string{"bash"}} {
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
	r, _ := runnerFixture(t, http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		gets.Add(1)
		if !strings.Contains(q.URL.Path, "/jobs/") {
			t.Errorf("unexpected network action %s", q.URL.Path)
		}
		writeJSON(w, swarm.JobView{JobID: job, FeatureID: feature, CurrentAttemptID: attempt, Attempts: []swarm.Attempt{{AttemptID: attempt, AssignedAgentID: "worker-a", State: "succeeded"}}})
	}))
	r.cfg.Role = "owner"
	r.cfg.AgentID = "owner"
	d := dispatchFixture(r)
	data := map[string]any{"job_id": job, "attempt_id": attempt, "result_message_id": swarm.NewID(), "verdict": "accepted", "reason": "17+25+38=80", "evidence": []string{"Сумма: 80", "ALPHA-619"}}
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
