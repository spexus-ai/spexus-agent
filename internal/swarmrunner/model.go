package swarmrunner

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spexus-ai/spexus-agent/internal/config"
	"github.com/spexus-ai/spexus-agent/internal/harness"
	"github.com/spexus-ai/spexus-agent/internal/piadapter"
	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

type Model interface {
	Run(context.Context, string, string) (string, bool, error)
}
type ActivityModel interface {
	RunWithActivity(context.Context, string, string, func(string)) (string, bool, error)
}
type LaunchModel interface {
	RunWithLaunch(context.Context, string, string, func(string), func() error) (string, bool, error)
}
type piModel struct {
	adapter   *piadapter.Adapter
	workspace string
}

func newModel(c Config, p profile) (Model, error) {
	provider, model, _ := strings.Cut(p.Model, "/")
	constructor := piadapter.New
	if c.Role == "owner" {
		constructor = piadapter.NewResident
	}
	a, e := constructor(config.AgentProfile{ID: p.ID, Provider: provider, Model: model, Thinking: p.Reasoning, SystemPrompt: p.Prompt + "\n\n" + modelInstructions(c), Workspace: c.Workspace, SessionDirectory: filepath.Join(c.StateDirectory, "sessions"), Tools: p.Tools, Extensions: p.Extensions}, c.PiBinary)
	if e != nil {
		return nil, e
	}
	return &piModel{a, c.Workspace}, nil
}
func (m *piModel) Close() error { return m.adapter.Close() }
func (m *piModel) Run(ctx context.Context, key, input string) (string, bool, error) {
	return m.RunWithActivity(ctx, key, input, nil)
}
func (m *piModel) RunWithActivity(ctx context.Context, key, input string, report func(string)) (string, bool, error) {
	return m.RunWithLaunch(ctx, key, input, report, nil)
}
func (m *piModel) RunWithLaunch(ctx context.Context, key, input string, report func(string), launched func() error) (string, bool, error) {
	s, e := m.adapter.StartPrompt(ctx, harness.SessionRequest{ProjectPath: m.workspace, ChannelID: "swarm", ThreadTS: key, Prompt: input})
	if e != nil {
		return "", false, e
	}
	defer s.Close()
	if launched != nil {
		if err := launched(); err != nil {
			return "", false, err
		}
	}
	var final string
	cancelled := false
	lastPhase := ""
	activity := func(phase string) {
		if report != nil && phase != lastPhase {
			lastPhase = phase
			report(phase)
		}
	}
	for event := range s.Events() {
		switch event.Kind {
		case harness.EventAssistantThinking:
			activity("thinking")
		case harness.EventToolStarted:
			activity("tool")
		case harness.EventToolFinished:
			activity("thinking")
		case harness.EventAssistantMessageChunk:
			activity("responding")
		case harness.EventAssistantMessageFinal:
			activity("responding")
			final = event.Text
		case harness.EventSessionCancelled:
			cancelled = true
		}
	}
	e = s.Wait()
	completed := false
	if lifecycle, ok := s.(interface{ Completed() bool }); ok {
		completed = lifecycle.Completed()
	}
	return final, cancelled || (ctx.Err() != nil && !completed), e
}
func (c Config) session(d swarm.Delivery) string {
	key := c.TenantID + "/" + c.ProjectID + "/" + d.FeatureID + "/"
	if c.Role == "worker" {
		key += d.JobID + "/" + d.AttemptID
	} else {
		key += c.AgentID
	}
	return key
}

type workerOutput struct {
	Outcome  string           `json:"outcome"`
	Summary  string           `json:"summary"`
	Evidence []swarm.Evidence `json:"evidence"`
	Error    *swarm.TaskError `json:"error"`
	Blocker  *swarm.Blocker   `json:"blocker,omitempty"`
}
type ownerOutput struct {
	Actions []action `json:"actions"`
	Reply   string   `json:"reply"`
}

func plainSummary(raw string) (string, bool) {
	reply := strings.TrimSpace(raw)
	if reply == "" || len(reply) > 16*1024 || !utf8.ValidString(reply) || strings.HasPrefix(reply, "{") || strings.HasPrefix(reply, "[") || strings.HasPrefix(reply, "```") {
		return "", false
	}
	return reply, true
}

type action struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}
type dispatchAction struct {
	WorkerAgentID string `json:"worker_agent_id"`
	JobID         string `json:"job_id,omitempty"`
	swarm.DispatchPayload
}
type reviewAction struct {
	JobID     string `json:"job_id"`
	AttemptID string `json:"attempt_id"`
	swarm.ReviewPayload
}
type reviewPreflightError struct{ reason string }

func (e *reviewPreflightError) Error() string { return e.reason }

func reviewedTrigger(d swarm.Delivery, v swarm.JobView) bool {
	if d.Type != "task.result" || v.JobID != d.JobID || v.FeatureID != d.FeatureID || v.CurrentAttemptID != d.AttemptID {
		return false
	}
	for _, a := range v.Attempts {
		if a.AttemptID == d.AttemptID {
			return a.ResultMessageID == d.MessageID && a.Review == "accepted"
		}
	}
	return false
}

type reviewedResultSummary struct {
	JobID     string `json:"job_id"`
	AttemptID string `json:"attempt_id"`
	Outcome   string `json:"outcome"`
	Summary   string `json:"summary"`
}

// A replacement Pi process has no previous session memory. Read a bounded
// slice of this owner's durable inbox, then confirm each result against the
// coordinator before offering it as context for a summary-only recovery turn.
func (r *Runner) recentAcceptedResults(ctx context.Context, d swarm.Delivery) ([]reviewedResultSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := r.journal.db.QueryContext(ctx, `SELECT body FROM inbox WHERE seq<? AND type='task.result' ORDER BY seq DESC LIMIT 200`, d.MailboxSeq)
	if err != nil {
		return nil, err
	}
	var candidates []swarm.Delivery
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			break
		}
		var previous swarm.Delivery
		if err = json.Unmarshal(raw, &previous); err != nil {
			break
		}
		if previous.FeatureID == d.FeatureID && previous.MessageID != d.MessageID {
			candidates = append(candidates, previous)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	results := make([]reviewedResultSummary, 0, 8)
	for _, previous := range candidates {
		if len(results) == 8 || len(seen) == 16 {
			break
		}
		if seen[previous.JobID] {
			continue
		}
		seen[previous.JobID] = true
		job, err := r.job(ctx, previous.JobID)
		if err != nil {
			return nil, err
		}
		if !reviewedTrigger(previous, job) {
			continue
		}
		for _, attempt := range job.Attempts {
			if attempt.AttemptID == previous.AttemptID && attempt.Result != nil {
				results = append(results, reviewedResultSummary{JobID: job.JobID, AttemptID: attempt.AttemptID, Outcome: attempt.Result.Outcome, Summary: attempt.Result.Summary})
				break
			}
		}
	}
	return results, nil
}

// A model may remember an earlier result from its Pi session. Review is an
// immutable, one-per-attempt transition, so only the triggering result may be
// reviewed in this owner turn. The backend remains the final authority.
func (r *Runner) preflightReview(d swarm.Delivery, x reviewAction) (string, error) {
	if d.Type != "task.result" || x.JobID != d.JobID || x.AttemptID != d.AttemptID || x.ResultMessageID != d.MessageID {
		return "", &reviewPreflightError{reason: "review must target the current task.result only"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v, err := r.job(ctx, x.JobID)
	if err != nil {
		return "", err
	}
	if v.FeatureID != d.FeatureID || v.CurrentAttemptID != x.AttemptID {
		return "", &reviewPreflightError{reason: "review target is not the current attempt"}
	}
	for _, a := range v.Attempts {
		if a.AttemptID == x.AttemptID {
			if a.ResultMessageID != d.MessageID || a.Review != "pending" || a.State != "succeeded" && a.State != "failed" && a.State != "blocked" && a.State != "cancelled" && a.State != "interrupted" {
				return "", &reviewPreflightError{reason: "result was already reviewed or is not terminal"}
			}
			return a.AssignedAgentID, nil
		}
	}
	return "", &reviewPreflightError{reason: "review attempt is missing"}
}

type cancelAction struct {
	JobID     string `json:"job_id"`
	AttemptID string `json:"attempt_id"`
	swarm.CancelPayload
}

func (c Config) wireVersion() int {
	if c.WireVersion == 2 {
		return 2
	}
	return 1
}

func (r *Runner) envelope(d swarm.Delivery, typ, to string, payload any, cause *string) swarm.Envelope {
	b, _ := json.Marshal(payload)
	return swarm.Envelope{ProtocolVersion: r.cfg.wireVersion(), MessageID: swarm.NewID(), Type: typ, TenantID: r.cfg.TenantID, ProjectID: r.cfg.ProjectID, FeatureID: d.FeatureID, FromAgentID: r.cfg.AgentID, ToAgentID: to, JobID: d.JobID, AttemptID: d.AttemptID, CausationID: cause, SentAt: time.Now().UTC().Format(time.RFC3339Nano), Payload: b}
}
func ptr(s string) *string { return &s }
func (r *Runner) workerResult(d swarm.Delivery, raw string) (swarm.ResultPayload, error) {
	var o workerOutput
	var fields map[string]json.RawMessage
	if e := json.Unmarshal([]byte(raw), &fields); e != nil || fields == nil {
		return swarm.ResultPayload{}, errors.New("invalid worker JSON")
	}
	if e := requiredKeys([]byte(raw), "outcome", "summary", "evidence", "error"); e != nil {
		return swarm.ResultPayload{}, e
	}
	if e := decode([]byte(raw), &o); e != nil {
		return swarm.ResultPayload{}, e
	}
	if o.Outcome != "succeeded" && o.Outcome != "failed" && (o.Outcome != "blocked" || r.cfg.wireVersion() != 2) {
		return swarm.ResultPayload{}, errors.New("invalid worker outcome")
	}
	_, hasBlocker := fields["blocker"]
	if hasBlocker != (o.Outcome == "blocked") {
		return swarm.ResultPayload{}, errors.New("blocker field must appear exactly for blocked outcome")
	}
	p := swarm.ResultPayload{Outcome: o.Outcome, Summary: o.Summary, Evidence: o.Evidence, Error: o.Error, Blocker: o.Blocker, Origin: "worker"}
	e := swarm.ValidateEnvelope(r.envelope(d, "task.result", d.FromAgentID, p, ptr(d.MessageID)))
	return p, e
}
func (r *Runner) ownerActions(d swarm.Delivery, turn, raw string) (ownerOutput, []swarm.Envelope, error) {
	var o ownerOutput
	if e := requiredKeys([]byte(raw), "actions", "reply"); e != nil {
		return o, nil, e
	}
	if e := decode([]byte(raw), &o); e != nil {
		return o, nil, e
	}
	if o.Actions == nil || len(o.Actions) > 8 || len(o.Reply) > 16*1024 {
		return o, nil, errors.New("invalid owner output bounds")
	}
	if d.Type == "task.result" {
		// Reject malformed review evidence before any dependent read, as with
		// ordinary owner action validation.
		for _, a := range o.Actions {
			if a.Kind == "review" {
				var x reviewAction
				if err := decode(a.Data, &x); err != nil {
					return o, nil, err
				}
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		v, err := r.job(ctx, d.JobID)
		if err != nil {
			return o, nil, err
		}
		if reviewedTrigger(d, v) && len(o.Actions) != 0 {
			return o, nil, &reviewPreflightError{reason: "this task.result was already accepted; return actions:[] and only summarize the recorded outcome"}
		}
		if !reviewedTrigger(d, v) {
			for _, attempt := range v.Attempts {
				if attempt.AttemptID != d.AttemptID || attempt.ResultMessageID != d.MessageID || attempt.Result == nil || attempt.Result.Outcome != "succeeded" || attempt.Review != "pending" {
					continue
				}
				containsReview := false
				for _, a := range o.Actions {
					containsReview = containsReview || a.Kind == "review"
				}
				if !containsReview {
					return o, nil, &reviewPreflightError{reason: "a successful task.result needs an explicit review action before a reply or further dispatch"}
				}
			}
		}
	}
	result := make([]swarm.Envelope, 0, len(o.Actions))
	// Actions are published in this order, but the whole output must validate
	// before any publication. Track transitions planned earlier in this output
	// so resolve_dependency and resume_task can be one owner turn.
	planned := map[string]string{}
	reviewPlanned := false
	for _, a := range o.Actions {
		var m swarm.Envelope
		switch a.Kind {
		case "dispatch":
			var x dispatchAction
			if e := decode(a.Data, &x); e != nil {
				return o, nil, e
			}
			var target *targetProfile
			for i := range r.targets {
				if r.targets[i].AgentID == x.WorkerAgentID {
					target = &r.targets[i]
				}
			}
			if target == nil || r.targetProfile(x.WorkerAgentID, x.Profile) != nil {
				return o, nil, errors.New("profile_unavailable")
			}
			if x.AcceptBy == "" {
				x.AcceptBy = time.Now().UTC().Add(60 * time.Second).Format(time.RFC3339Nano)
			}
			if x.RunTimeoutSeconds == 0 {
				x.RunTimeoutSeconds = 600
			}
			m = r.envelope(d, "task.dispatch", x.WorkerAgentID, x.DispatchPayload, nil)
			m.JobID = x.JobID
			if m.JobID == "" {
				m.JobID = swarm.NewID()
			}
			m.AttemptID = swarm.NewID()
		case "review":
			var x reviewAction
			if e := decode(a.Data, &x); e != nil {
				return o, nil, e
			}
			if reviewPlanned {
				return o, nil, &reviewPreflightError{reason: "duplicate review in one owner output"}
			}
			to, e := r.preflightReview(d, x)
			if e != nil {
				return o, nil, e
			}
			reviewPlanned = true
			m = r.envelope(d, "task.review", to, x.ReviewPayload, ptr(x.ResultMessageID))
			m.JobID = x.JobID
			m.AttemptID = x.AttemptID
		case "cancel":
			var x cancelAction
			if e := decode(a.Data, &x); e != nil {
				return o, nil, e
			}
			to, e := r.workerFor(d.FeatureID, x.JobID, x.AttemptID)
			if e != nil {
				return o, nil, e
			}
			x.RequestedBy = r.cfg.AgentID
			m = r.envelope(d, "task.cancel", to, x.CancelPayload, ptr(d.MessageID))
			m.JobID = x.JobID
			m.AttemptID = x.AttemptID
		case "request_human":
			if r.cfg.wireVersion() != 2 {
				return o, nil, errors.New("human request requires wire v2")
			}
			var x swarm.HumanRequestPayload
			if e := decode(a.Data, &x); e != nil {
				return o, nil, e
			}
			if e := requiredKeys(a.Data, "reason", "context", "question", "options", "recommendation", "kind"); e != nil {
				return o, nil, e
			}
			if x.DependencyID != "" {
				dep, e := r.actionDependency(d, x.DependencyID)
				if e != nil {
					return o, nil, e
				}
				if state := plannedState(dep, planned); state != "owner_resolution" {
					return o, nil, errors.New("dependency is not escalatable")
				}
				planned[x.DependencyID] = "human_pending"
			}
			m = r.envelope(d, "human.request", "coordinator", x, nil)
			m.JobID, m.AttemptID = "", ""
		case "decide_human":
			if r.cfg.wireVersion() != 2 {
				return o, nil, errors.New("human response requires wire v2")
			}
			var x swarm.HumanRespondPayload
			if e := decode(a.Data, &x); e != nil {
				return o, nil, e
			}
			if e := requiredKeys(a.Data, "request_id", "source_message_ts", "kind"); e != nil {
				return o, nil, e
			}
			if d.Type == "agent.input" {
				var input swarm.InputPayload
				if e := json.Unmarshal(d.Payload, &input); e != nil {
					return o, nil, e
				}
				if input.HumanAction != nil && x.SourceMessageTS == input.Source.MessageTS {
					if x.RequestID != input.HumanAction.RequestID || x.Kind != "answer" {
						return o, nil, &reviewPreflightError{reason: "a Slack button can answer only its own active question"}
					}
					// A button carries exactly one trusted option and no free text.
					// Pi interprets whether to decide, but cannot add a second
					// response or substitute an option to the human click.
					x.OptionID = input.HumanAction.OptionID
					x.Text = ""
				}
			}
			m = r.envelope(d, "human.respond", "coordinator", x, ptr(d.MessageID))
			m.JobID, m.AttemptID = "", ""
		case "resolve_dependency":
			if r.cfg.wireVersion() != 2 {
				return o, nil, errors.New("dependency resolution requires wire v2")
			}
			var x swarm.ResolveDependencyPayload
			if e := decode(a.Data, &x); e != nil {
				return o, nil, e
			}
			dep, e := r.actionDependency(d, x.DependencyID)
			if e != nil {
				return o, nil, e
			}
			if dep.Kind != "job" || plannedState(dep, planned) != "owner_resolution" {
				return o, nil, errors.New("dependency is not self-resolvable")
			}
			planned[x.DependencyID] = "resolved"
			m = r.envelope(d, "dependency.resolve", "coordinator", x, nil)
			m.JobID, m.AttemptID = "", ""
		case "resume_task":
			if r.cfg.wireVersion() != 2 {
				return o, nil, errors.New("task resume requires wire v2")
			}
			var x swarm.ResumeTaskPayload
			if e := decode(a.Data, &x); e != nil {
				return o, nil, e
			}
			dep, e := r.actionDependency(d, x.DependencyID)
			if e != nil {
				return o, nil, e
			}
			if dep.Kind != "job" || plannedState(dep, planned) != "resolved" || dep.JobID == "" || dep.AttemptID == "" || dep.DecisionID != x.DecisionID {
				return o, nil, errors.New("dependency is not resumable")
			}
			if e := r.targetProfile(x.WorkerAgentID, x.Dispatch.Profile); e != nil {
				return o, nil, e
			}
			if x.Dispatch.AcceptBy == "" {
				x.Dispatch.AcceptBy = time.Now().UTC().Add(60 * time.Second).Format(time.RFC3339Nano)
			}
			if x.Dispatch.RunTimeoutSeconds == 0 {
				x.Dispatch.RunTimeoutSeconds = 600
			}
			m = r.envelope(d, "task.resume", x.WorkerAgentID, x, nil)
			m.JobID, m.AttemptID = dep.JobID, swarm.NewID()
			planned[x.DependencyID] = "continuation_scheduled"
		case "complete_step":
			if r.cfg.wireVersion() != 2 {
				return o, nil, errors.New("step completion requires wire v2")
			}
			var x swarm.CompleteStepPayload
			if e := decode(a.Data, &x); e != nil {
				return o, nil, e
			}
			dep, e := r.actionDependency(d, x.DependencyID)
			if e != nil {
				return o, nil, e
			}
			if dep.Kind != "owner_step" || plannedState(dep, planned) != "resolved" || dep.DecisionID != x.DecisionID {
				return o, nil, errors.New("step is not completable")
			}
			planned[x.DependencyID] = "step_completed"
			m = r.envelope(d, "step.complete", "coordinator", x, nil)
			m.JobID, m.AttemptID = "", ""
		default:
			return o, nil, errors.New("unsupported owner action")
		}
		m.OwnerTurnID = turn
		if e := swarm.ValidateEnvelope(m); e != nil {
			return o, nil, e
		}
		result = append(result, m)
	}
	return o, result, nil
}
func plannedState(dep swarm.Dependency, planned map[string]string) string {
	if state := planned[dep.ID]; state != "" {
		return state
	}
	return dep.State
}
func (r *Runner) targetProfile(agent string, profile swarm.Profile) error {
	bound := false
	for _, target := range r.cfg.Targets {
		if target.AgentID == agent && target.ProfileID == profile.ID {
			bound = true
			break
		}
	}
	if !bound {
		return errors.New("profile_unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	active, err := r.activeProfile(ctx, profile.ID)
	if err != nil {
		return err
	}
	if active.wire() != profile {
		return errors.New("profile_changed_before_dispatch")
	}
	return nil
}
func (r *Runner) actionDependency(d swarm.Delivery, id string) (swarm.Dependency, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dep, err := r.dependency(ctx, id)
	if err != nil {
		return dep, err
	}
	if dep.FeatureID != d.FeatureID {
		return dep, errors.New("foreign dependency")
	}
	return dep, nil
}
func (r *Runner) workerFor(feature, job, attempt string) (string, error) {
	if !uuid.MatchString(job) || !uuid.MatchString(attempt) {
		return "", errors.New("invalid action identity")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v, e := r.job(ctx, job)
	if e != nil {
		return "", e
	}
	if v.FeatureID != feature || v.CurrentAttemptID != attempt {
		return "", errors.New("stale/foreign action")
	}
	for _, a := range v.Attempts {
		if a.AttemptID == attempt {
			return a.AssignedAgentID, nil
		}
	}
	return "", errors.New("attempt not found")
}
func modelError(code string) *swarm.TaskError {
	return &swarm.TaskError{Code: code, Message: code, Retryable: false}
}
func failedResult(code string) swarm.ResultPayload {
	return swarm.ResultPayload{Outcome: "failed", Summary: code, Evidence: []swarm.Evidence{}, Error: modelError(code), Origin: "worker"}
}
func (r *Runner) input(ctx context.Context, d swarm.Delivery) (string, error) {
	if r.cfg.Role == "worker" {
		var p swarm.DispatchPayload
		var resume *swarm.ResumeTaskPayload
		if d.Type == "task.resume" {
			var x swarm.ResumeTaskPayload
			if e := decode(d.Payload, &x); e != nil {
				return "", e
			}
			resume = &x
			p = x.Dispatch
		} else if e := decode(d.Payload, &p); e != nil {
			return "", e
		}
		v := struct {
			JobID     string                   `json:"job_id"`
			AttemptID string                   `json:"attempt_id"`
			Task      swarm.DispatchPayload    `json:"task"`
			Resume    *swarm.ResumeTaskPayload `json:"resume,omitempty"`
		}{d.JobID, d.AttemptID, p, resume}
		b, e := json.Marshal(v)
		return string(b), e
	}
	var job *swarm.JobView
	var dep *swarm.Dependency
	if d.JobID != "" {
		v, e := r.job(ctx, d.JobID)
		if e != nil {
			return "", e
		}
		job = &v
		dep = v.Dependency
	} else if d.Type == "human.decision" {
		var decision swarm.HumanDecisionPayload
		if e := decode(d.Payload, &decision); e != nil {
			return "", e
		}
		v, e := r.dependency(ctx, decision.DependencyID)
		if e != nil {
			return "", e
		}
		if v.FeatureID != d.FeatureID || v.RequestID != decision.RequestID || v.DecisionID != decision.DecisionID {
			return "", errors.New("foreign decision dependency")
		}
		dep = &v
		if v.Kind == "job" {
			j, e := r.job(ctx, v.JobID)
			if e != nil {
				return "", e
			}
			if j.FeatureID != d.FeatureID {
				return "", errors.New("foreign decision job")
			}
			job = &j
		}
	}
	summaryOnly := job != nil && reviewedTrigger(d, *job)
	var recentResults []reviewedResultSummary
	if summaryOnly {
		var err error
		recentResults, err = r.recentAcceptedResults(ctx, d)
		if err != nil {
			return "", err
		}
	}
	v := struct {
		Event                 swarm.Delivery          `json:"event"`
		Targets               []targetProfile         `json:"available_workers"`
		Job                   *swarm.JobView          `json:"job,omitempty"`
		Dependency            *swarm.Dependency       `json:"dependency,omitempty"`
		RecentAcceptedResults []reviewedResultSummary `json:"recent_accepted_results,omitempty"`
	}{d, r.targets, job, dep, recentResults}
	b, e := json.Marshal(v)
	prompt := string(b)
	if summaryOnly {
		prompt += `
This task.result was already accepted in the trusted job state. This turn exists only to summarize the recorded work. recent_accepted_results, if present, contains brief prior results from the same feature that the coordinator confirms are accepted. It is a bounded recent list, not necessarily complete history. Return actions:[] and a concise nonempty reply grounded in the supplied event/job and those recorded results; do not review, dispatch, cancel, request a human decision, or perform any other action. The runtime will reject every action for this turn. Do not imply that a new action was executed.`
	}
	return prompt, e
}
