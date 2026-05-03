package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

var ErrSlackEventAlreadyProcessed = errors.New("slack event already processed")

type SlackTurnCoordinator struct {
	store      Store
	lockOwner  string
	lease      time.Duration
	newExecID  func() string
	now        func() time.Time
	locks      sync.Map
	deliveries sync.Map
}

type AcceptedSlackExecution struct {
	Prepared   PreparedSlackEvent
	Execution  ExecutionRequest
	AcceptedAt time.Time
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
		newExecID: defaultExecutionID,
		now:       time.Now,
	}
}

func (c *SlackTurnCoordinator) SetExecutionIDGenerator(fn func() string) {
	if c == nil {
		return
	}
	if fn == nil {
		c.newExecID = defaultExecutionID
		return
	}
	c.newExecID = fn
}

func (c *SlackTurnCoordinator) Execute(ctx context.Context, prepared PreparedSlackEvent, execute func(context.Context, PreparedSlackEvent) error) (SlackTurnExecutionResult, error) {
	return c.executeWithClaim(ctx, prepared, false, execute)
}

func (c *SlackTurnCoordinator) Accept(ctx context.Context, prepared PreparedSlackEvent) (AcceptedSlackExecution, SlackTurnExecutionResult, error) {
	if err := ctx.Err(); err != nil {
		return AcceptedSlackExecution{}, SlackTurnExecutionResult{}, err
	}
	if c == nil {
		return AcceptedSlackExecution{}, SlackTurnExecutionResult{}, errors.New("slack turn coordinator is required")
	}
	if c.store == nil {
		return AcceptedSlackExecution{}, SlackTurnExecutionResult{}, errors.New("runtime store is required")
	}
	if prepared.ThreadTS == "" {
		return AcceptedSlackExecution{}, SlackTurnExecutionResult{}, errors.New("thread timestamp is required")
	}
	if prepared.SourceType == "" {
		return AcceptedSlackExecution{}, SlackTurnExecutionResult{}, errors.New("slack source type is required")
	}
	if prepared.DeliveryID == "" {
		return AcceptedSlackExecution{}, SlackTurnExecutionResult{}, errors.New("slack delivery id is required")
	}

	deliveryLock := c.deliveryLock(prepared.SourceType, prepared.DeliveryID)
	deliveryLock.Lock()
	dedupe, err := c.loadAcceptedDelivery(ctx, prepared)
	if err != nil {
		deliveryLock.Unlock()
		return AcceptedSlackExecution{}, SlackTurnExecutionResult{}, err
	}
	if dedupe.Status == "duplicate" {
		deliveryLock.Unlock()
		return AcceptedSlackExecution{}, SlackTurnExecutionResult{Duplicate: true}, nil
	}

	acceptedAt := dedupe.ReceivedAt
	dedupe.Status = "queued"
	dedupe.ProcessedAt = nil
	if err := c.store.SaveEventDedupe(ctx, dedupe); err != nil {
		deliveryLock.Unlock()
		return AcceptedSlackExecution{}, SlackTurnExecutionResult{}, fmt.Errorf("persist dedupe claim for %s/%q: %w", prepared.SourceType, prepared.DeliveryID, err)
	}
	deliveryLock.Unlock()

	lock := c.threadLock(prepared.ThreadTS)
	lock.Lock()
	defer lock.Unlock()

	state := prepared.ThreadState
	state.LastStatus = "queued"
	state.LastRequestID = prepared.DeliveryID
	state.UpdatedAt = acceptedAt
	threadLock := ThreadLock{
		ThreadTS:       prepared.ThreadTS,
		LockOwner:      c.lockOwner,
		LockedAt:       acceptedAt,
		LeaseExpiresAt: timePtr(acceptedAt.Add(c.lease)),
		UpdatedAt:      acceptedAt,
	}

	if err := c.store.SaveThreadState(ctx, state); err != nil {
		return AcceptedSlackExecution{}, SlackTurnExecutionResult{}, fmt.Errorf("persist thread state for %q: %w", prepared.ThreadTS, err)
	}
	if err := c.store.SaveThreadLock(ctx, threadLock); err != nil {
		return AcceptedSlackExecution{}, SlackTurnExecutionResult{}, fmt.Errorf("persist thread lock for %q: %w", prepared.ThreadTS, err)
	}

	execution := ExecutionRequest{
		ExecutionID: c.newExecID(),
		SourceType:  prepared.SourceType,
		DeliveryID:  prepared.DeliveryID,
		ChannelID:   prepared.ThreadState.ChannelID,
		ProjectName: prepared.Project.Name,
		SessionKey:  prepared.SessionName,
		ThreadTS:    prepared.ThreadTS,
		CommandText: prepared.Event.Text,
		Status:      ExecutionStateQueued,
		CreatedAt:   acceptedAt,
		UpdatedAt:   acceptedAt,
	}
	if err := c.store.CreateExecution(ctx, execution); err != nil {
		return AcceptedSlackExecution{}, SlackTurnExecutionResult{}, fmt.Errorf("persist execution request for %s/%q: %w", prepared.SourceType, prepared.DeliveryID, err)
	}

	return AcceptedSlackExecution{
			Prepared:   prepared,
			Execution:  execution,
			AcceptedAt: acceptedAt,
		}, SlackTurnExecutionResult{
			Executed:  true,
			ClaimedAt: acceptedAt,
		}, nil
}

func (c *SlackTurnCoordinator) loadAcceptedDelivery(ctx context.Context, prepared PreparedSlackEvent) (EventDedupe, error) {
	dedupe, err := c.store.LoadEventDedupe(ctx, prepared.SourceType, prepared.DeliveryID)
	if err == nil {
		if dedupe.Status == "acked" {
			return dedupe, nil
		}
		return EventDedupe{Status: "duplicate"}, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return EventDedupe{}, fmt.Errorf("load event dedupe %s/%q: %w", prepared.SourceType, prepared.DeliveryID, err)
	}

	return EventDedupe{
		SourceType: prepared.SourceType,
		DeliveryID: prepared.DeliveryID,
		ReceivedAt: c.now().UTC(),
	}, nil
}

func (c *SlackTurnCoordinator) ExecuteAccepted(ctx context.Context, accepted AcceptedSlackExecution, execute func(context.Context, PreparedSlackEvent) error) (SlackTurnExecutionResult, error) {
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
	if accepted.Prepared.ThreadTS == "" {
		return SlackTurnExecutionResult{}, errors.New("thread timestamp is required")
	}
	if accepted.Execution.ExecutionID == "" {
		return SlackTurnExecutionResult{}, errors.New("execution id is required")
	}

	startedAt := c.now().UTC()
	state := accepted.Prepared.ThreadState
	state.LastStatus = "processing"
	state.LastRequestID = accepted.Prepared.DeliveryID
	state.UpdatedAt = startedAt
	dedupe := EventDedupe{
		SourceType: accepted.Prepared.SourceType,
		DeliveryID: accepted.Prepared.DeliveryID,
		ReceivedAt: accepted.AcceptedAt,
		Status:     "processing",
	}

	if err := c.store.SaveThreadState(ctx, state); err != nil {
		_ = c.store.DeleteThreadLock(ctx, accepted.Prepared.ThreadTS)
		return SlackTurnExecutionResult{}, c.persistAcceptedStartupFailure(ctx, accepted, fmt.Errorf("persist thread state for %q: %w", accepted.Prepared.ThreadTS, err))
	}
	if err := c.store.SaveEventDedupe(ctx, dedupe); err != nil {
		_ = c.store.DeleteThreadLock(ctx, accepted.Prepared.ThreadTS)
		return SlackTurnExecutionResult{}, c.persistAcceptedStartupFailure(ctx, accepted, fmt.Errorf("persist dedupe claim for %s/%q: %w", accepted.Prepared.SourceType, accepted.Prepared.DeliveryID, err))
	}
	if err := c.store.UpdateExecutionState(ctx, ExecutionState{
		ExecutionID: accepted.Execution.ExecutionID,
		Status:      ExecutionStateRunning,
		StartedAt:   &startedAt,
		UpdatedAt:   startedAt,
	}); err != nil {
		_ = c.store.DeleteThreadLock(ctx, accepted.Prepared.ThreadTS)
		return SlackTurnExecutionResult{}, c.persistAcceptedStartupFailure(ctx, accepted, fmt.Errorf("persist running execution state for %q: %w", accepted.Execution.ExecutionID, err))
	}

	result := SlackTurnExecutionResult{
		Executed:  true,
		ClaimedAt: accepted.AcceptedAt,
	}

	execErr := execute(ctx, accepted.Prepared)
	completedAt := c.now().UTC()
	state.UpdatedAt = completedAt
	dedupe.ProcessedAt = &completedAt
	executionState := ExecutionState{
		ExecutionID: accepted.Execution.ExecutionID,
		UpdatedAt:   completedAt,
		CompletedAt: &completedAt,
	}

	if execErr != nil {
		state.LastStatus = "failed"
		dedupe.Status = "failed"
		executionState.Status = ExecutionStateFailed
		executionState.DiagnosticContext = execErr.Error()
	} else {
		state.LastStatus = "processed"
		dedupe.Status = "processed"
		executionState.Status = ExecutionStateSucceeded
		result.CompletedAt = completedAt
	}

	currentExecution, err := c.store.LoadExecution(ctx, accepted.Execution.ExecutionID)
	if err == nil && isTerminalExecutionState(currentExecution.Status) {
		executionState.Status = currentExecution.Status
		executionState.DiagnosticContext = currentExecution.DiagnosticContext
		executionState.StartedAt = currentExecution.StartedAt
		executionState.UpdatedAt = currentExecution.UpdatedAt
		executionState.CompletedAt = currentExecution.CompletedAt
		if currentExecution.Status != ExecutionStateSucceeded {
			result.CompletedAt = time.Time{}
		} else if currentExecution.CompletedAt != nil {
			result.CompletedAt = *currentExecution.CompletedAt
		}
	}

	if err := c.store.UpdateExecutionState(ctx, executionState); err != nil {
		_ = c.store.DeleteThreadLock(ctx, accepted.Prepared.ThreadTS)
		return SlackTurnExecutionResult{}, fmt.Errorf("persist terminal execution state for %q: %w", accepted.Execution.ExecutionID, err)
	}
	if err := c.store.SaveThreadState(ctx, state); err != nil {
		_ = c.store.DeleteThreadLock(ctx, accepted.Prepared.ThreadTS)
		return SlackTurnExecutionResult{}, fmt.Errorf("persist completed thread state for %q: %w", accepted.Prepared.ThreadTS, err)
	}
	if err := c.store.SaveEventDedupe(ctx, dedupe); err != nil {
		_ = c.store.DeleteThreadLock(ctx, accepted.Prepared.ThreadTS)
		return SlackTurnExecutionResult{}, fmt.Errorf("persist completed dedupe for %s/%q: %w", accepted.Prepared.SourceType, accepted.Prepared.DeliveryID, err)
	}
	if err := c.store.DeleteThreadLock(ctx, accepted.Prepared.ThreadTS); err != nil {
		return SlackTurnExecutionResult{}, fmt.Errorf("release thread lock for %q: %w", accepted.Prepared.ThreadTS, err)
	}

	if execErr != nil {
		return SlackTurnExecutionResult{}, execErr
	}
	return result, nil
}

func (c *SlackTurnCoordinator) persistAcceptedStartupFailure(ctx context.Context, accepted AcceptedSlackExecution, cause error) error {
	if cause == nil {
		return nil
	}

	failedAt := c.now().UTC()
	if err := c.store.UpdateExecutionState(ctx, ExecutionState{
		ExecutionID:       accepted.Execution.ExecutionID,
		Status:            ExecutionStateFailed,
		DiagnosticContext: cause.Error(),
		UpdatedAt:         failedAt,
		CompletedAt:       &failedAt,
	}); err != nil {
		return errors.Join(cause, fmt.Errorf("persist failed execution state for %q: %w", accepted.Execution.ExecutionID, err))
	}

	return cause
}

func (c *SlackTurnCoordinator) ClaimDelivery(ctx context.Context, sourceType, deliveryID string) (SlackTurnExecutionResult, error) {
	if err := ctx.Err(); err != nil {
		return SlackTurnExecutionResult{}, err
	}
	if c == nil {
		return SlackTurnExecutionResult{}, errors.New("slack turn coordinator is required")
	}
	if c.store == nil {
		return SlackTurnExecutionResult{}, errors.New("runtime store is required")
	}
	if sourceType == "" {
		return SlackTurnExecutionResult{}, errors.New("slack source type is required")
	}
	if deliveryID == "" {
		return SlackTurnExecutionResult{}, errors.New("slack delivery id is required")
	}

	lock := c.deliveryLock(sourceType, deliveryID)
	lock.Lock()
	defer lock.Unlock()

	if _, err := c.store.LoadEventDedupe(ctx, sourceType, deliveryID); err == nil {
		return SlackTurnExecutionResult{Duplicate: true}, nil
	} else if !errors.Is(err, ErrNotFound) {
		return SlackTurnExecutionResult{}, fmt.Errorf("load event dedupe %s/%q: %w", sourceType, deliveryID, err)
	}

	claimedAt := c.now().UTC()
	dedupe := EventDedupe{
		SourceType: sourceType,
		DeliveryID: deliveryID,
		ReceivedAt: claimedAt,
		Status:     "acked",
	}

	if err := c.store.SaveEventDedupe(ctx, dedupe); err != nil {
		return SlackTurnExecutionResult{}, fmt.Errorf("persist dedupe claim for %s/%q: %w", sourceType, deliveryID, err)
	}

	return SlackTurnExecutionResult{
		Executed:  true,
		ClaimedAt: claimedAt,
	}, nil
}

func (c *SlackTurnCoordinator) ExecuteClaimed(ctx context.Context, prepared PreparedSlackEvent, execute func(context.Context, PreparedSlackEvent) error) (SlackTurnExecutionResult, error) {
	return c.executeWithClaim(ctx, prepared, true, execute)
}

func (c *SlackTurnCoordinator) executeWithClaim(ctx context.Context, prepared PreparedSlackEvent, requireAckedClaim bool, execute func(context.Context, PreparedSlackEvent) error) (SlackTurnExecutionResult, error) {
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
	if prepared.SourceType == "" {
		return SlackTurnExecutionResult{}, errors.New("slack source type is required")
	}
	if prepared.DeliveryID == "" {
		return SlackTurnExecutionResult{}, errors.New("slack delivery id is required")
	}

	deliveryLock := c.deliveryLock(prepared.SourceType, prepared.DeliveryID)
	deliveryLock.Lock()
	dedupe, err := c.loadClaimedDelivery(ctx, prepared, requireAckedClaim)
	if err != nil {
		deliveryLock.Unlock()
		return SlackTurnExecutionResult{}, err
	}
	if dedupe.Status == "duplicate" {
		deliveryLock.Unlock()
		return SlackTurnExecutionResult{Duplicate: true}, nil
	}

	claimedAt := dedupe.ReceivedAt
	dedupe.Status = "processing"
	dedupe.ProcessedAt = nil
	if err := c.store.SaveEventDedupe(ctx, dedupe); err != nil {
		deliveryLock.Unlock()
		return SlackTurnExecutionResult{}, fmt.Errorf("persist dedupe claim for %s/%q: %w", prepared.SourceType, prepared.DeliveryID, err)
	}
	deliveryLock.Unlock()

	lock := c.threadLock(prepared.ThreadTS)
	lock.Lock()
	defer lock.Unlock()

	state := prepared.ThreadState
	state.LastStatus = "processing"
	state.LastRequestID = prepared.DeliveryID
	state.UpdatedAt = claimedAt
	threadLock := ThreadLock{
		ThreadTS:       prepared.ThreadTS,
		LockOwner:      c.lockOwner,
		LockedAt:       claimedAt,
		LeaseExpiresAt: timePtr(claimedAt.Add(c.lease)),
		UpdatedAt:      claimedAt,
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
			return SlackTurnExecutionResult{}, fmt.Errorf("persist failed dedupe for %s/%q: %w", prepared.SourceType, prepared.DeliveryID, err)
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
		return SlackTurnExecutionResult{}, fmt.Errorf("persist completed dedupe for %s/%q: %w", prepared.SourceType, prepared.DeliveryID, err)
	}
	if err := c.store.DeleteThreadLock(ctx, prepared.ThreadTS); err != nil {
		return SlackTurnExecutionResult{}, fmt.Errorf("release thread lock for %q: %w", prepared.ThreadTS, err)
	}

	return result, nil
}

func (c *SlackTurnCoordinator) loadClaimedDelivery(ctx context.Context, prepared PreparedSlackEvent, requireAckedClaim bool) (EventDedupe, error) {
	dedupe, err := c.store.LoadEventDedupe(ctx, prepared.SourceType, prepared.DeliveryID)
	if err == nil {
		if requireAckedClaim && dedupe.Status == "acked" {
			return dedupe, nil
		}
		return EventDedupe{Status: "duplicate"}, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return EventDedupe{}, fmt.Errorf("load event dedupe %s/%q: %w", prepared.SourceType, prepared.DeliveryID, err)
	}
	if requireAckedClaim {
		return EventDedupe{}, fmt.Errorf("missing claimed delivery for %s/%q", prepared.SourceType, prepared.DeliveryID)
	}

	return EventDedupe{
		SourceType: prepared.SourceType,
		DeliveryID: prepared.DeliveryID,
		ReceivedAt: c.now().UTC(),
	}, nil
}

func (c *SlackTurnCoordinator) threadLock(threadTS string) *sync.Mutex {
	value, _ := c.locks.LoadOrStore(threadTS, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func (c *SlackTurnCoordinator) deliveryLock(sourceType, deliveryID string) *sync.Mutex {
	key := sourceType + ":" + deliveryID
	value, _ := c.deliveries.LoadOrStore(key, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func timePtr(value time.Time) *time.Time {
	value = value.UTC()
	return &value
}

func defaultExecutionID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("exec-%d", time.Now().UTC().UnixNano())
	}
	return hex.EncodeToString(raw[:])
}
