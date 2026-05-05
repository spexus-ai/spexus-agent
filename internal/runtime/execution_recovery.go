package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const StartupRecoveryFailureReason = "execution was interrupted by a runtime restart and marked failed during startup recovery"

type ExecutionRecoveryResult struct {
	SourceType      string
	DeliveryID      string
	ChannelID       string
	ProjectName     string
	ExecutionID     string
	PreviousStatus  string
	RecoveredStatus string
	NotifiedSlack   bool
}

// ExecutionRecoveryNotifier delivers a terminal recovery message when a stale execution has a usable Slack thread anchor.
type ExecutionRecoveryNotifier func(context.Context, ExecutionState, string) error

type ExecutionRecoveryService struct {
	store  ExecutionRecoveryStore
	now    func() time.Time
	notify ExecutionRecoveryNotifier
}

func NewExecutionRecoveryService(store ExecutionRecoveryStore, notify ExecutionRecoveryNotifier) *ExecutionRecoveryService {
	return &ExecutionRecoveryService{
		store:  store,
		now:    time.Now,
		notify: notify,
	}
}

func (s *ExecutionRecoveryService) ReconcileNonTerminalExecutions(ctx context.Context) ([]ExecutionRecoveryResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.store == nil {
		return nil, nil
	}

	states, err := s.store.ListExecutionStatesByStatus(
		ctx,
		ExecutionStatusQueued,
		ExecutionStatusRunning,
		ExecutionStatusRendering,
	)
	if err != nil {
		return nil, err
	}

	results := make([]ExecutionRecoveryResult, 0, len(states))
	for _, state := range states {
		result, err := s.reconcileOne(ctx, state)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}

	return results, nil
}

func (s *ExecutionRecoveryService) reconcileOne(ctx context.Context, state ExecutionState) (ExecutionRecoveryResult, error) {
	if err := ctx.Err(); err != nil {
		return ExecutionRecoveryResult{}, err
	}

	now := s.now().UTC()
	reason := strings.TrimSpace(state.LastError)
	if reason == "" {
		reason = StartupRecoveryFailureReason
	}

	previousStatus := state.Status
	state.Status = ExecutionStatusFailed
	state.CompletedAt = timePtr(now)
	state.CancelledAt = nil
	state.LastError = reason
	state.UpdatedAt = now

	if err := s.store.SaveExecutionState(ctx, state); err != nil {
		return ExecutionRecoveryResult{}, fmt.Errorf("save reconciled execution state %q: %w", state.ExecutionID, err)
	}

	if threadTS := strings.TrimSpace(state.ThreadTS); threadTS != "" {
		threadState, err := s.store.LoadThreadState(ctx, threadTS)
		switch {
		case err == nil:
			threadState.LastStatus = ExecutionStatusFailed
			threadState.UpdatedAt = now
			if threadState.LastRequestID == "" {
				threadState.LastRequestID = state.DeliveryID
			}
			if err := s.store.SaveThreadState(ctx, threadState); err != nil {
				return ExecutionRecoveryResult{}, fmt.Errorf("save reconciled thread state %q: %w", threadTS, err)
			}
		case errors.Is(err, ErrNotFound):
		default:
			return ExecutionRecoveryResult{}, fmt.Errorf("load thread state %q for execution %q: %w", threadTS, state.ExecutionID, err)
		}

		if err := s.store.DeleteThreadLock(ctx, threadTS); err != nil && !errors.Is(err, ErrNotFound) {
			return ExecutionRecoveryResult{}, fmt.Errorf("delete thread lock %q for execution %q: %w", threadTS, state.ExecutionID, err)
		}
	}

	dedupe, err := s.store.LoadEventDedupe(ctx, state.SourceType, state.DeliveryID)
	switch {
	case err == nil:
		dedupe.Status = ExecutionStatusFailed
		dedupe.ProcessedAt = timePtr(now)
		if err := s.store.SaveEventDedupe(ctx, dedupe); err != nil {
			return ExecutionRecoveryResult{}, fmt.Errorf("save reconciled dedupe %s/%q: %w", state.SourceType, state.DeliveryID, err)
		}
	case errors.Is(err, ErrNotFound):
	default:
		return ExecutionRecoveryResult{}, fmt.Errorf("load dedupe %s/%q for execution %q: %w", state.SourceType, state.DeliveryID, state.ExecutionID, err)
	}

	notified := false
	if s.notify != nil && strings.TrimSpace(state.ChannelID) != "" && strings.TrimSpace(state.ThreadTS) != "" {
		if err := s.notify(ctx, state, reason); err != nil {
			return ExecutionRecoveryResult{}, fmt.Errorf("notify slack for reconciled execution %q: %w", state.ExecutionID, err)
		}
		notified = true
	}

	return ExecutionRecoveryResult{
		SourceType:      state.SourceType,
		DeliveryID:      state.DeliveryID,
		ChannelID:       state.ChannelID,
		ProjectName:     state.ProjectName,
		ExecutionID:     state.ExecutionID,
		PreviousStatus:  previousStatus,
		RecoveredStatus: state.Status,
		NotifiedSlack:   notified,
	}, nil
}
