package swarmrunner

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

func TestOwnerPublicReplyKeepsAnswersButHidesActionChatter(t *testing.T) {
	tests := []struct {
		name    string
		actions []string
		want    string
	}{
		{name: "clarification without action", want: "Готов пояснить выбор."},
		{name: "final reviewed result", actions: []string{"task.review"}, want: "Готов пояснить выбор."},
		{name: "dispatch", actions: []string{"task.dispatch"}},
		{name: "human question", actions: []string{"task.review", "human.request"}},
		{name: "decision write", actions: []string{"human.respond"}},
		{name: "dependent resume", actions: []string{"task.resume"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			messages := make([]swarm.Envelope, len(test.actions))
			for i, kind := range test.actions {
				messages[i].Type = kind
			}
			if got := ownerPublicReply("Готов пояснить выбор.", messages); got != test.want {
				t.Fatalf("reply = %q, want %q", got, test.want)
			}
		})
	}
}

func TestWireOneReviewCannotFinishSilently(t *testing.T) {
	r, _ := runnerFixture(t, http.NotFoundHandler())
	r.cfg.Role, r.cfg.AgentID, r.cfg.ProfileID = "owner", "orchestrator", "orchestrator"
	d := dispatchFixture(r)
	d.Type = "task.result"
	review, _ := json.Marshal(reviewAction{JobID: job, AttemptID: attempt, ReviewPayload: swarm.ReviewPayload{ResultMessageID: d.MessageID, Verdict: "accepted", Reason: "verified", Evidence: []swarm.Evidence{}}})
	raw, _ := json.Marshal(ownerOutput{Actions: []action{{Kind: "review", Data: review}}, Reply: ""})
	_, _, err := r.ownerActions(d, swarm.NewID(), string(raw))
	if err == nil || !strings.Contains(err.Error(), "human-facing reply") {
		t.Fatalf("silent review accepted: %v", err)
	}
}

func TestFailedResultCannotLeaveOrphanJobOnRetry(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Method == http.MethodGet && strings.Contains(q.URL.Path, "/jobs/") {
			writeJSON(w, swarm.JobView{JobID: job, FeatureID: feature, CurrentAttemptID: attempt, Attempts: []swarm.Attempt{{AttemptID: attempt, State: "failed", Review: "pending", ResultMessageID: "60000000-0000-4000-8000-000000000001", Result: &swarm.ResultPayload{Outcome: "failed", Summary: "model_output_invalid", Evidence: []swarm.Evidence{}, Error: &swarm.TaskError{Code: "model_output_invalid", Message: "model_output_invalid"}}}}})
			return
		}
		http.NotFound(w, q)
	})
	r, _ := runnerFixture(t, handler)
	r.cfg.Role, r.cfg.AgentID, r.cfg.ProfileID = "owner", "orchestrator", "orchestrator"
	d := dispatchFixture(r)
	d.Type, d.MessageID = "task.result", "60000000-0000-4000-8000-000000000001"
	retry, _ := json.Marshal(dispatchAction{WorkerAgentID: "worker-b", JobID: "70000000-0000-4000-8000-000000000001"})
	raw, _ := json.Marshal(ownerOutput{Actions: []action{{Kind: "dispatch", Data: retry}}, Reply: ""})
	_, _, err := r.ownerActions(d, swarm.NewID(), string(raw))
	if err == nil || !strings.Contains(err.Error(), "same job_id") {
		t.Fatalf("orphaning retry accepted: %v", err)
	}
}

func TestPlainSummaryOnlyAcceptsBoundedProse(t *testing.T) {
	for _, test := range []struct {
		raw string
		ok  bool
	}{
		{"Фрагмент A готов.\nПроверка B прошла.", true},
		{"   ", false},
		{`{"actions":[{"kind":"dispatch"}]}`, false},
		{"```json\n{}\n```", false},
	} {
		_, ok := plainSummary(test.raw)
		if ok != test.ok {
			t.Errorf("plainSummary(%q) accepted=%v", test.raw, ok)
		}
	}
}
