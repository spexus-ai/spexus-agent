package swarmrunner

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

func blockerFixture() swarm.Blocker {
	return swarm.Blocker{Reason: "approval required", Context: "choice affects delivery", Question: "Which route?", Options: []swarm.HumanOption{{ID: "safe", Label: "Use safe route"}}, Recommendation: "Use safe route", Kind: "choice"}
}

// Test: blocked output is strict, and a resumed worker gets a fresh Pi session.
// Validates: AC-435 and AC-437 (REQ-353 and REQ-355 - blocked handover and continuation).
func TestHumanBlockedWorkerAndFreshResume(t *testing.T) {
	var r *Runner
	currentAttempt := attempt
	var results []swarm.ResultPayload
	var keys []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		switch {
		case q.URL.Path == swarm.APIPrefix+"/messages":
			var m swarm.Envelope
			if err := json.NewDecoder(q.Body).Decode(&m); err != nil {
				t.Error(err)
			}
			if err := swarm.ValidateEnvelope(m); err != nil {
				t.Errorf("invalid producer envelope: %v", err)
			}
			if m.Type == "task.result" {
				var p swarm.ResultPayload
				_ = json.Unmarshal(m.Payload, &p)
				results = append(results, p)
			}
			writeJSON(w, swarm.Receipt{MessageID: m.MessageID, Receipt: "stored", MailboxSeq: 2})
		case strings.Contains(q.URL.Path, "/jobs/"):
			writeJSON(w, swarm.JobView{JobID: job, FeatureID: feature, CurrentAttemptID: currentAttempt, Attempts: []swarm.Attempt{{AttemptID: currentAttempt, AssignedAgentID: "worker-a", State: "running"}}})
		default:
			t.Errorf("unexpected request: %s", q.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	r, _ = runnerFixture(t, handler)
	r.cfg.WireVersion = 2
	r.model = modelFunc(func(_ context.Context, key, input string) (string, bool, error) {
		keys = append(keys, key)
		if len(keys) == 1 {
			if !strings.Contains(input, `"blocked"`) {
				t.Error("worker was not instructed to report a blocker")
			}
			b, _ := json.Marshal(map[string]any{"outcome": "blocked", "summary": "waiting for approval", "evidence": []any{}, "error": nil, "blocker": blockerFixture()})
			return string(b), false, nil
		}
		if !strings.Contains(input, `"resume"`) || !strings.Contains(input, "human approved") {
			t.Error("resume context omitted")
		}
		return `{"outcome":"succeeded","summary":"done","evidence":[],"error":null}`, false, nil
	})
	d := dispatchFixture(r)
	d.ProtocolVersion = 2
	storeInput(t, r, d)
	if err := r.process(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Outcome != "blocked" || results[0].Blocker == nil || r.active.AttemptID != "" {
		t.Fatalf("blocked result did not finish attempt: %+v", results)
	}
	if _, pending, err := r.journal.next(); err != nil || pending {
		t.Fatalf("blocked attempt restarted or waited: %v %v", pending, err)
	}
	resumeID := swarm.NewID()
	currentAttempt = resumeID
	var dispatch swarm.DispatchPayload
	_ = json.Unmarshal(d.Payload, &dispatch)
	dispatch.Context.Text = "original task; blocker; human approved safe route"
	resume := swarm.ResumeTaskPayload{DependencyID: swarm.NewID(), DecisionID: swarm.NewID(), WorkerAgentID: "worker-a", Dispatch: dispatch}
	d2 := d
	d2.Type = "task.resume"
	d2.MessageID = swarm.NewID()
	d2.AttemptID = resumeID
	d2.MailboxSeq = 3
	d2.Payload, _ = json.Marshal(resume)
	storeInput(t, r, d2)
	if err := r.process(context.Background(), d2); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] == keys[1] || len(results) != 2 || results[1].Outcome != "succeeded" {
		t.Fatalf("resume did not create one new session/attempt: keys=%v results=%+v", keys, results)
	}
	if err := r.journal.receive(d2); err != nil {
		t.Fatal(err)
	}
	if _, pending, err := r.journal.next(); err != nil || pending || len(keys) != 2 {
		t.Fatalf("duplicate resume replayed model: pending=%v err=%v keys=%v", pending, err, keys)
	}
}

// Test: malformed blockers and foreign/stale owner actions publish no commands.
// Validates: AC-437 and AC-463 (REQ-355 and REQ-390 - strict wire and action authority).
func TestHumanStrictOutputAndOwnerActions(t *testing.T) {
	depID, decisionID := swarm.NewID(), swarm.NewID()
	dep := swarm.Dependency{ID: depID, FeatureID: feature, Kind: "job", JobID: job, AttemptID: attempt, State: "resolved", DecisionID: decisionID}
	handler := http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Path == swarm.APIPrefix+"/dependencies/"+depID {
			writeJSON(w, dep)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	r, _ := runnerFixture(t, handler)
	r.cfg.WireVersion = 2
	r.cfg.Role = "owner"
	r.cfg.AgentID = "orchestrator"
	r.targets = []targetProfile{{AgentID: "worker-a", Profile: r.profile.wire()}}
	d := dispatchFixture(r)
	d.Type = "human.decision"
	d.JobID, d.AttemptID, d.OwnerTurnID = "", "", ""
	d.ToAgentID = "orchestrator"
	d.FromAgentID = "coordinator"
	d.ProtocolVersion = 2
	d.Payload, _ = json.Marshal(swarm.HumanDecisionPayload{RequestID: swarm.NewID(), DependencyID: depID, DecisionID: decisionID, State: "answered", Revision: 2, ApplicationStatus: "applied"})
	for _, raw := range []string{
		`{"outcome":"blocked","summary":"wait","evidence":[],"error":null}`,
		`{"outcome":"blocked","summary":"wait","evidence":[],"error":null,"blocker":null}`,
		`{"outcome":"succeeded","summary":"done","evidence":[],"error":null,"blocker":null}`,
		`{"outcome":"blocked","summary":"wait","evidence":[],"error":null,"blocker":{"reason":"r","context":"c","question":"q","options":[{"id":"bad id","label":"l"}],"recommendation":"use l","kind":"choice"}}`,
	} {
		if _, err := r.workerResult(d, raw); err == nil {
			t.Fatalf("accepted malformed worker output: %s", raw)
		}
	}
	resume := swarm.ResumeTaskPayload{DependencyID: depID, DecisionID: decisionID, WorkerAgentID: "worker-a", Dispatch: swarm.DispatchPayload{Goal: "finish", Scope: "given answer", ExpectedResult: []string{"done"}, Context: swarm.TaskContext{Text: "original task and decision", Refs: []swarm.ContextRef{}}, Profile: r.profile.wire(), AcceptBy: time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano), RunTimeoutSeconds: 60}}
	data, _ := json.Marshal(resume)
	raw, _ := json.Marshal(ownerOutput{Actions: []action{{Kind: "resume_task", Data: data}}, Reply: "Continuing"})
	_, messages, err := r.ownerActions(d, swarm.NewID(), string(raw))
	if err != nil || len(messages) != 1 || messages[0].Type != "task.resume" || messages[0].JobID != job || messages[0].AttemptID == attempt {
		t.Fatalf("resume action not constructed: %v %+v", err, messages)
	}
	if err := swarm.ValidateEnvelope(messages[0]); err != nil {
		t.Fatal(err)
	}
	dep.FeatureID = swarm.NewID()
	_, messages, err = r.ownerActions(d, swarm.NewID(), string(raw))
	if err == nil || messages != nil {
		t.Fatal("foreign dependency action escaped validation")
	}
	dep.FeatureID = feature
	dep.State = "denied"
	_, messages, err = r.ownerActions(d, swarm.NewID(), string(raw))
	if err == nil || messages != nil {
		t.Fatal("denied dependency resumed")
	}
	for _, tc := range []struct {
		kind string
		data any
		dep  swarm.Dependency
		want string
	}{
		{"request_human", swarm.HumanRequestPayload{DependencyID: depID, Blocker: blockerFixture()}, swarm.Dependency{ID: depID, FeatureID: feature, Kind: "job", State: "owner_resolution"}, "human.request"},
		{"resolve_dependency", swarm.ResolveDependencyPayload{DependencyID: depID, Resolution: "resolved from documented rule", Evidence: []swarm.Evidence{}}, swarm.Dependency{ID: depID, FeatureID: feature, Kind: "job", State: "owner_resolution"}, "dependency.resolve"},
		{"complete_step", swarm.CompleteStepPayload{DependencyID: depID, DecisionID: decisionID, Summary: "choice recorded"}, swarm.Dependency{ID: depID, FeatureID: feature, Kind: "owner_step", State: "resolved", DecisionID: decisionID}, "step.complete"},
	} {
		dep = tc.dep
		data, _ := json.Marshal(tc.data)
		output, _ := json.Marshal(ownerOutput{Actions: []action{{Kind: tc.kind, Data: data}}, Reply: "recorded"})
		_, messages, err = r.ownerActions(d, swarm.NewID(), string(output))
		if err != nil || len(messages) != 1 || messages[0].Type != tc.want {
			t.Fatalf("%s not produced: %v %+v", tc.kind, err, messages)
		}
		if err := swarm.ValidateEnvelope(messages[0]); err != nil {
			t.Fatal(err)
		}
	}
}
