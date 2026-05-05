package runtime

import (
	"context"
	"errors"
	"sync"
)

const DefaultExecutionConcurrency = 4

type ExecutionManagerConfig struct {
	GlobalConcurrency int
	LifecycleRecorder ExecutionLifecycleRecorder
}

type ExecutionManager struct {
	ctx           context.Context
	handler       func(context.Context, ExecutionRequest)
	maxConcurrent int
	lifecycle     ExecutionLifecycleRecorder

	mu       sync.Mutex
	active   int
	ready    []string
	sessions map[string]*executionSessionQueue
	wg       sync.WaitGroup
}

type executionSessionQueue struct {
	running bool
	pending []queuedExecution
}

type queuedExecution struct {
	ctx     context.Context
	request ExecutionRequest
}

func NewExecutionManager(ctx context.Context, cfg ExecutionManagerConfig, handler func(context.Context, ExecutionRequest)) *ExecutionManager {
	maxConcurrent := cfg.GlobalConcurrency
	if maxConcurrent <= 0 {
		maxConcurrent = DefaultExecutionConcurrency
	}
	if ctx == nil {
		ctx = context.Background()
	}

	return &ExecutionManager{
		ctx:           ctx,
		handler:       handler,
		maxConcurrent: maxConcurrent,
		lifecycle:     cfg.LifecycleRecorder,
		sessions:      make(map[string]*executionSessionQueue),
	}
}

func NewAsyncExecutionQueue(handler func(context.Context, ExecutionRequest)) *ExecutionManager {
	return NewExecutionManager(context.Background(), ExecutionManagerConfig{}, handler)
}

func (m *ExecutionManager) Wait() {
	if m == nil {
		return
	}
	m.wg.Wait()
}

func (m *ExecutionManager) Enqueue(ctx context.Context, request ExecutionRequest) error {
	if m != nil && m.ctx != nil {
		if err := m.ctx.Err(); err != nil {
			return err
		}
	}
	if ctx == nil {
		if m != nil && m.ctx != nil {
			ctx = m.ctx
		} else {
			ctx = context.Background()
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil {
		return errors.New("execution manager is required")
	}
	if m.handler == nil {
		return errors.New("execution manager handler is required")
	}
	if err := request.validateForEnqueue(); err != nil {
		return err
	}
	if m.lifecycle != nil {
		if err := m.lifecycle.RecordQueued(ctx, request); err != nil {
			return err
		}
	}

	m.mu.Lock()
	sessionKey := request.SessionKey()
	sessionQueue := m.sessions[sessionKey]
	if sessionQueue == nil {
		sessionQueue = &executionSessionQueue{}
		m.sessions[sessionKey] = sessionQueue
	}

	sessionQueue.pending = append(sessionQueue.pending, queuedExecution{
		ctx:     ctx,
		request: request,
	})
	if !sessionQueue.running && len(sessionQueue.pending) == 1 {
		m.ready = append(m.ready, sessionKey)
	}
	m.mu.Unlock()

	m.scheduleReady()
	return nil
}

func (m *ExecutionManager) scheduleReady() {
	for {
		sessionKey, execution, ok := m.dequeueReady()
		if !ok {
			return
		}

		m.wg.Add(1)
		go m.run(sessionKey, execution)
	}
}

func (m *ExecutionManager) dequeueReady() (string, queuedExecution, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for m.active < m.maxConcurrent && len(m.ready) > 0 {
		sessionKey := m.ready[0]
		m.ready = m.ready[1:]

		sessionQueue := m.sessions[sessionKey]
		if sessionQueue == nil {
			continue
		}
		if sessionQueue.running {
			continue
		}
		if len(sessionQueue.pending) == 0 {
			delete(m.sessions, sessionKey)
			continue
		}

		execution := sessionQueue.pending[0]
		sessionQueue.pending = sessionQueue.pending[1:]
		sessionQueue.running = true
		m.active++
		return sessionKey, execution, true
	}

	return "", queuedExecution{}, false
}

func (m *ExecutionManager) run(sessionKey string, execution queuedExecution) {
	defer m.wg.Done()
	defer m.complete(sessionKey)

	if execution.ctx == nil {
		if m.ctx != nil {
			execution.ctx = m.ctx
		} else {
			execution.ctx = context.Background()
		}
	}
	if execution.ctx.Err() != nil {
		return
	}
	if m.lifecycle != nil {
		if err := m.lifecycle.RecordRunning(execution.ctx, execution.request); err != nil {
			_ = m.lifecycle.RecordFailed(execution.ctx, execution.request, err)
			return
		}
	}

	m.handler(execution.ctx, execution.request)
}

func (m *ExecutionManager) complete(sessionKey string) {
	m.mu.Lock()
	if m.active > 0 {
		m.active--
	}

	sessionQueue := m.sessions[sessionKey]
	if sessionQueue != nil {
		sessionQueue.running = false
		if len(sessionQueue.pending) > 0 {
			m.ready = append(m.ready, sessionKey)
		} else {
			delete(m.sessions, sessionKey)
		}
	}
	m.mu.Unlock()

	m.scheduleReady()
}
