package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var liveExecutionAdmission sync.Map

type ExecutionManager interface {
	ScheduleAccepted(context.Context, ManagedExecution) (ExecutionScheduleResult, error)
}

type ManagedExecution struct {
	Accepted  AcceptedSlackExecution
	Execute   func(context.Context, PreparedSlackEvent) error
	Callbacks ExecutionCallbacks
}

type ExecutionCallbacks struct {
	OnCompleted func(SlackTurnExecutionResult)
	OnFailed    func(error)
}

type ExecutionScheduleResult struct {
	ExecutionID string
	Started     bool
	Decision    ExecutionStartDecision
}

type ExecutionStartDecision struct {
	Start  bool
	Reason string
}

type ExecutionAdmissionSnapshot struct {
	RunningExecutions int
	RunningSessions   map[string]int
}

type ExecutionAdmissionPolicy interface {
	DecideStart(context.Context, ExecutionAdmissionSnapshot, ExecutionRequest) (ExecutionStartDecision, error)
}

type queuedExecutionManager struct {
	coordinator *SlackTurnCoordinator
	policy      ExecutionAdmissionPolicy
	mu          sync.Mutex
	queue       []scheduledExecution
	running     map[string]ExecutionRequest
}

type scheduledExecution struct {
	ctx       context.Context
	managed   ManagedExecution
	request   ExecutionRequest
	accepted  AcceptedSlackExecution
	callbacks ExecutionCallbacks
}

func NewExecutionManager(coordinator *SlackTurnCoordinator, policy ExecutionAdmissionPolicy) ExecutionManager {
	if policy == nil {
		policy = boundedExecutionAdmissionPolicy{globalLimit: defaultExecutionConcurrencyLimit}
	}
	return &queuedExecutionManager{
		coordinator: coordinator,
		policy:      policy,
		running:     make(map[string]ExecutionRequest),
	}
}

func (m *queuedExecutionManager) ScheduleAccepted(ctx context.Context, execution ManagedExecution) (ExecutionScheduleResult, error) {
	if err := ctx.Err(); err != nil {
		return ExecutionScheduleResult{}, err
	}
	if m == nil {
		return ExecutionScheduleResult{}, errors.New("execution manager is required")
	}
	if m.coordinator == nil {
		return ExecutionScheduleResult{}, errors.New("slack turn coordinator is required")
	}
	if m.coordinator.store == nil {
		return ExecutionScheduleResult{}, errors.New("runtime store is required")
	}
	if execution.Execute == nil {
		return ExecutionScheduleResult{}, errors.New("execution callback is required")
	}
	if execution.Accepted.Execution.ExecutionID == "" {
		return ExecutionScheduleResult{}, errors.New("execution id is required")
	}

	request, err := m.coordinator.store.LoadExecution(ctx, execution.Accepted.Execution.ExecutionID)
	if err != nil {
		return ExecutionScheduleResult{}, fmt.Errorf("load execution request %q: %w", execution.Accepted.Execution.ExecutionID, err)
	}
	if request.Status != ExecutionStateQueued {
		return ExecutionScheduleResult{}, fmt.Errorf("execution %q is not queued", request.ExecutionID)
	}

	scheduled := scheduledExecution{
		ctx:       ctx,
		managed:   execution,
		request:   request,
		accepted:  execution.Accepted,
		callbacks: execution.Callbacks,
	}

	toStart, decisions, err := m.enqueueAndCollectStarts(ctx, scheduled)
	if err != nil {
		return ExecutionScheduleResult{}, fmt.Errorf("decide execution startup for %q: %w", request.ExecutionID, err)
	}

	decision := decisions[request.ExecutionID]
	result := ExecutionScheduleResult{
		ExecutionID: request.ExecutionID,
		Started:     decision.Start,
		Decision:    decision,
	}

	for _, candidate := range toStart {
		go m.runAccepted(candidate)
	}

	return result, nil
}

func (m *queuedExecutionManager) enqueueAndCollectStarts(ctx context.Context, execution scheduledExecution) ([]scheduledExecution, map[string]ExecutionStartDecision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.queue = append(m.queue, execution)
	toStart, decisions, err := m.collectStartableLocked(ctx)
	if err != nil {
		m.removeQueuedExecutionLocked(execution.request.ExecutionID)
		return nil, nil, err
	}
	return toStart, decisions, nil
}

func (m *queuedExecutionManager) collectStartsAfterCompletion(ctx context.Context, executionID string) ([]scheduledExecution, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.running, executionID)
	liveExecutionAdmission.Delete(executionID)
	toStart, _, err := m.collectStartableLocked(ctx)
	if err != nil {
		return nil, err
	}
	return toStart, nil
}

func (m *queuedExecutionManager) collectStartableLocked(ctx context.Context) ([]scheduledExecution, map[string]ExecutionStartDecision, error) {
	snapshot, err := m.admissionSnapshotLocked(ctx)
	if err != nil {
		return nil, nil, err
	}

	ready := make([]scheduledExecution, 0, len(m.queue))
	decisions := make(map[string]ExecutionStartDecision, len(m.queue))
	remaining := make([]scheduledExecution, 0, len(m.queue))
	for _, candidate := range m.queue {
		decision, err := m.policy.DecideStart(ctx, snapshot, candidate.request)
		if err != nil {
			return nil, nil, err
		}
		decisions[candidate.request.ExecutionID] = decision
		if !decision.Start {
			remaining = append(remaining, candidate)
			continue
		}

		m.running[candidate.request.ExecutionID] = candidate.request
		liveExecutionAdmission.Store(candidate.request.ExecutionID, m)
		ready = append(ready, candidate)
		snapshot.RunningExecutions++
		snapshot.RunningSessions[candidate.request.SessionKey]++
	}
	m.queue = remaining

	return ready, decisions, nil
}

func (m *queuedExecutionManager) admissionSnapshotLocked(ctx context.Context) (ExecutionAdmissionSnapshot, error) {
	running := make(map[string]ExecutionRequest, len(m.running))
	for executionID, request := range m.running {
		running[executionID] = request
	}

	persisted, err := m.coordinator.store.ListExecutions(ctx, []ExecutionLifecycleState{ExecutionStateRunning})
	if err != nil {
		return ExecutionAdmissionSnapshot{}, err
	}
	for _, request := range persisted {
		running[request.ExecutionID] = request
	}

	snapshot := ExecutionAdmissionSnapshot{
		RunningExecutions: len(running),
		RunningSessions:   make(map[string]int, len(running)),
	}
	for _, request := range running {
		snapshot.RunningSessions[request.SessionKey]++
	}

	return snapshot, nil
}

func (m *queuedExecutionManager) removeQueuedExecutionLocked(executionID string) {
	filtered := m.queue[:0]
	for _, candidate := range m.queue {
		if candidate.request.ExecutionID == executionID {
			continue
		}
		filtered = append(filtered, candidate)
	}
	m.queue = filtered
}

func (m *queuedExecutionManager) releaseExecutionAdmission(ctx context.Context, executionID string) ([]scheduledExecution, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.running[executionID]; !ok {
		liveExecutionAdmission.Delete(executionID)
		return nil, nil
	}

	delete(m.running, executionID)
	liveExecutionAdmission.Delete(executionID)
	toStart, _, err := m.collectStartableLocked(ctx)
	if err != nil {
		return nil, err
	}
	return toStart, nil
}

func (m *queuedExecutionManager) runAccepted(execution scheduledExecution) {
	result, err := m.coordinator.ExecuteAccepted(execution.ctx, execution.accepted, execution.managed.Execute)
	next, _ := m.collectStartsAfterCompletion(execution.ctx, execution.request.ExecutionID)
	for _, candidate := range next {
		go m.runAccepted(candidate)
	}
	if err != nil {
		if execution.callbacks.OnFailed != nil {
			execution.callbacks.OnFailed(err)
		}
		return
	}
	if execution.callbacks.OnCompleted != nil {
		execution.callbacks.OnCompleted(result)
	}
}

func ReleaseExecutionAdmission(ctx context.Context, executionID string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if executionID == "" {
		return nil
	}

	managerAny, ok := liveExecutionAdmission.Load(executionID)
	if !ok {
		return nil
	}

	manager, ok := managerAny.(*queuedExecutionManager)
	if !ok || manager == nil {
		liveExecutionAdmission.Delete(executionID)
		return nil
	}

	next, err := manager.releaseExecutionAdmission(ctx, executionID)
	if err != nil {
		return err
	}
	for _, candidate := range next {
		go manager.runAccepted(candidate)
	}
	return nil
}

const defaultExecutionConcurrencyLimit = 2

type boundedExecutionAdmissionPolicy struct {
	globalLimit int
}

func (p boundedExecutionAdmissionPolicy) DecideStart(_ context.Context, snapshot ExecutionAdmissionSnapshot, request ExecutionRequest) (ExecutionStartDecision, error) {
	limit := p.globalLimit
	if limit < 1 {
		limit = 1
	}

	if snapshot.RunningSessions[request.SessionKey] > 0 {
		return ExecutionStartDecision{
			Start:  false,
			Reason: fmt.Sprintf("session key %q already running", request.SessionKey),
		}, nil
	}
	if snapshot.RunningExecutions >= limit {
		return ExecutionStartDecision{
			Start:  false,
			Reason: fmt.Sprintf("global concurrency limit reached (%d/%d running)", snapshot.RunningExecutions, limit),
		}, nil
	}

	return ExecutionStartDecision{Start: true}, nil
}
