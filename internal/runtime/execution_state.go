package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	ExecutionStatusQueued    = "queued"
	ExecutionStatusRunning   = "running"
	ExecutionStatusRendering = "rendering"
	ExecutionStatusProcessed = "processed"
	ExecutionStatusFailed    = "failed"
	ExecutionStatusCancelled = "cancelled"
)

var ErrExecutionCancelled = errors.New("execution cancelled")

type ExecutionState struct {
	ExecutionID                string     `json:"executionId"`
	SourceType                 string     `json:"sourceType"`
	DeliveryID                 string     `json:"deliveryId"`
	ProjectName                string     `json:"projectName"`
	ChannelID                  string     `json:"channelId"`
	ThreadTS                   string     `json:"threadTs,omitempty"`
	SessionName                string     `json:"sessionName,omitempty"`
	Status                     string     `json:"status"`
	QueuedAt                   time.Time  `json:"queuedAt"`
	StartedAt                  *time.Time `json:"startedAt,omitempty"`
	RenderingStartedAt         *time.Time `json:"renderingStartedAt,omitempty"`
	CompletedAt                *time.Time `json:"completedAt,omitempty"`
	CancelledAt                *time.Time `json:"cancelledAt,omitempty"`
	UpdatedAt                  time.Time  `json:"updatedAt"`
	LastError                  string     `json:"lastError,omitempty"`
	PublisherCheckpointKind    string     `json:"publisherCheckpointKind,omitempty"`
	PublisherCheckpointSummary string     `json:"publisherCheckpointSummary,omitempty"`
	PublisherCheckpointAt      *time.Time `json:"publisherCheckpointAt,omitempty"`
}

type ExecutionCheckpoint struct {
	Kind    string
	Summary string
}

type ExecutionStateStore interface {
	SaveExecutionState(context.Context, ExecutionState) error
	LoadExecutionState(context.Context, string) (ExecutionState, error)
	LoadExecutionStateByDelivery(context.Context, string, string) (ExecutionState, error)
	LoadLatestExecutionByThread(context.Context, string) (ExecutionState, error)
}

type ExecutionRecoveryStore interface {
	Store
	ExecutionStateStore
	ListExecutionStatesByStatus(context.Context, ...string) ([]ExecutionState, error)
}

type ExecutionLifecycleRecorder interface {
	RecordQueued(context.Context, ExecutionRequest) error
	RecordRunning(context.Context, ExecutionRequest) error
	RecordFailed(context.Context, ExecutionRequest, error) error
}

type ExecutionLifecycleTracker struct {
	store ExecutionStateStore
	now   func() time.Time
}

func NewExecutionLifecycleTracker(store ExecutionStateStore) *ExecutionLifecycleTracker {
	return &ExecutionLifecycleTracker{
		store: store,
		now:   time.Now,
	}
}

func (t *ExecutionLifecycleTracker) RecordQueued(ctx context.Context, request ExecutionRequest) error {
	return t.updateByRequest(ctx, request, func(state *ExecutionState, now time.Time) {
		state.Status = ExecutionStatusQueued
		if state.QueuedAt.IsZero() {
			state.QueuedAt = now
		}
		state.UpdatedAt = now
	})
}

func (t *ExecutionLifecycleTracker) RecordRunning(ctx context.Context, request ExecutionRequest) error {
	return t.updateByRequest(ctx, request, func(state *ExecutionState, now time.Time) {
		state.Status = ExecutionStatusRunning
		if state.QueuedAt.IsZero() {
			state.QueuedAt = now
		}
		if state.StartedAt == nil {
			state.StartedAt = timePtr(now)
		}
		state.CompletedAt = nil
		state.CancelledAt = nil
		state.LastError = ""
		state.UpdatedAt = now
	})
}

func (t *ExecutionLifecycleTracker) RecordRendering(ctx context.Context, request ExecutionRequest, checkpoint ExecutionCheckpoint) error {
	return t.updateByRequest(ctx, request, func(state *ExecutionState, now time.Time) {
		state.Status = ExecutionStatusRendering
		if state.QueuedAt.IsZero() {
			state.QueuedAt = now
		}
		if state.StartedAt == nil {
			state.StartedAt = timePtr(now)
		}
		if state.RenderingStartedAt == nil {
			state.RenderingStartedAt = timePtr(now)
		}
		state.CompletedAt = nil
		state.CancelledAt = nil
		state.LastError = ""
		applyCheckpoint(state, checkpoint, now)
		state.UpdatedAt = now
	})
}

func (t *ExecutionLifecycleTracker) RecordProcessed(ctx context.Context, request ExecutionRequest, checkpoint ExecutionCheckpoint) error {
	return t.updateByRequest(ctx, request, func(state *ExecutionState, now time.Time) {
		state.Status = ExecutionStatusProcessed
		if state.QueuedAt.IsZero() {
			state.QueuedAt = now
		}
		if state.StartedAt == nil {
			state.StartedAt = timePtr(now)
		}
		if state.RenderingStartedAt == nil {
			state.RenderingStartedAt = timePtr(now)
		}
		state.CompletedAt = timePtr(now)
		state.CancelledAt = nil
		state.LastError = ""
		applyCheckpoint(state, checkpoint, now)
		state.UpdatedAt = now
	})
}

func (t *ExecutionLifecycleTracker) RecordFailed(ctx context.Context, request ExecutionRequest, execErr error) error {
	trimmed := ""
	if execErr != nil {
		trimmed = strings.TrimSpace(execErr.Error())
	}
	return t.updateByRequest(ctx, request, func(state *ExecutionState, now time.Time) {
		state.Status = ExecutionStatusFailed
		if state.QueuedAt.IsZero() {
			state.QueuedAt = now
		}
		if state.StartedAt == nil {
			state.StartedAt = timePtr(now)
		}
		state.CompletedAt = timePtr(now)
		state.CancelledAt = nil
		state.LastError = trimmed
		state.UpdatedAt = now
	})
}

func (t *ExecutionLifecycleTracker) RecordCancelled(ctx context.Context, request ExecutionRequest, reason string, checkpoint ExecutionCheckpoint) error {
	return t.updateByRequest(ctx, request, func(state *ExecutionState, now time.Time) {
		state.Status = ExecutionStatusCancelled
		if state.QueuedAt.IsZero() {
			state.QueuedAt = now
		}
		if state.StartedAt == nil {
			state.StartedAt = timePtr(now)
		}
		state.CompletedAt = timePtr(now)
		state.CancelledAt = timePtr(now)
		state.LastError = strings.TrimSpace(reason)
		applyCheckpoint(state, checkpoint, now)
		state.UpdatedAt = now
	})
}

func (t *ExecutionLifecycleTracker) RecordCancelledByThread(ctx context.Context, threadTS, reason string) error {
	if t == nil || t.store == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	threadTS = strings.TrimSpace(threadTS)
	if threadTS == "" {
		return fmt.Errorf("thread timestamp is required")
	}

	state, err := t.store.LoadLatestExecutionByThread(ctx, threadTS)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}

	now := t.now().UTC()
	state.Status = ExecutionStatusCancelled
	if state.QueuedAt.IsZero() {
		state.QueuedAt = now
	}
	if state.StartedAt == nil {
		state.StartedAt = timePtr(now)
	}
	state.CompletedAt = timePtr(now)
	state.CancelledAt = timePtr(now)
	state.LastError = strings.TrimSpace(reason)
	state.UpdatedAt = now

	return t.store.SaveExecutionState(ctx, state)
}

func (t *ExecutionLifecycleTracker) updateByRequest(ctx context.Context, request ExecutionRequest, mutate func(*ExecutionState, time.Time)) error {
	if t == nil || t.store == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := request.validateForEnqueue(); err != nil {
		return err
	}

	now := t.now().UTC()
	state, err := t.store.LoadExecutionStateByDelivery(ctx, request.SourceType, request.DeliveryID)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		state = NewExecutionStateFromRequest(request, now)
	}

	if state.ExecutionID == "" {
		state.ExecutionID = request.ExecutionID()
	}
	if state.SourceType == "" {
		state.SourceType = request.SourceType
	}
	if state.DeliveryID == "" {
		state.DeliveryID = request.DeliveryID
	}
	state.ProjectName = request.Project.Name
	state.ChannelID = request.ChannelID
	if request.ThreadTS != "" {
		state.ThreadTS = request.ThreadTS
	}
	if request.SessionName != "" {
		state.SessionName = request.SessionName
	}

	mutate(&state, now)
	return t.store.SaveExecutionState(ctx, state)
}

func NewExecutionStateFromRequest(request ExecutionRequest, queuedAt time.Time) ExecutionState {
	queuedAt = queuedAt.UTC()
	return ExecutionState{
		ExecutionID: request.ExecutionID(),
		SourceType:  request.SourceType,
		DeliveryID:  request.DeliveryID,
		ProjectName: request.Project.Name,
		ChannelID:   request.ChannelID,
		ThreadTS:    request.ThreadTS,
		SessionName: request.SessionName,
		Status:      ExecutionStatusQueued,
		QueuedAt:    queuedAt,
		UpdatedAt:   queuedAt,
	}
}

func applyCheckpoint(state *ExecutionState, checkpoint ExecutionCheckpoint, now time.Time) {
	if state == nil {
		return
	}
	if strings.TrimSpace(checkpoint.Kind) == "" && strings.TrimSpace(checkpoint.Summary) == "" {
		return
	}

	state.PublisherCheckpointKind = strings.TrimSpace(checkpoint.Kind)
	state.PublisherCheckpointSummary = strings.TrimSpace(checkpoint.Summary)
	state.PublisherCheckpointAt = timePtr(now)
}

func IsTerminalExecutionStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case ExecutionStatusProcessed, ExecutionStatusFailed, ExecutionStatusCancelled:
		return true
	default:
		return false
	}
}
