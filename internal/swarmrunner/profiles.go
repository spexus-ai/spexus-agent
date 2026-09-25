package swarmrunner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

func workerProfileReadFailure(err error) string {
	if errors.Is(err, errProfileDisabled) {
		return "profile_disabled_before_launch"
	}
	if transient(err) || errors.Is(err, context.DeadlineExceeded) {
		return "profile_backend_unavailable"
	}
	return "profile_unavailable"
}

func (r *Runner) activeProfile(ctx context.Context, id string) (profile, error) {
	var active swarm.ActiveProfile
	// Each attempt gets one bounded authoritative read. A transient backend
	// failure must finish visibly instead of retrying this queued input forever.
	if err := r.client.once(ctx, "GET", "/agent-profiles/"+id+"/active", nil, &active); err != nil {
		return profile{}, err
	}
	if id == "orchestrator" && (active.Slot != "owner" || active.Role != "owner") || id != "orchestrator" && (active.Slot != id || active.Role != "worker") {
		return profile{}, errors.New("profile slot or role mismatch")
	}
	p, err := profileFromActive(active, id, r.cfg.AvailableModels)
	if err != nil {
		return p, err
	}
	backendAllowed := false
	for _, model := range active.AllowedModels {
		if model == p.Model {
			backendAllowed = true
			break
		}
	}
	if !backendAllowed {
		return p, errors.New("profile model denied by backend policy")
	}
	return p, nil
}

func (r *Runner) claimProfile(ctx context.Context, d swarm.Delivery, p profile) (swarm.LaunchClaim, error) {
	ref := swarm.ExecutionRef{}
	if r.cfg.Role == "worker" {
		ref.WorkerAttemptID = d.AttemptID
	} else {
		ref.OwnerTurnID = d.OwnerTurnID
	}
	request := swarm.LaunchClaimRequest{SchemaVersion: 1, ExecutionRef: ref, ExpectedGeneration: p.Generation, ExpectedRevision: p.Revision}
	var claim swarm.LaunchClaim
	if err := r.client.call(ctx, "POST", "/agent-profiles/"+p.ID+"/launch-claims", request, &claim); err != nil {
		return claim, err
	}
	if claim.ProfileID != p.ID || claim.Generation != p.Generation || claim.Revision != p.Revision || claim.ExecutionRef != ref || claim.State != "starting" || !uuid.MatchString(claim.ClaimID) {
		return claim, errors.New("profile claim identity mismatch")
	}
	raw, err := base64.StdEncoding.DecodeString(claim.SnapshotBytesBase64)
	if err != nil || swarm.Digest(raw) != claim.Revision || string(raw) != string(p.Bytes) || string(claim.SnapshotJSON) != string(raw) {
		return claim, errors.New("profile claim snapshot mismatch")
	}
	if err := r.journal.pin(d.MailboxSeq, p, claim, ref); err != nil {
		return claim, err
	}
	if err := r.waitForLocalFixtureClaimRelease(ctx, d, p, claim); err != nil {
		return claim, err
	}
	return claim, nil
}

func (r *Runner) observeLaunch(ctx context.Context, d swarm.Delivery, p profile, claim swarm.LaunchClaim) error {
	request := swarm.LaunchObservationRequest{SchemaVersion: 1, ExecutionRef: claim.ExecutionRef, ClaimID: claim.ClaimID, Revision: p.Revision, Outcome: "launched"}
	var response struct {
		ProfileID string `json:"profile_id"`
		ClaimID   string `json:"claim_id"`
		Revision  string `json:"revision"`
		Outcome   string `json:"outcome"`
	}
	if err := r.client.call(ctx, "POST", "/agent-profiles/"+p.ID+"/observations", request, &response); err != nil {
		return err
	}
	if response.ProfileID != p.ID || response.ClaimID != claim.ClaimID || response.Revision != p.Revision || response.Outcome != "launched" {
		return errors.New("profile observation mismatch")
	}
	return r.journal.observed(d.MailboxSeq, claim.ClaimID)
}

// Only records with a durable physical-launch receipt are replayed. An
// unobserved claim in the starting state is never treated as a launch.
func (r *Runner) reconcileProfileObservations(ctx context.Context) error {
	pending, err := r.journal.pendingProfileObservations()
	if err != nil {
		return err
	}
	for _, record := range pending {
		ref := swarm.ExecutionRef{}
		if record.RefType == "worker_attempt" {
			ref.WorkerAttemptID = record.RefID
		} else if record.RefType == "owner_turn" {
			ref.OwnerTurnID = record.RefID
		} else {
			return errors.New("invalid retained execution type")
		}
		claim := swarm.LaunchClaim{ClaimID: record.ClaimID, ExecutionRef: ref}
		d := swarm.Delivery{MailboxSeq: record.Seq}
		p := profile{TextProfile: swarm.TextProfile{ID: record.ProfileID}, Revision: record.Revision}
		if err := r.observeLaunch(ctx, d, p, claim); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) requeueOwnerProfile(ctx context.Context, d swarm.Delivery, turnID string) error {
	var result struct {
		TurnID        string `json:"turn_id"`
		NewMailboxSeq int64  `json:"new_mailbox_seq"`
		Status        string `json:"status"`
	}
	if err := r.client.call(ctx, "POST", "/owner-turns/"+turnID+"/profile-requeue", struct{}{}, &result); err != nil {
		return err
	}
	if result.TurnID != turnID || result.NewMailboxSeq <= d.MailboxSeq || result.Status != "queued" {
		return errors.New("invalid owner requeue receipt")
	}
	return r.journal.state(d.MailboxSeq, "interrupted", "profile_prelaunch_requeued")
}

func (r *Runner) retryBlockedOwnerProfile(ctx context.Context) error {
	rows, err := r.journal.db.Query(`SELECT seq,turn_id,body FROM inbox WHERE state='profile_blocked' ORDER BY seq`)
	if err != nil {
		return err
	}
	type blockedInput struct {
		seq    int64
		turnID string
		body   []byte
	}
	var inputs []blockedInput
	for rows.Next() {
		var input blockedInput
		if err := rows.Scan(&input.seq, &input.turnID, &input.body); err != nil {
			rows.Close()
			return err
		}
		inputs = append(inputs, input)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, input := range inputs {
		if input.turnID == "" {
			if _, err := r.activeProfile(ctx, r.cfg.ProfileID); err != nil {
				continue
			}
			if err := r.journal.state(input.seq, "received", ""); err != nil {
				return err
			}
			continue
		}
		var d swarm.Delivery
		if err := json.Unmarshal(input.body, &d); err != nil {
			return err
		}
		if err := r.requeueOwnerProfile(ctx, d, input.turnID); err != nil {
			continue // No fresh 404 claim readback; retain the input for reconciliation.
		}
	}
	return nil
}

func (r *Runner) refreshTargets(ctx context.Context) error {
	result := make([]targetProfile, 0, len(r.cfg.Targets))
	for _, target := range r.cfg.Targets {
		p, err := r.activeProfile(ctx, target.ProfileID)
		if err != nil {
			var h *HTTPError
			if errors.Is(err, errProfileDisabled) || errors.As(err, &h) && h.Code == "DISABLED" || strings.Contains(err.Error(), "profile_unavailable") {
				continue
			}
			return fmt.Errorf("target %s: %w", target.AgentID, err)
		}
		result = append(result, targetProfile{AgentID: target.AgentID, Profile: p.wire()})
	}
	r.targets = result
	return nil
}
