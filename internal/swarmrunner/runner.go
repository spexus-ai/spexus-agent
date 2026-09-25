package swarmrunner

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

type Runner struct {
	cfg                    Config
	profile                profile
	targets                []targetProfile
	client                 *Client
	journal                *Journal
	model                  LaunchModel
	mu                     sync.Mutex
	active                 swarm.Delivery
	cancel                 context.CancelFunc
	activityPhase          string
	activityCh             chan struct{}
	fixtureClaimBarrierDir string
}

func New(c Config) (*Runner, error) {
	if e := c.validate(); e != nil {
		return nil, e
	}
	client, e := NewClient(c)
	if e != nil {
		return nil, e
	}
	if e = os.MkdirAll(c.Workspace, 0700); e != nil {
		return nil, e
	}
	r := &Runner{cfg: c, client: client, targets: []targetProfile{}, activityCh: make(chan struct{}, 1)}
	for _, target := range c.Targets {
		r.targets = append(r.targets, targetProfile{AgentID: target.AgentID})
	}
	j, e := OpenJournal(c.StateDirectory, c.wireVersion())
	if e != nil {
		return nil, e
	}
	r.journal = j
	if e = j.identity(c); e != nil {
		j.Close()
		return nil, e
	}
	return r, nil
}
func (r *Runner) Close() error {
	if m, ok := r.model.(interface{ Close() error }); ok {
		_ = m.Close()
	}
	return r.journal.Close()
}
func (r *Runner) Run(ctx context.Context) error {
	if e := r.journal.bind(r.cfg.InstanceID); e != nil {
		return e
	}
	if e := r.journal.recover(); e != nil {
		return e
	}
	if e := r.reconcileProfileObservations(ctx); e != nil {
		return e
	}
	if e := r.heartbeat(ctx); e != nil {
		return e
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	fail := make(chan error, 2)
	var wg sync.WaitGroup
	background := func(fn func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := fn(ctx); e != nil && ctx.Err() == nil {
				fail <- e
				cancel()
			}
		}()
	}
	background(r.control)
	background(func(ctx context.Context) error {
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-tick.C:
			case <-r.activityCh:
			}
			if e := r.heartbeat(ctx); e != nil {
				return e
			}
		}
	})
	defer func() { cancel(); wg.Wait() }()
	for ctx.Err() == nil {
		if r.cfg.Role == "owner" {
			if e := r.retryBlockedOwnerProfile(ctx); e != nil {
				return e
			}
		}
		if e := r.flush(ctx); e != nil {
			return e
		}
		if r.cfg.Role == "owner" {
			// Fetch newly queued urgent input before selecting an older local
			// inbox entry. The coordinator also guards owner start if an urgent
			// source races with this poll.
			if e := r.pollWithWait(ctx, "normal", 0); e != nil && ctx.Err() == nil {
				return e
			}
		}
		d, ok, e := r.journal.next()
		if e != nil {
			return e
		}
		if ok {
			if e = r.process(ctx, d); e != nil {
				return e
			}
			continue
		}
		if e = r.poll(ctx, "normal"); e != nil && ctx.Err() == nil {
			return e
		}
	}
	select {
	case e := <-fail:
		return e
	default:
		return ctx.Err()
	}
}
func (r *Runner) heartbeat(ctx context.Context) error {
	r.mu.Lock()
	active := r.active
	phase := r.activityPhase
	r.mu.Unlock()
	q := swarm.HeartbeatRequest{InstanceID: r.cfg.InstanceID}
	if r.cfg.Role == "worker" && active.AttemptID != "" {
		q.ActiveAttemptID = ptr(active.AttemptID)
		q.ActivityPhase = phase
	}
	if r.cfg.Role == "owner" && active.OwnerTurnID != "" {
		q.ActiveOwnerTurnID = ptr(active.OwnerTurnID)
		q.ActivityPhase = phase
	}
	var reply swarm.HeartbeatResponse
	return r.client.call(ctx, "POST", "/agents/self/heartbeat", q, &reply)
}

func (r *Runner) reportActivity(phase string) {
	r.mu.Lock()
	if r.active.OwnerTurnID == "" && r.active.AttemptID == "" || r.activityPhase == phase {
		r.mu.Unlock()
		return
	}
	r.activityPhase = phase
	r.mu.Unlock()
	select {
	case r.activityCh <- struct{}{}:
	default:
	}
}
func (r *Runner) poll(ctx context.Context, lane string) error {
	return r.pollWithWait(ctx, lane, 20)
}

func (r *Runner) pollWithWait(ctx context.Context, lane string, waitSeconds int) error {
	var response swarm.MailboxResponse
	if e := r.client.call(ctx, "GET", "/mailbox?lane="+lane+"&limit=20&wait_seconds="+strconv.Itoa(waitSeconds), nil, &response); e != nil {
		return e
	}
	for _, d := range response.Messages {
		if d.MailboxSeq <= 0 || d.ProtocolVersion != r.cfg.wireVersion() || d.TenantID != r.cfg.TenantID || d.ProjectID != r.cfg.ProjectID || d.ToAgentID != r.cfg.AgentID || swarm.ValidateEnvelope(d.Envelope) != nil {
			return errors.New("invalid addressed delivery")
		}
		if (lane == "control") != (d.Type == "task.cancel" || d.Type == "turn.cancel") {
			return errors.New("wrong mailbox lane")
		}
		if e := r.journal.receive(d); e != nil {
			return e
		}

		if lane == "control" {
			r.mu.Lock()
			active := r.active
			if r.cancel != nil && active.FeatureID == d.FeatureID && ((d.Type == "task.cancel" && active.AttemptID == d.AttemptID) || (d.Type == "turn.cancel" && active.OwnerTurnID == d.OwnerTurnID)) {
				r.cancel()
			}
			r.mu.Unlock()
			if e := r.journal.state(d.MailboxSeq, "applied", ""); e != nil {
				return e
			}
		}
		// Journal commit precedes ACK; a lost ACK only repeats durable receive.
		var ack swarm.AckResponse
		if e := r.client.call(ctx, "POST", "/acks", swarm.AckRequest{MailboxSeqs: []int64{d.MailboxSeq}}, &ack); e != nil {
			return e
		}
		if len(ack.Acked) != 1 || ack.Acked[0] != d.MailboxSeq {
			return errors.New("invalid ACK receipt")
		}
	}
	return nil
}
func (r *Runner) control(ctx context.Context) error {
	for ctx.Err() == nil {
		if e := r.poll(ctx, "control"); e != nil {
			return e
		}
	}
	return ctx.Err()
}
func (r *Runner) process(ctx context.Context, d swarm.Delivery) error {
	if d.ProtocolVersion != r.cfg.wireVersion() || d.TenantID != r.cfg.TenantID || d.ProjectID != r.cfg.ProjectID || d.ToAgentID != r.cfg.AgentID || swarm.ValidateEnvelope(d.Envelope) != nil {
		return errors.New("invalid addressed delivery")
	}
	switch d.Type {
	case "task.accepted", "task.started", "task.review":
		return r.journal.state(d.MailboxSeq, "applied", "")
	case "task.dispatch", "task.resume":
		if r.cfg.Role != "worker" {
			return errors.New("dispatch addressed to owner")
		}
		return r.worker(ctx, d)
	case "agent.input", "task.result", "human.decision":
		if r.cfg.Role != "owner" {
			return errors.New("owner trigger addressed to worker")
		}
		return r.owner(ctx, d)
	default:
		return errors.New("unsupported input")
	}
}
func (r *Runner) job(ctx context.Context, id string) (swarm.JobView, error) {
	var v swarm.JobView
	path := "/jobs/" + id + "?limit=100"
	if e := r.client.call(ctx, "GET", path, nil, &v); e != nil {
		return v, e
	}
	for v.NextCursor != nil {
		var page swarm.JobView
		if e := r.client.call(ctx, "GET", path+"&cursor="+url.QueryEscape(*v.NextCursor), nil, &page); e != nil {
			return v, e
		}
		v.Attempts = append(v.Attempts, page.Attempts...)
		v.NextCursor = page.NextCursor
	}
	return v, nil
}
func (r *Runner) dependency(ctx context.Context, id string) (swarm.Dependency, error) {
	var d swarm.Dependency
	if !uuid.MatchString(id) {
		return d, errors.New("invalid dependency identity")
	}
	if err := r.client.call(ctx, "GET", "/dependencies/"+id, nil, &d); err != nil {
		return d, err
	}
	if d.ID != id || d.FeatureID == "" {
		return d, errors.New("invalid dependency response")
	}
	return d, nil
}
func (r *Runner) current(ctx context.Context, d swarm.Delivery) (swarm.Attempt, error) {
	v, e := r.job(ctx, d.JobID)
	if e != nil {
		return swarm.Attempt{}, e
	}
	if v.FeatureID != d.FeatureID || v.CurrentAttemptID != d.AttemptID {
		return swarm.Attempt{}, errors.New("stale_attempt")
	}
	for _, a := range v.Attempts {
		if a.AttemptID == d.AttemptID && a.AssignedAgentID == r.cfg.AgentID {
			return a, nil
		}
	}
	return swarm.Attempt{}, errors.New("foreign_attempt")
}
func (r *Runner) run(ctx context.Context, d swarm.Delivery, input string, p profile, claim swarm.LaunchClaim, correction bool) (string, bool, error) {
	runctx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	r.active = d
	r.cancel = cancel
	r.activityPhase = ""
	r.mu.Unlock()
	defer func() {
		cancel()
		r.mu.Lock()
		r.active = swarm.Delivery{}
		r.cancel = nil
		r.activityPhase = ""
		r.mu.Unlock()
		select {
		case r.activityCh <- struct{}{}:
		default:
		}
	}()
	// A control may already be durable before the active process is registered.
	control, e := r.journal.cancelFor(d)
	if e != nil {
		return "", false, e
	}
	if control != nil {
		return "", true, nil
	}
	if correction {
		e = r.journal.correctionLaunch(d.MailboxSeq)
	} else {
		e = r.journal.launch(d.MailboxSeq)
	}
	if e != nil {
		return "", false, e
	}
	model := r.model
	if model == nil {
		model, e = newModel(r.cfg, p)
		if e != nil {
			return "", false, e
		}
		if closer, ok := model.(interface{ Close() error }); ok {
			defer closer.Close()
		}
	}
	var output string
	var cancelled bool
	sessionKey := r.cfg.session(d) + "/" + p.Revision
	launched := func() error {
		if err := r.journal.physicalLaunch(d.MailboxSeq, claim.ClaimID); err != nil {
			return err
		}
		return r.observeLaunch(runctx, d, p, claim)
	}
	output, cancelled, e = model.RunWithLaunch(runctx, sessionKey, input, r.reportActivity, launched)
	return output, cancelled, e
}
func (r *Runner) worker(ctx context.Context, d swarm.Delivery) error {
	if e := r.journal.starting(d.MailboxSeq, ""); e != nil {
		return e
	}
	var dispatch swarm.DispatchPayload
	if d.Type == "task.resume" {
		var resume swarm.ResumeTaskPayload
		if e := decode(d.Payload, &resume); e != nil {
			return e
		}
		dispatch = resume.Dispatch
	} else if e := decode(d.Payload, &dispatch); e != nil {
		return e
	}
	currentProfile, e := r.activeProfile(ctx, r.cfg.ProfileID)
	if e != nil {
		return r.workerFinish(ctx, d, failedResult(workerProfileReadFailure(e)), d.MessageID)
	}
	if dispatch.Profile != currentProfile.wire() {
		return r.workerFinish(ctx, d, failedResult("profile_changed_before_launch"), d.MessageID)
	}
	accepted := r.envelope(d, "task.accepted", d.FromAgentID, swarm.AcceptedPayload{DispatchMessageID: d.MessageID, ProfileRevision: currentProfile.Revision}, ptr(d.MessageID))
	if e = r.journal.queue(d.MailboxSeq, "message", "/messages", accepted); e != nil {
		return e
	}
	if e = r.flush(ctx); e != nil {
		return e
	}
	if rejected, e := r.rejected(d.MailboxSeq); e != nil {
		return e
	} else if rejected {
		return r.rejectBeforeLaunch(ctx, d, "acceptance_rejected")
	}
	prelaunch, e := r.activeProfile(ctx, r.cfg.ProfileID)
	if e != nil {
		return r.workerFinish(ctx, d, failedResult(workerProfileReadFailure(e)), accepted.MessageID)
	}
	if prelaunch.wire() != dispatch.Profile || prelaunch.Generation != currentProfile.Generation {
		return r.workerFinish(ctx, d, failedResult("profile_changed_before_launch"), accepted.MessageID)
	}
	claim, e := r.claimProfile(ctx, d, prelaunch)
	if e != nil {
		var denied *HTTPError
		if errors.As(e, &denied) && denied.Code == "DISABLED" {
			return r.workerFinish(ctx, d, failedResult("profile_disabled_before_launch"), accepted.MessageID)
		}
		if errors.As(e, &denied) && denied.Status == 412 {
			return r.workerFinish(ctx, d, failedResult("profile_changed_before_launch"), accepted.MessageID)
		}
		return r.journal.state(d.MailboxSeq, "interrupted", "profile_claim_unknown")
	}
	started := r.envelope(d, "task.started", d.FromAgentID, swarm.StartedPayload{AcceptedMessageID: accepted.MessageID}, ptr(accepted.MessageID))
	if e = r.journal.queue(d.MailboxSeq, "message", "/messages", started); e != nil {
		return e
	}
	if e = r.flush(ctx); e != nil {
		return e
	}
	if rejected, e := r.rejected(d.MailboxSeq); e != nil {
		return e
	} else if rejected {
		return r.rejectBeforeLaunch(ctx, d, "start_rejected")
	}
	a, e := r.current(ctx, d)
	if e != nil {
		return r.journal.state(d.MailboxSeq, "interrupted", "prelaunch_state_unknown")
	}
	if a.State != "running" || a.CancelRequested {
		if a.CancelRequested && a.CancelMessageID != "" {
			return r.workerFinish(ctx, d, cancelResult(false, nil), a.CancelMessageID)
		}
		return r.journal.state(d.MailboxSeq, "interrupted", "prelaunch_not_running")
	}
	input, e := r.input(ctx, d)
	if e != nil {
		return e
	}
	raw, cancelled, runErr := r.run(ctx, d, input, prelaunch, claim, false)
	if e := r.journal.modelOutput(d.MailboxSeq, raw); e != nil {
		return e
	}
	var result swarm.ResultPayload
	if cancelled {
		result = cancelResult(false, nil)
	} else if runErr != nil {
		result = failedResult("model_execution_failed")
	} else {
		result, e = r.workerResult(d, raw)
		if e != nil && ctx.Err() == nil {
			correction := input + "\nYour preceding FINAL JSON failed strict validation. Return the complete corrected JSON object only. For a blocked result, blocker has exactly reason, context, question, options, recommendation and kind; never add dependency_id or blocked_work. The runtime creates IDs. Do not claim that the work was completed."
			corrected, wasCancelled, retryErr := r.run(ctx, d, correction, prelaunch, claim, true)
			cancelled, runErr = wasCancelled, retryErr
			if corrected != "" {
				if journalErr := r.journal.modelOutput(d.MailboxSeq, corrected); journalErr != nil {
					return journalErr
				}
			}
			if !cancelled && runErr == nil {
				result, e = r.workerResult(d, corrected)
			}
		}
		if cancelled {
			result = cancelResult(false, nil)
		} else if runErr != nil {
			result = failedResult("model_execution_failed")
		} else if e != nil {
			result = failedResult("model_output_invalid")
		}
	}
	cause := started.MessageID
	if cancelled {
		control, err := r.journal.cancelFor(d)
		if err != nil {
			return err
		}
		if control != nil {
			cause = control.MessageID
		} else {
			result = failedResult("runtime_stopped")
		}
	}
	// Commit output/outbox before any network reconciliation. A late cancellation
	// is reconciled by flush against the definitive coordinator rejection.

	return r.workerFinish(ctx, d, result, cause)
}
func (r *Runner) rejectBeforeLaunch(ctx context.Context, d swarm.Delivery, code string) error {
	a, e := r.current(ctx, d)
	if e == nil && a.CancelRequested && a.CancelMessageID != "" && (a.State == "queued" || a.State == "accepted" || a.State == "running") {
		// No Pi process has been started for this input, so cessation is certain.
		return r.workerFinish(ctx, d, cancelResult(false, nil), a.CancelMessageID)
	}
	return r.journal.state(d.MailboxSeq, "interrupted", code)
}
func cancelResult(completed bool, obs *swarm.Observation) swarm.ResultPayload {
	p := swarm.ResultPayload{Outcome: "cancelled", Summary: "Pi process stopped", Evidence: []swarm.Evidence{}, Error: modelError("cancelled"), Origin: "worker"}
	if completed {
		p.Outcome = "interrupted"
		p.Summary = "Pi completed before cancellation was observed"
		p.Error = modelError("cancel_after_completion")
		p.Observation = obs
	}
	return p
}
func (r *Runner) workerFinish(ctx context.Context, d swarm.Delivery, result swarm.ResultPayload, cause string) error {
	m := r.envelope(d, "task.result", d.FromAgentID, result, ptr(cause))
	if e := r.journal.output(d.MailboxSeq, result, []swarm.Envelope{m}, nil, ""); e != nil {
		return e
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return r.flush(ctx)
}
func (r *Runner) owner(ctx context.Context, d swarm.Delivery) error {
	currentProfile, e := r.activeProfile(ctx, r.cfg.ProfileID)
	if e != nil {
		return r.journal.state(d.MailboxSeq, "profile_blocked", "profile_unavailable")
	}
	turn := swarm.NewID()
	if e := r.journal.starting(d.MailboxSeq, turn); e != nil {
		return e
	}
	start := swarm.OwnerStartRequest{TurnID: turn, FeatureID: d.FeatureID, InputMailboxSeq: d.MailboxSeq}
	if e := r.journal.queue(d.MailboxSeq, "start", "/owner-turns/start", start); e != nil {
		return e
	}
	if e := r.flush(ctx); e != nil {
		return e
	}
	if rejected, code, e := r.startRejected(d.MailboxSeq); e != nil {
		return e
	} else if rejected {
		if code == "urgent_input_pending" {
			return r.journal.state(d.MailboxSeq, "received", code)
		}
		return r.journal.state(d.MailboxSeq, "interrupted", "owner_start_rejected")
	}
	var state swarm.OwnerTurn
	if e := r.client.call(ctx, "GET", "/owner-turns/"+turn, nil, &state); e != nil {
		return e
	}
	finish := swarm.OwnerFinishRequest{Outcome: "succeeded", Actions: []swarm.ActionReceipt{}, Observation: json.RawMessage("null")}
	d.OwnerTurnID = turn
	if state.State != "running" || state.CancelRequested {
		finish.Outcome = "cancelled"
		finish.Error = modelError("cancelled")
		return r.ownerFinish(ctx, d, turn, finish, nil)
	}
	prelaunch, e := r.activeProfile(ctx, r.cfg.ProfileID)
	if e != nil || prelaunch.wire() != currentProfile.wire() || prelaunch.Generation != currentProfile.Generation {
		if e = r.requeueOwnerProfile(ctx, d, turn); e != nil {
			return r.journal.state(d.MailboxSeq, "profile_blocked", "profile_prelaunch_unknown")
		}
		return nil
	}
	if e = r.refreshTargets(ctx); e != nil {
		if err := r.requeueOwnerProfile(ctx, d, turn); err != nil {
			return r.journal.state(d.MailboxSeq, "profile_blocked", "target_profile_unavailable")
		}
		return nil
	}
	claim, e := r.claimProfile(ctx, d, prelaunch)
	if e != nil {
		var denied *HTTPError
		if errors.As(e, &denied) && (denied.Status == 412 || denied.Code == "DISABLED") {
			if err := r.requeueOwnerProfile(ctx, d, turn); err != nil {
				return r.journal.state(d.MailboxSeq, "profile_blocked", "profile_denied_requeue_pending")
			}
			return nil
		}
		// A lost claim receipt may mean the claim committed. This execution
		// requires manual reconciliation, even if an immediate readback is 404.
		return r.journal.state(d.MailboxSeq, "interrupted", "profile_claim_unknown")
	}
	input, e := r.input(ctx, d)
	if e != nil {
		return e
	}
	raw, cancelled, runErr := r.run(ctx, d, input, prelaunch, claim, false)
	if e := r.journal.modelOutput(d.MailboxSeq, raw); e != nil {
		return e
	}
	var messages []swarm.Envelope
	var ownerResult ownerOutput
	var outputErr error
	if !cancelled && runErr == nil {
		ownerResult, messages, outputErr = r.ownerActions(d, turn, raw)
		// An already accepted result permits only a public summary. Pi can
		// occasionally return that summary as prose despite the JSON contract.
		// Accept prose only in this action-free state; never parse actions from it.
		if outputErr != nil && d.Type == "task.result" {
			job, jobErr := r.job(ctx, d.JobID)
			if jobErr != nil {
				return jobErr
			}
			if reviewedTrigger(d, job) {
				if reply, ok := plainSummary(raw); ok {
					ownerResult = ownerOutput{Actions: []action{}, Reply: reply}
					messages, outputErr = nil, nil
				}
			}
		}
		var reviewErr *reviewPreflightError
		summaryMissing := false
		if outputErr == nil && d.Type == "task.result" && len(messages) == 0 && strings.TrimSpace(ownerResult.Reply) == "" {
			job, jobErr := r.job(ctx, d.JobID)
			if jobErr != nil {
				return jobErr
			}
			summaryMissing = reviewedTrigger(d, job)
		}
		if summaryMissing {
			outputErr = errors.New("empty final summary")
		}
		if (errors.As(outputErr, &reviewErr) || summaryMissing) && ctx.Err() == nil {
			correction := input + "\nYour preceding FINAL JSON was rejected before any actions were published: "
			if summaryMissing {
				correction += "The final reply was empty. This result was already accepted; return actions:[] and a concise, nonempty human-facing summary of the recorded results."
			} else {
				correction += reviewErr.Error() + ". Return a complete corrected FINAL JSON. For a pending task.result, review only this event's job_id, attempt_id and message_id; do not repeat reviews of earlier attempts. If the trusted job shows this result was already accepted, return actions:[] and only summarize the recorded outcome."
			}
			corrected, wasCancelled, retryErr := r.run(ctx, d, correction, prelaunch, claim, true)
			cancelled, runErr = wasCancelled, retryErr
			if corrected != "" {
				raw = corrected
				if err := r.journal.modelOutput(d.MailboxSeq, raw); err != nil {
					return err
				}
			}
			if !cancelled && runErr == nil {
				ownerResult, messages, outputErr = r.ownerActions(d, turn, corrected)
				if outputErr != nil && d.Type == "task.result" {
					job, jobErr := r.job(ctx, d.JobID)
					if jobErr != nil {
						return jobErr
					}
					if reviewedTrigger(d, job) {
						if reply, ok := plainSummary(corrected); ok {
							ownerResult = ownerOutput{Actions: []action{}, Reply: reply}
							messages, outputErr = nil, nil
						}
					}
				}
			}
		}
	}
	if cancelled {
		finish.Outcome = "cancelled"
		finish.Error = modelError("cancelled")
		// An urgent human message also cancels this turn while the feature keeps
		// running. The next owner turn will respond with the new context.
	} else if runErr != nil {
		finish.Outcome = "failed"
		finish.Error = modelError("model_execution_failed")
		finish.Reply = "Pi model execution failed."
	} else {
		if outputErr != nil {
			finish.Outcome = "failed"
			finish.Error = modelError("model_output_invalid")
			finish.Reply = "Pi returned invalid structured output; no actions were published."
		} else {
			finish.Reply = ownerPublicReply(ownerResult.Reply, messages)
		}
	}

	return r.ownerFinish(ctx, d, turn, finish, messages)
}

// The question and temporary thread status already explain an in-flight
// action. Keep only a standalone answer or a result review visible as a
// persistent reply; action failures get their own notice during flush.
func ownerPublicReply(reply string, messages []swarm.Envelope) string {
	for _, message := range messages {
		if message.Type != "task.review" {
			return ""
		}
	}
	return reply
}

func (r *Runner) ownerFinish(ctx context.Context, d swarm.Delivery, turn string, finish swarm.OwnerFinishRequest, messages []swarm.Envelope) error {
	if e := r.journal.output(d.MailboxSeq, finish, messages, &finish, turn); e != nil {
		return e
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return r.flush(ctx)
}
func (r *Runner) rejected(seq int64) (bool, error) {
	var n int
	e := r.journal.db.QueryRow(`SELECT count(*) FROM outbox WHERE seq=? AND status='rejected'`, seq).Scan(&n)
	return n > 0, e
}
func (r *Runner) startRejected(seq int64) (bool, string, error) {
	var status, code string
	e := r.journal.db.QueryRow(`SELECT status,code FROM outbox WHERE seq=? AND kind='start' ORDER BY id DESC LIMIT 1`, seq).Scan(&status, &code)
	return status == "rejected", code, e
}
func (r *Runner) flush(ctx context.Context) error {
	for {
		p, ok, e := r.journal.pending()
		if e != nil || !ok {
			return e
		}
		var receipt json.RawMessage
		body := p.Body
		if p.Kind == "finish" {
			var request swarm.OwnerFinishRequest
			if e = json.Unmarshal(body, &request); e != nil {
				return e
			}
			request.Actions, e = r.journal.actions(p.Seq)
			if e != nil {
				return e
			}
			var state swarm.OwnerTurn
			turnPath := strings.TrimSuffix(p.Path, "/finish")
			if e = r.client.call(ctx, "GET", turnPath, nil, &state); e != nil {
				return e
			}
			if state.CancelRequested && request.Outcome != "cancelled" {
				var raw string
				_ = r.journal.db.QueryRow(`SELECT coalesce(model_output,'') FROM inbox WHERE seq=?`, p.Seq).Scan(&raw)
				request.Outcome = "interrupted"
				request.Error = modelError("cancel_after_completion")
				request.Observation, _ = json.Marshal(map[string]any{"output": raw})
				request.Reply = "Pi completed while cancellation was pending; output and action receipts are preserved."
			} else {
				for _, a := range request.Actions {
					if a.Status == "rejected" {
						request.Outcome = "failed"
						request.Error = modelError("action_rejected")
						request.Reply = "Не удалось завершить действие. Уже сохранённые результаты не потеряны; вопрос остаётся открытым, если решение ещё не записано."
						break
					}
				}
			}

			body, e = json.Marshal(request)
			if e != nil {
				return e
			}
			if _, e = r.journal.db.Exec(`UPDATE outbox SET body=? WHERE id=?`, body, p.ID); e != nil {
				return e
			}
		}
		e = r.client.call(ctx, "POST", p.Path, body, &receipt)
		if e != nil {
			if ctx.Err() != nil || transient(e) {
				return e
			}
			var h *HTTPError
			if !errors.As(e, &h) {
				return e
			}
			if p.Kind == "finish" && h.Code == "cancel_requested" {
				// The turn was cancelled after pre-finish GET. Preserve the pending
				// finish and rebuild it with observation from the durable model output.
				continue
			}
			if p.Kind == "message" && h.Code == "cancel_requested" {
				var m swarm.Envelope
				if json.Unmarshal(body, &m) == nil && m.Type == "task.result" {
					if e = r.reconcileResult(ctx, p, m); e != nil {
						return e
					}
					continue
				}
			}
			if e = r.journal.settle(p.ID, "rejected", h.Code, nil); e != nil {
				return e
			}
			if p.Kind == "finish" {
				if e = r.journal.state(p.Seq, "interrupted", h.Code); e != nil {
					return e
				}
			}
		} else {
			if e = r.journal.settle(p.ID, "stored", "", receipt); e != nil {
				return e
			}
		}
		if e = r.journal.finishOutput(p.Seq); e != nil {
			return e
		}
	}
}
func (r *Runner) reconcileResult(ctx context.Context, p pending, m swarm.Envelope) error {
	d := swarm.Delivery{Envelope: m, MailboxSeq: p.Seq}
	a, e := r.current(ctx, d)
	if e != nil {
		return e
	}
	if !a.CancelRequested || a.CancelMessageID == "" {
		return errors.New("cancellation conflict without state")
	}
	var result swarm.ResultPayload
	if e = json.Unmarshal(m.Payload, &result); e != nil {
		return e
	}
	obs := &swarm.Observation{Outcome: result.Outcome, Summary: result.Summary, Evidence: result.Evidence, Error: result.Error}
	replacement := r.envelope(d, "task.result", m.ToAgentID, cancelResult(true, obs), ptr(a.CancelMessageID))
	raw, _ := json.Marshal(replacement)
	tx, e := r.journal.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec(`UPDATE outbox SET status='rejected',code='cancel_requested' WHERE id=?`, p.ID); e != nil {
		return e
	}
	if _, e = tx.Exec(`INSERT INTO outbox(seq,kind,path,body) VALUES(?,'message','/messages',?)`, p.Seq, raw); e != nil {
		return e
	}
	return tx.Commit()
}
