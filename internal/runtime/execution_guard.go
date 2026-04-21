package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var ErrSlackEventAlreadyProcessed = errors.New("slack event already processed")

type SlackTurnCoordinator struct {
	store     Store
	lockOwner string
	lease     time.Duration
	now       func() time.Time
	locks     sync.Map
}

type SlackTurnExecutionResult struct {
	Executed    bool
	Duplicate   bool
	ClaimedAt   time.Time
	CompletedAt time.Time
}

func NewSlackTurnCoordinator(store Store, lockOwner string) *SlackTurnCoordinator {
	if lockOwner == "" {
		lockOwner = "spexus-agent"
	}

	return &SlackTurnCoordinator{
		store:     store,
		lockOwner: lockOwner,
		lease:     5 * time.Minute,
		now:       time.Now,
	}
}

func (c *SlackTurnCoordinator) Execute(ctx context.Context, prepared PreparedSlackEvent, execute func(context.Context, PreparedSlackEvent) error) (SlackTurnExecutionResult, error) {
	if err := ctx.Err(); err != nil {
		return SlackTurnExecutionResult{}, err
	}
	if c == nil {
		return SlackTurnExecutionResult{}, errors.New("slack turn coordinator is required")
	}
	if c.store == nil {
		return SlackTurnExecutionResult{}, errors.New("runtime store is required")
	}
	if execute == nil {
		return SlackTurnExecutionResult{}, errors.New("execution callback is required")
	}
	if prepared.ThreadTS == "" {
		return SlackTurnExecutionResult{}, errors.New("thread timestamp is required")
	}
	if prepared.Event.ID == "" {
		return SlackTurnExecutionResult{}, errors.New("slack event id is required")
	}

	lock := c.threadLock(prepared.ThreadTS)
	lock.Lock()
	defer lock.Unlock()

	if _, err := c.store.LoadEventDedupe(ctx, prepared.Event.ID); err == nil {
		return SlackTurnExecutionResult{Duplicate: true}, nil
	} else if !errors.Is(err, ErrNotFound) {
		return SlackTurnExecutionResult{}, fmt.Errorf("load event dedupe %q: %w", prepared.Event.ID, err)
	}

	claimedAt := c.now().UTC()
	dedupe := EventDedupe{
		SlackEventID: prepared.Event.ID,
		ReceivedAt:   claimedAt,
		Status:       "processing",
	}
	state := prepared.ThreadState
	state.LastStatus = "processing"
	state.LastRequestID = prepared.Event.ID
	state.UpdatedAt = claimedAt
	threadLock := ThreadLock{
		ThreadTS:       prepared.ThreadTS,
		LockOwner:      c.lockOwner,
		LockedAt:       claimedAt,
		LeaseExpiresAt: timePtr(claimedAt.Add(c.lease)),
		UpdatedAt:      claimedAt,
	}

	if err := c.store.SaveEventDedupe(ctx, dedupe); err != nil {
		return SlackTurnExecutionResult{}, fmt.Errorf("persist dedupe claim for event %q: %w", prepared.Event.ID, err)
	}
	if err := c.store.SaveThreadState(ctx, state); err != nil {
		return SlackTurnExecutionResult{}, fmt.Errorf("persist thread state for %q: %w", prepared.ThreadTS, err)
	}
	if err := c.store.SaveThreadLock(ctx, threadLock); err != nil {
		return SlackTurnExecutionResult{}, fmt.Errorf("persist thread lock for %q: %w", prepared.ThreadTS, err)
	}

	result := SlackTurnExecutionResult{
		Executed:  true,
		ClaimedAt: claimedAt,
	}

	execErr := execute(ctx, prepared)
	completedAt := c.now().UTC()
	state.UpdatedAt = completedAt
	dedupe.ProcessedAt = &completedAt

	if execErr != nil {
		state.LastStatus = "failed"
		dedupe.Status = "failed"
		if err := c.store.SaveThreadState(ctx, state); err != nil {
			_ = c.store.DeleteThreadLock(ctx, prepared.ThreadTS)
			return SlackTurnExecutionResult{}, fmt.Errorf("persist failed thread state for %q: %w", prepared.ThreadTS, err)
		}
		if err := c.store.SaveEventDedupe(ctx, dedupe); err != nil {
			_ = c.store.DeleteThreadLock(ctx, prepared.ThreadTS)
			return SlackTurnExecutionResult{}, fmt.Errorf("persist failed dedupe for event %q: %w", prepared.Event.ID, err)
		}
		_ = c.store.DeleteThreadLock(ctx, prepared.ThreadTS)
		return SlackTurnExecutionResult{}, execErr
	}

	state.LastStatus = "processed"
	dedupe.Status = "processed"
	result.CompletedAt = completedAt

	if err := c.store.SaveThreadState(ctx, state); err != nil {
		_ = c.store.DeleteThreadLock(ctx, prepared.ThreadTS)
		return SlackTurnExecutionResult{}, fmt.Errorf("persist completed thread state for %q: %w", prepared.ThreadTS, err)
	}
	if err := c.store.SaveEventDedupe(ctx, dedupe); err != nil {
		_ = c.store.DeleteThreadLock(ctx, prepared.ThreadTS)
		return SlackTurnExecutionResult{}, fmt.Errorf("persist completed dedupe for event %q: %w", prepared.Event.ID, err)
	}
	if err := c.store.DeleteThreadLock(ctx, prepared.ThreadTS); err != nil {
		return SlackTurnExecutionResult{}, fmt.Errorf("release thread lock for %q: %w", prepared.ThreadTS, err)
	}

	return result, nil
}

func (c *SlackTurnCoordinator) threadLock(threadTS string) *sync.Mutex {
	value, _ := c.locks.LoadOrStore(threadTS, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func timePtr(value time.Time) *time.Time {
	value = value.UTC()
	return &value
}
