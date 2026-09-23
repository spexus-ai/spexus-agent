package swarm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Dependency state is local execution authority. Backend state is a separate
// immutable fact and never launches a runner on its own.
func dependency(ctx context.Context, tx *sql.Tx, id string) (Dependency, error) {
	var raw []byte
	err := tx.QueryRowContext(ctx, "SELECT data FROM dependencies WHERE id=?", id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Dependency{}, wireError(404, "dependency_not_found")
	}
	if err != nil {
		return Dependency{}, err
	}
	var d Dependency
	err = json.Unmarshal(raw, &d)
	return d, err
}
func saveDependency(ctx context.Context, tx *sql.Tx, d Dependency) error {
	_, err := tx.ExecContext(ctx, "UPDATE dependencies SET state=?,data=? WHERE id=?", d.State, mustJSON(d), d.ID)
	return err
}
func (s *Store) blocked(ctx context.Context, tx *sql.Tx, a attemptRecord, e Envelope, p ResultPayload) error {
	if p.Blocker == nil {
		return wireError(400, "blocker_required")
	}
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM dependencies WHERE feature_id=? AND job_id=? AND state NOT IN ('continuation_scheduled','denied','cancelled')", a.FeatureID, a.JobID).Scan(&n); err != nil {
		return err
	}
	if n != 0 {
		return wireError(409, "dependency_already_open")
	}
	d := Dependency{ID: NewID(), FeatureID: a.FeatureID, Kind: "job", JobID: a.JobID, AttemptID: a.AttemptID, SourceMessageID: e.MessageID, State: "owner_resolution", Blocker: *p.Blocker, CreatedAt: s.stamp(), UpdatedAt: s.stamp()}
	d.BlockedWork = "Goal: " + a.Dispatch.Goal + "\nScope: " + a.Dispatch.Scope
	if len(d.BlockedWork) > 16*1024 {
		d.BlockedWork = "Goal: " + a.Dispatch.Goal
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO dependencies(id,feature_id,job_id,source_message_id,state,data) VALUES(?,?,?,?,?,?)", d.ID, d.FeatureID, d.JobID, d.SourceMessageID, d.State, mustJSON(d)); err != nil {
		return err
	}
	a.DependencyID = d.ID
	return saveAttempt(ctx, tx, a)
}
func (s *Store) humanRequest(ctx context.Context, tx *sql.Tx, e Envelope, p HumanRequestPayload) error {
	if s.cfg.Human == nil {
		return wireError(503, "human_backend_unconfigured")
	}
	var d Dependency
	var err error
	if p.DependencyID != "" {
		d, err = dependency(ctx, tx, p.DependencyID)
		if err != nil {
			return err
		}
		if d.FeatureID != e.FeatureID || d.State != "owner_resolution" {
			return wireError(409, "dependency_not_escalatable")
		}
	} else {
		contextWithWork := p.Context + "\nBlocked work: " + p.BlockedWork
		if len(contextWithWork) > 16*1024 {
			return wireError(400, "context_too_large")
		}
		d = Dependency{ID: NewID(), FeatureID: e.FeatureID, Kind: "owner_step", StepKey: p.StepKey, OriginTurnID: e.OwnerTurnID, SourceMessageID: e.MessageID, State: "owner_resolution", Blocker: p.Blocker, CreatedAt: s.stamp(), UpdatedAt: s.stamp()}
		d.BlockedWork = p.BlockedWork
		d.Blocker.Context = contextWithWork
		if _, err = tx.ExecContext(ctx, "INSERT INTO dependencies(id,feature_id,job_id,source_message_id,state,data) VALUES(?,?,?,?,?,?)", d.ID, d.FeatureID, "", d.SourceMessageID, d.State, mustJSON(d)); err != nil {
			return err
		}
	}
	if d.RequestID != "" {
		return wireError(409, "dependency_already_escalated")
	}
	f, err := feature(ctx, tx, d.FeatureID)
	if err != nil {
		return err
	}
	if f.Stopped {
		return wireError(409, "feature_stopped")
	}
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM backend_sync_operations WHERE status IN ('pending','retry','blocked')").Scan(&count); err != nil {
		return err
	}
	if count >= 1000 {
		return wireError(429, "backend_sync_full")
	}
	d.State = "human_pending"
	d.RequestID = NewID()
	if d.Kind == "job" {
		d.Blocker = p.Blocker
		contextWithWork := p.Context + "\nBlocked work: " + d.BlockedWork
		if len(contextWithWork) > 16*1024 {
			return wireError(400, "context_too_large")
		}
		d.Blocker.Context = contextWithWork
	}
	// Slack truncates long postMessage text. A human request must expose its
	// full UUID, context, options, recommendation and blocked work.
	if len(questionText(d)) > 35*1024 {
		return wireError(400, "slack_question_too_large")
	}
	d.UpdatedAt = s.stamp()
	if err = saveDependency(ctx, tx, d); err != nil {
		return err
	}
	type depDTO struct {
		ID           string `json:"id"`
		Kind         string `json:"kind"`
		JobID        string `json:"job_id,omitempty"`
		AttemptID    string `json:"attempt_id,omitempty"`
		StepKey      string `json:"step_key,omitempty"`
		OriginTurnID string `json:"origin_turn_id,omitempty"`
	}
	body := struct {
		EpicID         string        `json:"epic_id"`
		FeatureID      string        `json:"feature_id"`
		OwnerAgentID   string        `json:"owner_agent_id"`
		Dependency     depDTO        `json:"dependency"`
		Reason         string        `json:"reason"`
		Context        string        `json:"context"`
		Question       string        `json:"question"`
		Kind           string        `json:"kind"`
		Options        []HumanOption `json:"options"`
		Recommendation string        `json:"recommendation"`
		Slack          struct {
			WorkspaceID string `json:"workspace_id"`
			ChannelID   string `json:"channel_id"`
			ThreadTS    string `json:"thread_ts"`
		} `json:"slack"`
		AllowedResponders []string `json:"allowed_responders"`
		Source            struct {
			Kind      string `json:"kind"`
			MessageID string `json:"message_id,omitempty"`
			TurnID    string `json:"turn_id,omitempty"`
		} `json:"source"`
	}{EpicID: s.cfg.Human.EpicID, FeatureID: f.FeatureID, OwnerAgentID: f.OwnerAgentID, Dependency: depDTO{ID: d.ID, Kind: d.Kind, JobID: d.JobID, AttemptID: d.AttemptID, StepKey: d.StepKey, OriginTurnID: d.OriginTurnID}, Reason: p.Reason, Context: d.Blocker.Context, Question: p.Question, Kind: p.Kind, Options: p.Options, Recommendation: p.Recommendation, AllowedResponders: f.AllowedActorIDs}
	body.Slack.WorkspaceID = s.cfg.Human.WorkspaceID
	body.Slack.ChannelID = f.ChannelID
	body.Slack.ThreadTS = f.ThreadTS
	if d.Kind == "job" {
		body.Source.Kind = "worker"
		body.Source.MessageID = d.SourceMessageID
	} else {
		body.Source.Kind = "owner"
		body.Source.TurnID = e.OwnerTurnID
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO backend_sync_operations(operation_id,request_id,kind,status,payload) VALUES(?,?,?,?,?)", d.RequestID, d.RequestID, "create", "pending", mustJSON(body))
	return err
}
func (s *Store) resolveDependency(ctx context.Context, tx *sql.Tx, e Envelope, p ResolveDependencyPayload) error {
	d, err := dependency(ctx, tx, p.DependencyID)
	if err != nil {
		return err
	}
	if d.FeatureID != e.FeatureID || d.Kind != "job" || d.State != "owner_resolution" {
		return wireError(409, "dependency_not_resolvable")
	}
	if d.Blocker.Kind == "permission" || d.Blocker.Kind == "external_action" || d.Blocker.Kind == "choice" {
		return wireError(409, "human_decision_required")
	}
	f, err := feature(ctx, tx, d.FeatureID)
	if err != nil {
		return err
	}
	if f.Stopped {
		return wireError(409, "feature_stopped")
	}
	d.State = "resolved"
	d.Resolution = p.Resolution
	d.UpdatedAt = s.stamp()
	return saveDependency(ctx, tx, d)
}
func (s *Store) completeStep(ctx context.Context, tx *sql.Tx, e Envelope, p CompleteStepPayload) error {
	d, err := dependency(ctx, tx, p.DependencyID)
	if err != nil {
		return err
	}
	if d.FeatureID != e.FeatureID || d.Kind != "owner_step" || d.State != "resolved" || d.DecisionID != p.DecisionID {
		return wireError(409, "step_not_completable")
	}
	f, err := feature(ctx, tx, d.FeatureID)
	if err != nil {
		return err
	}
	if f.Stopped {
		return wireError(409, "feature_stopped")
	}
	d.State = "step_completed"
	d.Resolution = p.Summary
	d.UpdatedAt = s.stamp()
	return saveDependency(ctx, tx, d)
}
func (s *Store) canResume(ctx context.Context, tx *sql.Tx, e Envelope, p ResumeTaskPayload) (Dependency, error) {
	d, err := dependency(ctx, tx, p.DependencyID)
	if err != nil {
		return d, err
	}
	if d.FeatureID != e.FeatureID || d.Kind != "job" || d.JobID != e.JobID || d.State != "resolved" || d.ContinuationAttemptID != "" {
		return d, wireError(409, "dependency_not_resumable")
	}
	if d.RequestID != "" && (p.DecisionID == "" || d.DecisionID != p.DecisionID) || d.RequestID == "" && p.DecisionID != "" {
		return d, wireError(409, "decision_mismatch")
	}
	f, err := feature(ctx, tx, e.FeatureID)
	if err != nil {
		return d, err
	}
	if f.Stopped {
		return d, wireError(409, "feature_stopped")
	}
	var current string
	if err = tx.QueryRowContext(ctx, "SELECT current_attempt_id FROM jobs WHERE id=? AND feature_id=?", d.JobID, d.FeatureID).Scan(&current); err != nil {
		return d, err
	}
	if current != d.AttemptID {
		return d, wireError(409, "stale_dependency")
	}
	old, err := attempt(ctx, tx, current)
	if err != nil {
		return d, err
	}
	if old.State != "blocked" {
		return d, wireError(409, "attempt_not_blocked")
	}
	if d.RequestID != "" {
		var raw []byte
		if err = tx.QueryRowContext(ctx, "SELECT data FROM human_projections WHERE request_id=?", d.RequestID).Scan(&raw); err != nil {
			return d, err
		}
		var proj HumanProjection
		if err = json.Unmarshal(raw, &proj); err != nil {
			return d, err
		}
		if proj.BackendState != "answered" || proj.ApplicationStatus != "applied" {
			return d, wireError(409, "decision_not_applied")
		}
		var view struct {
			AllowedResponders []string `json:"allowed_responders"`
			Terminal          struct {
				Source struct {
					ActorID string `json:"actor_id"`
				} `json:"source"`
			} `json:"terminal"`
		}
		if err = json.Unmarshal(proj.View, &view); err != nil {
			return d, err
		}
		if !allowedActor(f, view.Terminal.Source.ActorID) || !containsResponder(view.AllowedResponders, view.Terminal.Source.ActorID) {
			return d, wireError(409, "actor_revoked")
		}
	}
	var barrier string
	err = tx.QueryRowContext(ctx, "SELECT reason FROM recovery_barriers WHERE feature_id=?", e.FeatureID).Scan(&barrier)
	if err == nil {
		return d, wireError(503, "recovery_pending")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return d, err
	}
	return d, nil
}
func containsResponder(ids []string, actor string) bool {
	for _, id := range ids {
		if id == actor {
			return true
		}
	}
	return false
}
func (s *Store) markResumed(ctx context.Context, tx *sql.Tx, d Dependency, e Envelope) error {
	d.State = "continuation_scheduled"
	d.ContinuationAttemptID = e.AttemptID
	d.UpdatedAt = s.stamp()
	return saveDependency(ctx, tx, d)
}
func (s *Store) cancelDependencies(ctx context.Context, tx *sql.Tx, featureID, actor, reason string) error {
	rows, err := tx.QueryContext(ctx, "SELECT data FROM dependencies WHERE feature_id=? AND state NOT IN ('cancelled','denied','continuation_scheduled','step_completed')", featureID)
	if err != nil {
		return err
	}
	var ds []Dependency
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			break
		}
		var d Dependency
		if err = json.Unmarshal(raw, &d); err != nil {
			break
		}
		ds = append(ds, d)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, d := range ds {
		d.State = "cancelled"
		d.UpdatedAt = s.stamp()
		if err = saveDependency(ctx, tx, d); err != nil {
			return err
		}
		if d.RequestID != "" {
			var raw []byte
			queryErr := tx.QueryRowContext(ctx, "SELECT data FROM slack_outbox WHERE id=?", d.RequestID).Scan(&raw)
			if queryErr == nil {
				var question SlackDelivery
				if err = json.Unmarshal(raw, &question); err != nil {
					return err
				}
				if question.Status == "queued" {
					question.Status = "suppressed"
					if _, err = tx.ExecContext(ctx, "UPDATE slack_outbox SET status=?,data=? WHERE id=?", question.Status, mustJSON(question), question.ID); err != nil {
						return err
					}
				}
			} else if !errors.Is(queryErr, sql.ErrNoRows) {
				return queryErr
			}
			op := struct {
				OperationID      string `json:"operation_id"`
				ExpectedRevision int    `json:"expected_revision"`
				Reason           string `json:"reason"`
				Source           struct {
					Kind    string `json:"kind"`
					EventID string `json:"event_id"`
					ActorID string `json:"actor_id"`
				} `json:"source"`
			}{OperationID: NewID(), ExpectedRevision: 1, Reason: reason}
			op.Source.Kind = "runtime"
			op.Source.EventID = NewID()
			op.Source.ActorID = actor
			if _, err = tx.ExecContext(ctx, "INSERT INTO backend_sync_operations(operation_id,request_id,kind,status,payload) VALUES(?,?,?,?,?)", op.OperationID, d.RequestID, "cancel", "pending", mustJSON(op)); err != nil {
				return err
			}
		}
	}
	return nil
}
func questionText(d Dependency) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Решение человека требуется для работы.\nЗапрос: %s\nПричина: %s\nКонтекст: %s\nВопрос: %s\nРекомендация: %s\nОжидает: ", d.RequestID, d.Blocker.Reason, d.Blocker.Context, d.Blocker.Question, d.Blocker.Recommendation)
	if d.Kind == "job" {
		fmt.Fprintf(&b, "%s (job %s, attempt %s)", d.BlockedWork, d.JobID, d.AttemptID)
	} else {
		fmt.Fprintf(&b, "%s (step %s)", d.BlockedWork, d.StepKey)
	}
	for _, o := range d.Blocker.Options {
		fmt.Fprintf(&b, "\n• %s — %s", o.ID, o.Label)
	}
	if len(d.Blocker.Options) > 0 {
		b.WriteString("\nВыберите вариант кнопкой ниже. Для отказа напишите в этом треде: Отказ: причина.")
	} else {
		b.WriteString("\nНапишите в этом треде: Ответ: ваш текст. Для отказа: Отказ: причина.")
	}
	return b.String()
}

// SetRecoveryBarrier is called only by the trusted Slack catchup coordinator.
// A failed/incomplete history scan keeps the barrier in place.
func (s *Store) SetRecoveryBarrier(ctx context.Context, featureID, reason string) error {
	if !uuid(featureID) || len(reason) > 1024 {
		return wireError(400, "invalid_barrier")
	}
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if _, err := feature(ctx, tx, featureID); err != nil {
			return err
		}
		if reason == "" {
			_, err := tx.ExecContext(ctx, "DELETE FROM recovery_barriers WHERE feature_id=?", featureID)
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO recovery_barriers(feature_id,reason) VALUES(?,?) ON CONFLICT(feature_id) DO UPDATE SET reason=excluded.reason", featureID, reason)
		return err
	})
	if err == nil && reason == "" {
		return s.ApplyPendingHuman(ctx)
	}
	return err
}
func barrier(ctx context.Context, tx *sql.Tx, featureID string) error {
	var reason string
	err := tx.QueryRowContext(ctx, "SELECT reason FROM recovery_barriers WHERE feature_id=?", featureID).Scan(&reason)
	if err == nil {
		return wireError(503, "recovery_pending")
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}
