package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/acpxadapter"
	"github.com/spexus-ai/spexus-agent/internal/config"
	"github.com/spexus-ai/spexus-agent/internal/registry"
	"github.com/spexus-ai/spexus-agent/internal/runtime"
	"github.com/spexus-ai/spexus-agent/internal/slack"
	"github.com/spexus-ai/spexus-agent/internal/storage"
)

type runtimeCommandHandler struct {
	configStore *config.FileStore
	adapter     acpxadapter.Adapter
	starter     runtimeStarter
	out         io.Writer
	mu          sync.Mutex
	loaded      bool
	snapshot    runtimeSnapshot
}

type runtimeStarter interface {
	Start(context.Context, runtime.Status) error
}

type runtimeSnapshot struct {
	status           runtime.Status
	projectByChannel map[string]registry.Project
}

type blockingRuntimeStarter struct{}

type runtimeStartOptions struct {
	debug bool
}

const rawSocketDebugEnvVar = "SPEXUS_AGENT_DEBUG_RAW_SOCKET"

func newRuntimeCommandHandler(out io.Writer) RuntimeHandler {
	if out == nil {
		out = io.Discard
	}
	return &runtimeCommandHandler{
		configStore: config.NewFileStore(""),
		adapter:     acpxadapter.NewCLIAdapter("", nil),
		out:         out,
	}
}

func (h *runtimeCommandHandler) Start(ctx context.Context, args []string) error {
	opts, err := parseRuntimeStartOptions(args)
	if err != nil {
		return err
	}

	if opts.debug {
		h.debugf("runtime.start: validating startup state")
	}

	snapshot, err := h.loadStartupSnapshot(ctx)
	if err != nil {
		if opts.debug {
			h.debugf("runtime.start: startup failed: %v", err)
		}
		return err
	}

	if opts.debug {
		h.debugf(
			"runtime.start: startup loaded config=%s storage=%s projects=%d active_threads=%d",
			snapshot.status.ConfigPath,
			snapshot.status.StoragePath,
			snapshot.status.ProjectCount,
			snapshot.status.ActiveThreads,
		)
	}

	h.mu.Lock()
	h.snapshot = snapshot
	h.loaded = true
	h.mu.Unlock()

	starter := h.starter
	if starter == nil {
		cfg, _, err := h.loadValidatedConfig(ctx)
		if err != nil {
			return err
		}
		starter, err = h.newForegroundRuntimeStarter(ctx, cfg, opts)
		if err != nil {
			return err
		}
	}

	if opts.debug {
		h.debugf("runtime.start: entering foreground runtime loop")
	}

	err = starter.Start(ctx, snapshot.statusCopy())
	if opts.debug {
		if errors.Is(err, context.Canceled) {
			h.debugf("runtime.start: shutdown signal received; exiting cleanly")
		} else if err != nil {
			h.debugf("runtime.start: runtime loop stopped with error: %v", err)
		} else {
			h.debugf("runtime.start: runtime loop stopped")
		}
	}

	return err
}

func (h *runtimeCommandHandler) Status(ctx context.Context, args []string) error {
	switch len(args) {
	case 0:
		status, err := h.currentStatus(ctx)
		if err != nil {
			return err
		}

		return writeJSON(h.out, status)
	case 1:
		executionID := strings.TrimSpace(args[0])
		if executionID == "" {
			return fmt.Errorf("runtime status requires a non-empty execution id")
		}

		report, err := h.loadExecutionStatus(ctx, executionID)
		if err != nil {
			return err
		}

		return writeJSON(h.out, report)
	default:
		return fmt.Errorf("runtime status accepts at most one execution id argument")
	}
}

func (h *runtimeCommandHandler) Reload(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("runtime reload does not accept arguments")
	}

	snapshot, err := h.reloadSnapshot(ctx)
	if err != nil {
		return err
	}

	report := runtime.ReloadReport{
		ReloadedAt: time.Now().UTC(),
		Status:     snapshot.statusCopy(),
	}
	report.Status.Message = "runtime foundation reloaded"

	return writeJSON(h.out, report)
}

func (h *runtimeCommandHandler) Doctor(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("runtime doctor does not accept arguments")
	}

	report, err := h.loadDoctorReport(ctx)
	if err != nil {
		return err
	}

	return writeJSON(h.out, report)
}

func (h *runtimeCommandHandler) Cancel(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("runtime cancel requires exactly one execution id or thread timestamp argument")
	}

	identifier := strings.TrimSpace(args[0])
	if identifier == "" {
		return fmt.Errorf("runtime cancel requires exactly one execution id or thread timestamp argument")
	}

	if _, _, err := h.loadValidatedConfig(ctx); err != nil {
		return err
	}

	storagePath, err := storage.DefaultPath()
	if err != nil {
		return err
	}

	store, err := storage.Open(ctx, storagePath)
	if err != nil {
		return err
	}
	defer func() {
		_ = store.Close()
	}()

	runtimeRepo := store.Runtime()
	if runtimeRepo == nil {
		return errors.New("runtime repository unavailable")
	}

	execution, err := runtimeRepo.LoadExecution(ctx, identifier)
	if err == nil {
		return h.cancelExecution(ctx, runtimeRepo, execution, nil, nil)
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return err
	}

	threadTS := identifier
	state, err := runtimeRepo.LoadThreadState(ctx, threadTS)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return h.writeCancelNoOp(ctx, threadTS, "", "thread is inactive")
		}
		return err
	}

	lock, err := runtimeRepo.LoadThreadLock(ctx, threadTS)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return h.writeCancelNoOp(ctx, threadTS, state.SessionName, "thread is inactive")
		}
		return err
	}

	if strings.TrimSpace(state.LastStatus) != "processing" || lock.LockOwner == "" {
		return h.writeCancelNoOp(ctx, threadTS, state.SessionName, "thread is inactive")
	}

	adapter := h.adapter
	if adapter == nil {
		return errors.New("acpx adapter is required")
	}
	if err := adapter.Cancel(ctx, threadTS); err != nil {
		return err
	}

	cancelledAt := time.Now().UTC()
	execution, err = findRunningExecutionForThread(ctx, runtimeRepo, threadTS)
	if err != nil {
		if !errors.Is(err, runtime.ErrNotFound) {
			return err
		}
	} else {
		if err := runtimeRepo.UpdateExecutionState(ctx, runtime.ExecutionState{
			ExecutionID:       execution.ExecutionID,
			Status:            runtime.ExecutionStateCancelled,
			DiagnosticContext: "cancelled by operator",
			UpdatedAt:         cancelledAt,
			CompletedAt:       &cancelledAt,
		}); err != nil {
			return fmt.Errorf("persist cancelled execution state for %q: %w", execution.ExecutionID, err)
		}
	}

	state.LastStatus = "cancelled"
	state.UpdatedAt = cancelledAt
	if err := runtimeRepo.SaveThreadState(ctx, state); err != nil {
		return fmt.Errorf("persist cancelled thread state for %q: %w", threadTS, err)
	}
	if err := runtimeRepo.DeleteThreadLock(ctx, threadTS); err != nil {
		return fmt.Errorf("release thread lock for %q: %w", threadTS, err)
	}

	snapshot, err := h.reloadSnapshot(ctx)
	if err != nil {
		return err
	}

	report := runtime.CancelReport{
		RequestedAt: cancelledAt,
		ThreadTS:    threadTS,
		SessionName: state.SessionName,
		Result:      "cancelled",
		NoOp:        false,
		Message:     "active ACPX execution cancelled",
		Status:      snapshot.statusCopy(),
	}
	report.Status.Message = report.Message

	return writeJSON(h.out, report)
}

func (h *runtimeCommandHandler) cancelExecution(ctx context.Context, runtimeRepo runtime.Store, execution runtime.ExecutionRequest, state *runtime.ThreadState, lock *runtime.ThreadLock) error {
	if runtimeRepo == nil {
		return errors.New("runtime repository unavailable")
	}
	if execution.ExecutionID == "" {
		return errors.New("execution id is required")
	}
	if execution.Status != runtime.ExecutionStateRunning {
		sessionName := strings.TrimSpace(execution.SessionKey)
		if state != nil && strings.TrimSpace(state.SessionName) != "" {
			sessionName = state.SessionName
		}
		return h.writeCancelNoOp(ctx, execution.ThreadTS, sessionName, fmt.Sprintf("execution is %s", execution.Status))
	}
	if strings.TrimSpace(execution.ThreadTS) == "" {
		return h.persistCancelRoutingFailure(ctx, runtimeRepo, execution, "cancel routing failed: execution thread timestamp is unavailable")
	}

	if state == nil {
		loadedState, err := runtimeRepo.LoadThreadState(ctx, execution.ThreadTS)
		if err != nil {
			if !errors.Is(err, storage.ErrNotFound) {
				return err
			}
		} else {
			state = &loadedState
		}
	}
	if lock == nil {
		loadedLock, err := runtimeRepo.LoadThreadLock(ctx, execution.ThreadTS)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return h.persistCancelRoutingFailure(ctx, runtimeRepo, execution, fmt.Sprintf("cancel routing failed: execution owner unavailable for thread %q", execution.ThreadTS))
			}
			return err
		}
		lock = &loadedLock
	}
	if strings.TrimSpace(lock.LockOwner) == "" {
		return h.persistCancelRoutingFailure(ctx, runtimeRepo, execution, fmt.Sprintf("cancel routing failed: execution owner missing for thread %q", execution.ThreadTS))
	}

	adapter := h.adapter
	if adapter == nil {
		return h.persistCancelRoutingFailure(ctx, runtimeRepo, execution, "cancel routing failed: acpx adapter is required")
	}
	if err := adapter.Cancel(ctx, execution.ThreadTS); err != nil {
		return h.persistCancelRoutingFailure(ctx, runtimeRepo, execution, fmt.Sprintf("cancel routing failed via owner %q: %v", lock.LockOwner, err))
	}

	cancelledAt := time.Now().UTC()
	if err := runtimeRepo.UpdateExecutionState(ctx, runtime.ExecutionState{
		ExecutionID:       execution.ExecutionID,
		Status:            runtime.ExecutionStateCancelled,
		DiagnosticContext: "cancelled by operator",
		UpdatedAt:         cancelledAt,
		CompletedAt:       &cancelledAt,
	}); err != nil {
		return fmt.Errorf("persist cancelled execution state for %q: %w", execution.ExecutionID, err)
	}

	sessionName := strings.TrimSpace(execution.SessionKey)
	if state != nil {
		state.LastStatus = "cancelled"
		state.UpdatedAt = cancelledAt
		if err := runtimeRepo.SaveThreadState(ctx, *state); err != nil {
			return fmt.Errorf("persist cancelled thread state for %q: %w", execution.ThreadTS, err)
		}
		sessionName = state.SessionName
	}
	if err := runtimeRepo.DeleteThreadLock(ctx, execution.ThreadTS); err != nil {
		return fmt.Errorf("release thread lock for %q: %w", execution.ThreadTS, err)
	}

	snapshot, err := h.reloadSnapshot(ctx)
	if err != nil {
		return err
	}

	report := runtime.CancelReport{
		RequestedAt: cancelledAt,
		ThreadTS:    execution.ThreadTS,
		SessionName: sessionName,
		Result:      "cancelled",
		NoOp:        false,
		Message:     "active ACPX execution cancelled",
		Status:      snapshot.statusCopy(),
	}
	report.Status.Message = report.Message

	return writeJSON(h.out, report)
}

func (h *runtimeCommandHandler) persistCancelRoutingFailure(ctx context.Context, runtimeRepo runtime.Store, execution runtime.ExecutionRequest, diagnostic string) error {
	if runtimeRepo == nil {
		return errors.New("runtime repository unavailable")
	}
	diagnostic = strings.TrimSpace(diagnostic)
	if diagnostic == "" {
		diagnostic = "cancel routing failed"
	}

	failedAt := time.Now().UTC()
	if err := runtimeRepo.UpdateExecutionState(ctx, runtime.ExecutionState{
		ExecutionID:       execution.ExecutionID,
		Status:            runtime.ExecutionStateFailed,
		DiagnosticContext: diagnostic,
		StartedAt:         execution.StartedAt,
		UpdatedAt:         failedAt,
		CompletedAt:       &failedAt,
	}); err != nil {
		return fmt.Errorf("%s: persist execution diagnostic for %q: %w", diagnostic, execution.ExecutionID, err)
	}
	if err := runtime.ReleaseExecutionAdmission(ctx, execution.ExecutionID); err != nil {
		return fmt.Errorf("%s: release execution admission for %q: %w", diagnostic, execution.ExecutionID, err)
	}

	return errors.New(diagnostic)
}

func findRunningExecutionForThread(ctx context.Context, runtimeRepo runtime.Store, threadTS string) (runtime.ExecutionRequest, error) {
	if runtimeRepo == nil {
		return runtime.ExecutionRequest{}, errors.New("runtime repository unavailable")
	}

	executions, err := runtimeRepo.ListExecutions(ctx, []runtime.ExecutionLifecycleState{runtime.ExecutionStateRunning})
	if err != nil {
		return runtime.ExecutionRequest{}, fmt.Errorf("list running executions: %w", err)
	}

	var matched runtime.ExecutionRequest
	for _, execution := range executions {
		if execution.ThreadTS != threadTS {
			continue
		}
		if matched.ExecutionID == "" || execution.UpdatedAt.After(matched.UpdatedAt) {
			matched = execution
		}
	}
	if matched.ExecutionID == "" {
		return runtime.ExecutionRequest{}, runtime.ErrNotFound
	}

	return matched, nil
}

func (h *runtimeCommandHandler) loadExecutionStatus(ctx context.Context, executionID string) (runtime.ExecutionStatusReport, error) {
	if _, _, err := h.loadValidatedConfig(ctx); err != nil {
		return runtime.ExecutionStatusReport{}, err
	}

	storagePath, err := storage.DefaultPath()
	if err != nil {
		return runtime.ExecutionStatusReport{}, err
	}

	store, err := storage.Open(ctx, storagePath)
	if err != nil {
		return runtime.ExecutionStatusReport{}, err
	}
	defer func() {
		_ = store.Close()
	}()

	runtimeRepo := store.Runtime()
	if runtimeRepo == nil {
		return runtime.ExecutionStatusReport{}, errors.New("runtime repository unavailable")
	}

	execution, err := runtimeRepo.LoadExecution(ctx, executionID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return runtime.ExecutionStatusReport{}, fmt.Errorf("execution %q not found", executionID)
		}
		return runtime.ExecutionStatusReport{}, err
	}

	return runtime.ExecutionStatusReport{
		RequestedAt: time.Now().UTC(),
		Execution:   execution,
	}, nil
}

func (h *runtimeCommandHandler) writeCancelNoOp(ctx context.Context, threadTS, sessionName, message string) error {
	snapshot, err := h.loadSnapshot(ctx, false, "runtime cancel no-op")
	if err != nil {
		return err
	}

	report := runtime.CancelReport{
		RequestedAt: time.Now().UTC(),
		ThreadTS:    threadTS,
		SessionName: sessionName,
		Result:      "no-op",
		NoOp:        true,
		Message:     message,
		Status:      snapshot.statusCopy(),
	}
	report.Status.Message = message

	return writeJSON(h.out, report)
}

func (h *runtimeCommandHandler) loadValidatedConfig(ctx context.Context) (config.GlobalConfig, string, error) {
	store := h.configStore
	if store == nil {
		store = config.NewFileStore("")
	}

	cfg, err := store.Validate(ctx)
	if err != nil {
		return config.GlobalConfig{}, "", err
	}

	path, err := storePath(store)
	if err != nil {
		return config.GlobalConfig{}, "", err
	}

	return cfg, path, nil
}

func (h *runtimeCommandHandler) newForegroundRuntimeStarter(ctx context.Context, cfg config.GlobalConfig, opts runtimeStartOptions) (runtimeStarter, error) {
	store, err := storage.OpenDefault(ctx)
	if err != nil {
		return nil, err
	}

	projectRepo := store.Projects()
	if projectRepo == nil {
		_ = store.Close()
		return nil, errors.New("project repository unavailable")
	}

	runtimeRepo := store.Runtime()
	if runtimeRepo == nil {
		_ = store.Close()
		return nil, errors.New("runtime repository unavailable")
	}

	client := slack.NewHTTPClient(cfg.Slack.BotToken)
	source := slack.NewSocketModeClient(cfg.Slack.AppToken)
	if opts.debug && rawSocketDebugEnabled() {
		source.SetDebugLogger(h.debugf)
	}

	starter := &foregroundRuntimeStarter{
		source:      source,
		client:      client,
		renderer:    runtime.SlackThreadRenderer{Client: client},
		adapter:     h.adapter,
		projectRepo: projectRepo,
		runtimeRepo: runtimeRepo,
		store:       store,
	}
	if opts.debug {
		starter.debugf = h.debugf
	}

	return starter, nil
}

func parseRuntimeStartOptions(args []string) (runtimeStartOptions, error) {
	var opts runtimeStartOptions
	for _, arg := range args {
		switch strings.TrimSpace(arg) {
		case "--debug":
			opts.debug = true
		case "":
			return runtimeStartOptions{}, fmt.Errorf("runtime start does not accept empty arguments")
		default:
			return runtimeStartOptions{}, fmt.Errorf("runtime start does not accept argument %q", arg)
		}
	}
	return opts, nil
}

func rawSocketDebugEnabled() bool {
	value := strings.TrimSpace(os.Getenv(rawSocketDebugEnvVar))
	switch strings.ToLower(value) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func (h *runtimeCommandHandler) debugf(format string, args ...any) {
	if h == nil || h.out == nil {
		return
	}
	fmt.Fprintf(h.out, format+"\n", args...)
}

type foregroundRuntimeStarter struct {
	source           slack.InboundInvocationSource
	client           slack.Client
	renderer         runtime.SlackThreadRenderer
	adapter          acpxadapter.Adapter
	projectRepo      runtime.ProjectContextResolver
	runtimeRepo      runtime.Store
	executionManager runtime.ExecutionManager
	store            interface{ Close() error }
	newExecID        func() string
	debugf           func(string, ...any)
}

func (s *foregroundRuntimeStarter) Start(ctx context.Context, _ runtime.Status) error {
	if s == nil {
		return errors.New("foreground runtime starter is required")
	}
	if s.source == nil {
		return errors.New("slack event source is required")
	}
	if s.client == nil {
		return errors.New("slack client is required")
	}
	if s.adapter == nil {
		return errors.New("acpx adapter is required")
	}
	if s.projectRepo == nil {
		return errors.New("project repository is required")
	}
	if s.runtimeRepo == nil {
		return errors.New("runtime repository is required")
	}
	defer func() {
		_ = s.source.Close()
		_ = s.client.Close()
		if s.store != nil {
			_ = s.store.Close()
		}
	}()

	invocations, err := s.source.InboundInvocations(ctx)
	if err != nil {
		return err
	}

	coordinator := runtime.NewSlackTurnCoordinator(s.runtimeRepo, "runtime-start")
	if s.newExecID != nil {
		coordinator.SetExecutionIDGenerator(s.newExecID)
	}
	manager := s.executionManager
	if manager == nil {
		manager = runtime.NewExecutionManager(coordinator, nil)
	}
	s.logf("runtime.loop: connected to slack socket mode")

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case invocation, ok := <-invocations:
			if !ok {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return errors.New("slack invocation source stopped")
			}
			s.logf(
				"runtime.loop: received slack invocation delivery=%s source=%s channel=%s thread=%s user=%s text=%q",
				invocation.DeliveryID,
				invocation.SourceType,
				invocation.ChannelID,
				strings.TrimSpace(invocation.ThreadTS),
				invocation.UserID,
				strings.TrimSpace(invocation.CommandText),
			)

			switch invocation.SourceType {
			case slack.InboundSourceMention:
				if err := s.handleMentionInvocation(ctx, coordinator, manager, invocation); err != nil {
					s.logf("runtime.loop: mention failed delivery=%s: %v", invocation.DeliveryID, err)
				}
			case slack.InboundSourceMessage:
				if err := s.handleMessageInvocation(ctx, coordinator, manager, invocation); err != nil {
					s.logf("runtime.loop: message failed delivery=%s: %v", invocation.DeliveryID, err)
				}
			case slack.InboundSourceSlash:
				if err := s.handleSlashInvocation(ctx, coordinator, manager, invocation); err != nil {
					s.logf("runtime.loop: slash failed delivery=%s: %v", invocation.DeliveryID, err)
				}
			default:
				s.logf("runtime.loop: ignored slack invocation delivery=%s source=%s", invocation.DeliveryID, invocation.SourceType)
			}
		}
	}
}

func (s *foregroundRuntimeStarter) handleMentionInvocation(ctx context.Context, coordinator *runtime.SlackTurnCoordinator, manager runtime.ExecutionManager, invocation slack.InboundInvocation) error {
	prepared, err := runtime.PrepareSlackMentionEvent(ctx, s.projectRepo, invocation)
	if err != nil {
		return s.handleInvocationRejection(ctx, invocation, err)
	}
	command := runtime.ParseSlackCommand(prepared.Event.Text)
	switch command.Kind {
	case runtime.SlackCommandCancel, runtime.SlackCommandStatus, runtime.SlackCommandHelp, runtime.SlackCommandInvalid:
		return s.handleLocalMentionCommand(ctx, coordinator, prepared, command)
	}

	accepted, result, err := coordinator.Accept(ctx, prepared)
	if err != nil {
		s.logLifecycle("execution_completed", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "failed", "session="+prepared.SessionName, "error="+strconv.Quote(err.Error()))
		_ = s.postTurnError(ctx, prepared, err)
		return err
	}
	if result.Duplicate {
		s.logLifecycle("duplicate_skipped", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "duplicate")
		s.logf("runtime.loop: duplicate mention skipped delivery=%s", invocation.DeliveryID)
		return nil
	}

	s.logLifecycle("execution_handed_off", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "queued", "session="+prepared.SessionName, "execution_id="+accepted.Execution.ExecutionID)
	s.logf("runtime.loop: mention handed off delivery=%s execution=%s session=%s", invocation.DeliveryID, accepted.Execution.ExecutionID, prepared.SessionName)

	return s.scheduleAcceptedInvocation(ctx, manager, accepted, "mention", func(ctx context.Context, prepared runtime.PreparedSlackEvent) error {
		s.logLifecycle("execution_started", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "processing", "session="+prepared.SessionName)
		s.logf(
			"runtime.loop: dispatching mention delivery=%s project=%s session=%s",
			prepared.Event.ID,
			prepared.Project.Name,
			prepared.SessionName,
		)

		if !command.ShouldExecute() {
			if err := s.client.PostThreadMessage(ctx, slack.Message{
				ChannelID: prepared.Project.SlackChannelID,
				ThreadTS:  prepared.ThreadTS,
				Text:      mentionUsageText(),
			}); err != nil {
				return err
			}
			s.logf("runtime.loop: rendered mention usage thread=%s", prepared.ThreadTS)
			return nil
		}

		session, err := s.sendPromptAndRender(ctx, prepared, command.ACPXPrompt(), "mention", command.Kind == runtime.SlackCommandAsk)
		if err != nil {
			return err
		}
		s.logf("runtime.loop: rendered slack reply thread=%s session=%s", prepared.ThreadTS, session.SessionName)
		return nil
	})
}

func (s *foregroundRuntimeStarter) handleLocalMentionCommand(ctx context.Context, coordinator *runtime.SlackTurnCoordinator, prepared runtime.PreparedSlackEvent, command runtime.SlackCommand) error {
	if s == nil {
		return errors.New("foreground runtime starter is required")
	}
	if s.client == nil {
		return errors.New("slack client is required")
	}
	if coordinator == nil {
		return errors.New("slack turn coordinator is required")
	}

	claim, err := coordinator.ClaimDelivery(ctx, prepared.SourceType, prepared.DeliveryID)
	if err != nil {
		s.logLifecycle("execution_completed", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "failed", "session="+prepared.SessionName, "error="+strconv.Quote(err.Error()))
		_ = s.postTurnError(ctx, prepared, err)
		return err
	}
	if claim.Duplicate {
		s.logLifecycle("duplicate_skipped", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "duplicate")
		s.logf("runtime.loop: duplicate mention skipped delivery=%s", prepared.DeliveryID)
		return nil
	}

	s.logLifecycle("execution_started", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "processing", "session="+prepared.SessionName, "command="+string(command.Kind))
	s.logf("runtime.loop: dispatching local mention delivery=%s project=%s session=%s command=%s", prepared.DeliveryID, prepared.Project.Name, prepared.SessionName, command.Kind)

	if err := s.renderLocalMentionCommand(ctx, prepared, command); err != nil {
		s.markDeliveryCompleted(ctx, prepared.SourceType, prepared.DeliveryID, "failed", err.Error())
		s.logLifecycle("execution_completed", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "failed", "session="+prepared.SessionName, "command="+string(command.Kind), "error="+strconv.Quote(err.Error()))
		_ = s.postTurnError(ctx, prepared, err)
		return err
	}

	s.markDeliveryCompleted(ctx, prepared.SourceType, prepared.DeliveryID, "processed", "")
	s.logLifecycle("execution_completed", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "processed", "session="+prepared.SessionName, "command="+string(command.Kind))
	s.logf("runtime.loop: local mention processed delivery=%s session=%s command=%s", prepared.DeliveryID, prepared.SessionName, command.Kind)
	return nil
}

func (s *foregroundRuntimeStarter) handleMessageInvocation(ctx context.Context, coordinator *runtime.SlackTurnCoordinator, manager runtime.ExecutionManager, invocation slack.InboundInvocation) error {
	if strings.TrimSpace(invocation.ThreadTS) == "" {
		s.logf("runtime.loop: ignored root message delivery=%s channel=%s", invocation.DeliveryID, invocation.ChannelID)
		return nil
	}
	if isLeadingSlackMention(strings.TrimSpace(invocation.CommandText)) {
		s.logf("runtime.loop: ignored threaded message with mention prefix delivery=%s channel=%s thread=%s", invocation.DeliveryID, invocation.ChannelID, invocation.ThreadTS)
		return nil
	}

	prepared, err := runtime.PrepareSlackMessageEvent(ctx, s.projectRepo, invocation)
	if err != nil {
		return s.handleInvocationRejection(ctx, invocation, err)
	}

	accepted, result, err := coordinator.Accept(ctx, prepared)
	if err != nil {
		s.logLifecycle("execution_completed", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "failed", "session="+prepared.SessionName, "error="+strconv.Quote(err.Error()))
		_ = s.postTurnError(ctx, prepared, err)
		return err
	}
	if result.Duplicate {
		s.logLifecycle("duplicate_skipped", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "duplicate")
		s.logf("runtime.loop: duplicate message skipped delivery=%s", invocation.DeliveryID)
		return nil
	}

	s.logLifecycle("execution_handed_off", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "queued", "session="+prepared.SessionName, "execution_id="+accepted.Execution.ExecutionID)
	s.logf("runtime.loop: message handed off delivery=%s execution=%s session=%s", invocation.DeliveryID, accepted.Execution.ExecutionID, prepared.SessionName)

	return s.scheduleAcceptedInvocation(ctx, manager, accepted, "message", func(ctx context.Context, prepared runtime.PreparedSlackEvent) error {
		s.logLifecycle("execution_started", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "processing", "session="+prepared.SessionName)
		s.logf(
			"runtime.loop: dispatching message delivery=%s project=%s session=%s",
			prepared.Event.ID,
			prepared.Project.Name,
			prepared.SessionName,
		)

		prompt := strings.TrimSpace(prepared.Event.Text)
		if prompt == "" {
			s.logf("runtime.loop: ignored empty message thread=%s", prepared.ThreadTS)
			return nil
		}

		session, err := s.sendPromptAndRender(ctx, prepared, prompt, "message", false)
		if err != nil {
			return err
		}
		s.logf("runtime.loop: rendered message reply thread=%s session=%s", prepared.ThreadTS, session.SessionName)
		return nil
	})
}

func (s *foregroundRuntimeStarter) renderLocalMentionCommand(ctx context.Context, prepared runtime.PreparedSlackEvent, command runtime.SlackCommand) error {
	switch command.Kind {
	case runtime.SlackCommandCancel:
		return s.handleMentionCancelInvocation(ctx, prepared)
	case runtime.SlackCommandStatus:
		return s.handleMentionStatusInvocation(ctx, prepared)
	case runtime.SlackCommandHelp, runtime.SlackCommandInvalid:
		if err := s.client.PostThreadMessage(ctx, slack.Message{
			ChannelID: prepared.Project.SlackChannelID,
			ThreadTS:  prepared.ThreadTS,
			Text:      mentionUsageText(),
		}); err != nil {
			return err
		}
		s.logf("runtime.loop: rendered mention usage thread=%s", prepared.ThreadTS)
		return nil
	default:
		return fmt.Errorf("unsupported local mention command %q", command.Kind)
	}
}

func (s *foregroundRuntimeStarter) handleMentionCancelInvocation(ctx context.Context, prepared runtime.PreparedSlackEvent) error {
	if s == nil {
		return errors.New("foreground runtime starter is required")
	}
	if s.client == nil {
		return errors.New("slack client is required")
	}

	var out bytes.Buffer
	handler := &runtimeCommandHandler{
		configStore: config.NewFileStore(""),
		adapter:     s.adapter,
		out:         &out,
	}
	if err := handler.Cancel(ctx, []string{prepared.ThreadTS}); err != nil {
		s.logf("runtime.loop: mention cancel failed thread=%s session=%s: %v", prepared.ThreadTS, prepared.SessionName, err)
		_ = s.postTurnError(ctx, prepared, err)
		return err
	}

	var report runtime.CancelReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		return fmt.Errorf("decode runtime cancel report: %w", err)
	}

	messageText := strings.TrimSpace(report.Message)
	if messageText == "" {
		messageText = "active ACPX execution cancelled"
	}
	if err := s.client.PostThreadMessage(ctx, slack.Message{
		ChannelID: prepared.Project.SlackChannelID,
		ThreadTS:  prepared.ThreadTS,
		Text:      messageText,
	}); err != nil {
		return err
	}

	s.logf("runtime.loop: rendered mention cancel thread=%s session=%s result=%s", prepared.ThreadTS, prepared.SessionName, report.Result)
	return nil
}

func (s *foregroundRuntimeStarter) handleMentionStatusInvocation(ctx context.Context, prepared runtime.PreparedSlackEvent) error {
	if s == nil {
		return errors.New("foreground runtime starter is required")
	}
	if s.client == nil {
		return errors.New("slack client is required")
	}
	if s.runtimeRepo == nil {
		return errors.New("runtime repository unavailable")
	}

	statusText, err := s.describeThreadStatus(ctx, prepared.ThreadTS)
	if err != nil {
		return err
	}
	if err := s.client.PostThreadMessage(ctx, slack.Message{
		ChannelID: prepared.Project.SlackChannelID,
		ThreadTS:  prepared.ThreadTS,
		Text:      statusText,
	}); err != nil {
		return err
	}

	s.logf("runtime.loop: rendered mention status thread=%s session=%s", prepared.ThreadTS, prepared.SessionName)
	return nil
}

func (s *foregroundRuntimeStarter) describeThreadStatus(ctx context.Context, threadTS string) (string, error) {
	if s == nil || s.runtimeRepo == nil {
		return "", errors.New("runtime repository unavailable")
	}

	state, stateErr := s.runtimeRepo.LoadThreadState(ctx, threadTS)
	if stateErr != nil && !errors.Is(stateErr, storage.ErrNotFound) {
		return "", stateErr
	}

	executions, err := s.runtimeRepo.ListExecutions(ctx, nil)
	if err != nil {
		return "", err
	}

	var latest runtime.ExecutionRequest
	for _, execution := range executions {
		if execution.ThreadTS != threadTS {
			continue
		}
		if latest.ExecutionID == "" || execution.UpdatedAt.After(latest.UpdatedAt) {
			latest = execution
		}
	}

	if state.ThreadTS == "" && latest.ExecutionID == "" {
		return "thread is inactive", nil
	}

	lines := []string{"thread status"}
	if state.ThreadTS != "" {
		status := strings.TrimSpace(state.LastStatus)
		if status == "" {
			status = "unknown"
		}
		lines = append(lines, "status: "+status)
		if sessionName := strings.TrimSpace(state.SessionName); sessionName != "" {
			lines = append(lines, "session: "+sessionName)
		}
	} else if latest.ExecutionID != "" {
		lines = append(lines, "status: "+string(latest.Status))
	}
	if latest.ExecutionID != "" {
		lines = append(lines, fmt.Sprintf("execution: %s (%s)", latest.ExecutionID, latest.Status))
		if diagnostic := strings.TrimSpace(latest.DiagnosticContext); diagnostic != "" {
			lines = append(lines, "detail: "+diagnostic)
		}
	}

	return strings.Join(lines, "\n"), nil
}

func (s *foregroundRuntimeStarter) handleSlashInvocation(ctx context.Context, coordinator *runtime.SlackTurnCoordinator, manager runtime.ExecutionManager, invocation slack.InboundInvocation) error {
	prepared, err := runtime.PrepareSlackInvocation(ctx, s.projectRepo, invocation)
	if err != nil {
		return s.handleInvocationRejection(ctx, invocation, err)
	}
	claim, err := coordinator.ClaimDelivery(ctx, prepared.Invocation.SourceType, prepared.Invocation.DeliveryID)
	if err != nil {
		return err
	}
	if claim.Duplicate {
		s.logLifecycle("duplicate_skipped", prepared.Invocation.SourceType, prepared.Invocation.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "duplicate")
		s.logf("runtime.loop: duplicate slash skipped delivery=%s", prepared.Invocation.DeliveryID)
		return nil
	}

	s.logLifecycle("ack_sent", prepared.Invocation.SourceType, prepared.Invocation.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "acked")
	s.logf("runtime.loop: slash ack sent delivery=%s", prepared.Invocation.DeliveryID)
	go s.executeSlashInvocation(ctx, coordinator, manager, prepared)
	return nil
}

func (s *foregroundRuntimeStarter) executeSlashInvocation(ctx context.Context, coordinator *runtime.SlackTurnCoordinator, manager runtime.ExecutionManager, prepared runtime.PreparedSlackInvocation) {
	command := runtime.ParseSlackCommand(prepared.Invocation.CommandText)
	rawCommand := strings.TrimSpace(prepared.Invocation.CommandText)

	rootMessage, err := s.client.PostMessage(ctx, slack.Message{
		ChannelID: prepared.Project.SlackChannelID,
		Text:      slashStartText(rawCommand),
	})
	if err != nil {
		s.markDeliveryCompleted(ctx, prepared.Invocation.SourceType, prepared.Invocation.DeliveryID, "failed", err.Error())
		s.logLifecycle("execution_completed", prepared.Invocation.SourceType, prepared.Invocation.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "failed", "error="+strconv.Quote(err.Error()))
		s.logf("runtime.loop: slash start message failed delivery=%s: %v", prepared.Invocation.DeliveryID, err)
		return
	}

	threadTS := strings.TrimSpace(rootMessage.Timestamp)
	sessionName := acpxadapter.SessionName(threadTS)
	preparedEvent := runtime.PreparedSlackEvent{
		SourceType: prepared.Invocation.SourceType,
		DeliveryID: prepared.Invocation.DeliveryID,
		Event: slack.Event{
			ID:        prepared.Invocation.DeliveryID,
			ChannelID: prepared.Project.SlackChannelID,
			ThreadTS:  threadTS,
			Timestamp: threadTS,
			UserID:    prepared.Invocation.UserID,
			Text:      rawCommand,
		},
		Project:     prepared.Project,
		ThreadTS:    threadTS,
		SessionName: sessionName,
		ThreadState: runtime.ThreadState{
			ThreadTS:    threadTS,
			ChannelID:   prepared.Project.SlackChannelID,
			ProjectName: prepared.Project.Name,
			SessionName: sessionName,
		},
	}

	s.logf(
		"runtime.loop: dispatching slash delivery=%s project=%s session=%s thread=%s",
		prepared.Invocation.DeliveryID,
		prepared.Project.Name,
		sessionName,
		threadTS,
	)

	accepted, result, err := coordinator.Accept(ctx, preparedEvent)
	if err != nil {
		s.markDeliveryCompleted(ctx, preparedEvent.SourceType, preparedEvent.DeliveryID, "failed", err.Error())
		s.logLifecycle("execution_completed", preparedEvent.SourceType, preparedEvent.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "failed", "session="+preparedEvent.SessionName, "error="+strconv.Quote(err.Error()))
		s.logf("runtime.loop: slash async failed delivery=%s: %v", prepared.Invocation.DeliveryID, err)
		_ = s.postTurnError(ctx, preparedEvent, err)
		return
	}
	if result.Duplicate {
		s.logLifecycle("duplicate_skipped", preparedEvent.SourceType, preparedEvent.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "duplicate")
		s.logf("runtime.loop: duplicate slash skipped delivery=%s", prepared.Invocation.DeliveryID)
		return
	}

	s.logLifecycle("execution_handed_off", preparedEvent.SourceType, preparedEvent.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "queued", "session="+preparedEvent.SessionName, "execution_id="+accepted.Execution.ExecutionID)
	s.logf("runtime.loop: slash handed off delivery=%s execution=%s session=%s", prepared.Invocation.DeliveryID, accepted.Execution.ExecutionID, preparedEvent.SessionName)

	if err := s.scheduleAcceptedInvocation(ctx, manager, accepted, "slash", func(ctx context.Context, prepared runtime.PreparedSlackEvent) error {
		s.logLifecycle("execution_started", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "processing", "session="+prepared.SessionName)
		if !command.ShouldExecute() {
			if err := s.client.PostThreadMessage(ctx, slack.Message{
				ChannelID: prepared.Project.SlackChannelID,
				ThreadTS:  prepared.ThreadTS,
				Text:      slashUsageText(),
			}); err != nil {
				return err
			}
			s.logf("runtime.loop: rendered slash usage thread=%s", prepared.ThreadTS)
			return nil
		}

		session, err := s.sendPromptAndRender(ctx, prepared, command.ACPXPrompt(), "slash", command.Kind == runtime.SlackCommandAsk)
		if err != nil {
			return err
		}
		s.logf("runtime.loop: rendered slash reply thread=%s session=%s", prepared.ThreadTS, session.SessionName)
		return nil
	}); err != nil {
		s.logLifecycle("execution_completed", preparedEvent.SourceType, preparedEvent.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "failed", "session="+preparedEvent.SessionName, "execution_id="+accepted.Execution.ExecutionID, "error="+strconv.Quote(err.Error()))
		s.logf("runtime.loop: slash async failed delivery=%s execution=%s session=%s: %v", prepared.Invocation.DeliveryID, accepted.Execution.ExecutionID, preparedEvent.SessionName, err)
		_ = s.postTurnError(ctx, preparedEvent, err)
	}
}

func (s *foregroundRuntimeStarter) scheduleAcceptedInvocation(ctx context.Context, manager runtime.ExecutionManager, accepted runtime.AcceptedSlackExecution, sourceLabel string, execute func(context.Context, runtime.PreparedSlackEvent) error) error {
	if manager == nil {
		return errors.New("execution manager is required")
	}

	result, err := manager.ScheduleAccepted(ctx, runtime.ManagedExecution{
		Accepted: accepted,
		Execute:  execute,
		Callbacks: runtime.ExecutionCallbacks{
			OnCompleted: func(result runtime.SlackTurnExecutionResult) {
				s.logLifecycle("execution_completed", accepted.Prepared.SourceType, accepted.Prepared.DeliveryID, accepted.Prepared.Project.SlackChannelID, accepted.Prepared.Project.Name, "processed", "session="+accepted.Prepared.SessionName, "execution_id="+accepted.Execution.ExecutionID)
				s.logf("runtime.loop: %s processed delivery=%s execution=%s session=%s completed_at=%s", sourceLabel, accepted.Prepared.DeliveryID, accepted.Execution.ExecutionID, accepted.Prepared.SessionName, result.CompletedAt.UTC().Format(time.RFC3339Nano))
			},
			OnFailed: func(err error) {
				s.logLifecycle("execution_completed", accepted.Prepared.SourceType, accepted.Prepared.DeliveryID, accepted.Prepared.Project.SlackChannelID, accepted.Prepared.Project.Name, "failed", "session="+accepted.Prepared.SessionName, "execution_id="+accepted.Execution.ExecutionID, "error="+strconv.Quote(err.Error()))
				s.logf("runtime.loop: %s failed delivery=%s execution=%s session=%s: %v", sourceLabel, accepted.Prepared.DeliveryID, accepted.Execution.ExecutionID, accepted.Prepared.SessionName, err)
				_ = s.postTurnError(ctx, accepted.Prepared, err)
			},
		},
	})
	if err != nil {
		return err
	}
	if !result.Started {
		fields := []string{
			"session=" + accepted.Prepared.SessionName,
			"execution_id=" + accepted.Execution.ExecutionID,
		}
		if reason := strings.TrimSpace(result.Decision.Reason); reason != "" {
			fields = append(fields, "reason="+strconv.Quote(reason))
		}
		s.logLifecycle("execution_deferred", accepted.Prepared.SourceType, accepted.Prepared.DeliveryID, accepted.Prepared.Project.SlackChannelID, accepted.Prepared.Project.Name, "queued", fields...)
		s.logf("runtime.loop: %s deferred delivery=%s execution=%s session=%s reason=%q", sourceLabel, accepted.Prepared.DeliveryID, accepted.Execution.ExecutionID, accepted.Prepared.SessionName, result.Decision.Reason)
	}
	return nil
}

func (s *foregroundRuntimeStarter) sendPromptAndRender(ctx context.Context, prepared runtime.PreparedSlackEvent, prompt, sourceLabel string, forceNew bool) (acpxadapter.SessionResult, error) {
	req := acpxadapter.SessionRequest{
		ProjectPath: prepared.Project.LocalPath,
		ThreadTS:    prepared.ThreadTS,
		Prompt:      prompt,
		ForceNew:    forceNew,
	}
	renderReq := runtime.SlackThreadRenderRequest{
		ChannelID:   prepared.Project.SlackChannelID,
		ThreadTS:    prepared.ThreadTS,
		SessionName: prepared.SessionName,
	}

	if streamingAdapter, ok := s.adapter.(acpxadapter.StreamingAdapter); ok {
		stream, err := s.renderer.NewStream(ctx, renderReq)
		if err != nil {
			return acpxadapter.SessionResult{}, err
		}
		session, err := streamingAdapter.SendPromptStream(ctx, req, func(output string) error {
			return stream.RenderOutput(ctx, output)
		})
		if err != nil {
			return session, err
		}
		if err := stream.RenderOutput(ctx, session.Output); err != nil {
			return session, err
		}
		s.logACPXCompletion(session)
		s.logf("runtime.loop: streamed %s reply thread=%s session=%s", sourceLabel, prepared.ThreadTS, session.SessionName)
		return session, nil
	}

	session, err := s.adapter.SendPrompt(ctx, req)
	if err != nil {
		return acpxadapter.SessionResult{}, err
	}
	s.logACPXCompletion(session)
	if err := runtime.RenderACPXTurnOutput(ctx, s.renderer, renderReq, session.Output); err != nil {
		return session, err
	}
	return session, nil
}

func (s *foregroundRuntimeStarter) logACPXCompletion(session acpxadapter.SessionResult) {
	s.logf(
		"runtime.loop: acpx completed session=%s output_bytes=%d",
		session.SessionName,
		len(session.Output),
	)
	s.logf(
		"runtime.loop: acpx output session=%s payload=%s",
		session.SessionName,
		summarizeACPXOutput(session.Output, 4000),
	)
}

func (s *foregroundRuntimeStarter) postTurnError(ctx context.Context, prepared runtime.PreparedSlackEvent, err error) error {
	if s == nil || s.client == nil {
		return nil
	}
	if strings.TrimSpace(prepared.Project.SlackChannelID) == "" || strings.TrimSpace(prepared.ThreadTS) == "" {
		return nil
	}
	return s.client.PostThreadMessage(ctx, slack.Message{
		ChannelID: prepared.Project.SlackChannelID,
		ThreadTS:  prepared.ThreadTS,
		Text:      "Session error: " + err.Error(),
	})
}

func (s *foregroundRuntimeStarter) logf(format string, args ...any) {
	if s != nil && s.debugf != nil {
		s.debugf(format, args...)
	}
}

func (s *foregroundRuntimeStarter) logLifecycle(event, sourceType, deliveryID, channelID, project, status string, fields ...string) {
	line := fmt.Sprintf(
		"runtime.loop: lifecycle event=%s source=%s delivery_id=%s channel_id=%s project=%s status=%s",
		event,
		sourceType,
		deliveryID,
		channelID,
		project,
		status,
	)
	if len(fields) > 0 {
		line += " " + strings.Join(fields, " ")
	}
	s.logf("%s", line)
}

func (s *foregroundRuntimeStarter) logInvocationRejection(invocation slack.InboundInvocation, err error) {
	var rejectionErr *runtime.RejectedSlackInvocationError
	if errors.As(err, &rejectionErr) {
		sourceType := rejectionErr.Rejection.SourceType
		if sourceType == "" {
			sourceType = invocation.SourceType
		}
		channelID := rejectionErr.Rejection.ChannelID
		if channelID == "" {
			channelID = invocation.ChannelID
		}
		s.logLifecycle("execution_rejected", sourceType, invocation.DeliveryID, channelID, "unknown", "rejected", "error="+strconv.Quote(err.Error()))
	}
	s.logf("runtime.loop: rejected slack invocation delivery=%s: %v", invocation.DeliveryID, err)
}

func isLeadingSlackMention(text string) bool {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "<@") {
		return false
	}
	return strings.Contains(text, ">")
}

func (s *foregroundRuntimeStarter) handleInvocationRejection(ctx context.Context, invocation slack.InboundInvocation, err error) error {
	s.logInvocationRejection(invocation, err)

	var rejectionErr *runtime.RejectedSlackInvocationError
	if !errors.As(err, &rejectionErr) {
		return nil
	}
	if renderErr := s.renderSlackInvocationRejection(ctx, rejectionErr.Rejection); renderErr != nil {
		return fmt.Errorf("render slack rejection delivery=%s: %w", invocation.DeliveryID, renderErr)
	}

	sourceType := rejectionErr.Rejection.SourceType
	if sourceType == "" {
		sourceType = invocation.SourceType
	}
	channelID := rejectionErr.Rejection.ChannelID
	if channelID == "" {
		channelID = invocation.ChannelID
	}
	s.logf(
		"runtime.loop: rendered slack rejection delivery=%s source=%s channel=%s ephemeral=%t",
		invocation.DeliveryID,
		sourceType,
		channelID,
		rejectionErr.Rejection.Ephemeral,
	)
	return nil
}

func (s *foregroundRuntimeStarter) renderSlackInvocationRejection(ctx context.Context, rejection runtime.SlackInvocationRejection) error {
	if s == nil || s.client == nil {
		return errors.New("slack client is required")
	}

	if rejection.Ephemeral {
		client, ok := s.client.(slack.ResponseURLClient)
		if !ok {
			return errors.New("slack response_url client is required")
		}
		return client.PostResponseURLMessage(ctx, slack.ResponseURLMessage{
			ResponseURL:  strings.TrimSpace(rejection.ResponseURL),
			Text:         rejection.Message,
			ResponseType: slack.ResponseTypeEphemeral,
		})
	}

	message := slack.Message{
		ChannelID: strings.TrimSpace(rejection.ChannelID),
		ThreadTS:  strings.TrimSpace(rejection.ThreadTS),
		Text:      rejection.Message,
	}
	if message.ChannelID == "" {
		return errors.New("slack rejection channel id is required")
	}
	if message.ThreadTS != "" {
		return s.client.PostThreadMessage(ctx, message)
	}
	_, err := s.client.PostMessage(ctx, message)
	return err
}

func (s *foregroundRuntimeStarter) markDeliveryCompleted(ctx context.Context, sourceType, deliveryID, status, diagnosticContext string) {
	if s == nil || s.runtimeRepo == nil {
		return
	}

	dedupe, err := s.runtimeRepo.LoadEventDedupe(ctx, sourceType, deliveryID)
	if err != nil {
		return
	}
	completedAt := time.Now().UTC()
	dedupe.Status = status
	dedupe.ProcessedAt = &completedAt
	dedupe.DiagnosticContext = diagnosticContext
	_ = s.runtimeRepo.SaveEventDedupe(ctx, dedupe)
}

func summarizeACPXOutput(output string, limit int) string {
	output = strings.TrimSpace(output)
	if limit <= 0 || len(output) <= limit {
		return output
	}
	return output[:limit] + "...(truncated)"
}

func mentionUsageText() string {
	return runtime.SlackCommandHelpText(runtime.SlackCommandSurfaceMention)
}

func slashUsageText() string {
	return runtime.SlackCommandHelpText(runtime.SlackCommandSurfaceSlash)
}

func slashStartText(command string) string {
	command = strings.TrimSpace(command)
	if command == "" {
		return "Received `/spexus` command."
	}
	return fmt.Sprintf("Running `/spexus %s` in a new execution thread.", command)
}

func (h *runtimeCommandHandler) loadStartupSnapshot(ctx context.Context) (runtimeSnapshot, error) {
	snapshot, err := h.loadSnapshot(ctx, false, "runtime start loaded")
	if err != nil {
		return runtimeSnapshot{}, err
	}

	if !snapshot.status.Config.Slack.Status().Configured {
		return runtimeSnapshot{}, errors.New("slack authentication is required for runtime start")
	}

	return snapshot, nil
}

func (h *runtimeCommandHandler) currentStatus(ctx context.Context) (runtime.Status, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.loaded {
		return h.snapshot.statusCopy(), nil
	}

	snapshot, err := h.loadSnapshot(ctx, true, "runtime state loaded")
	if err != nil {
		return runtime.Status{}, err
	}

	h.snapshot = snapshot
	h.loaded = true
	return h.snapshot.statusCopy(), nil
}

func (h *runtimeCommandHandler) reloadSnapshot(ctx context.Context) (runtimeSnapshot, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	snapshot, err := h.loadSnapshot(ctx, false, "runtime state reloaded")
	if err != nil {
		return runtimeSnapshot{}, err
	}

	h.snapshot = snapshot
	h.loaded = true
	return h.snapshot, nil
}

func (h *runtimeCommandHandler) loadSnapshot(ctx context.Context, allowBootstrap bool, message string) (runtimeSnapshot, error) {
	cfg, configPath, err := h.loadValidatedConfig(ctx)
	if err != nil {
		return runtimeSnapshot{}, err
	}

	storagePath, err := storage.DefaultPath()
	if err != nil {
		return runtimeSnapshot{}, err
	}

	if !allowBootstrap {
		info, statErr := os.Stat(storagePath)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				return runtimeSnapshot{}, fmt.Errorf("storage database %q is missing", storagePath)
			}
			return runtimeSnapshot{}, fmt.Errorf("stat storage database: %w", statErr)
		}
		if info.IsDir() {
			return runtimeSnapshot{}, fmt.Errorf("storage database path %q is a directory", storagePath)
		}
	}

	store, err := storage.Open(ctx, storagePath)
	if err != nil {
		return runtimeSnapshot{}, err
	}
	defer func() {
		_ = store.Close()
	}()

	projectRepo := store.Projects()
	if projectRepo == nil {
		return runtimeSnapshot{}, errors.New("project repository unavailable")
	}

	projects, err := projectRepo.List(ctx)
	if err != nil {
		return runtimeSnapshot{}, err
	}

	runtimeRepo := store.Runtime()
	if runtimeRepo == nil {
		return runtimeSnapshot{}, errors.New("runtime repository unavailable")
	}

	activeThreads, err := runtimeRepo.CountThreads(ctx)
	if err != nil {
		return runtimeSnapshot{}, err
	}

	dedupeCount, err := runtimeRepo.CountEventDedupe(ctx)
	if err != nil {
		return runtimeSnapshot{}, err
	}

	snapshot := runtimeSnapshot{
		status: runtime.Status{
			Running:          true,
			Healthy:          true,
			Message:          message,
			ActiveThreads:    activeThreads,
			ProjectCount:     len(projects),
			EventDedupeCount: dedupeCount,
			ConfigPath:       configPath,
			StoragePath:      storagePath,
			LoadedAt:         time.Now().UTC(),
			Config:           cfg.Redacted(),
			Projects:         cloneProjects(projects),
		},
		projectByChannel: make(map[string]registry.Project, len(projects)),
	}

	for _, project := range projects {
		if project.SlackChannelID == "" {
			continue
		}
		snapshot.projectByChannel[project.SlackChannelID] = project
	}

	return snapshot, nil
}

func (h *runtimeCommandHandler) loadDoctorReport(ctx context.Context) (runtime.DoctorReport, error) {
	report := runtime.DoctorReport{
		Healthy: true,
		Message: "runtime diagnostics completed",
		Status: runtime.Status{
			Running:  false,
			Healthy:  true,
			Message:  "runtime diagnostics completed",
			LoadedAt: time.Now().UTC(),
		},
	}

	configOK := false
	store := h.configStore
	if store == nil {
		store = config.NewFileStore("")
	}

	configPath, err := storePath(store)
	if err != nil {
		return runtime.DoctorReport{}, err
	}
	report.Status.ConfigPath = configPath

	cfg, err := store.Load(ctx)
	if err != nil {
		report.Healthy = false
		report.Status.Healthy = false
		report.Checks = append(report.Checks, runtime.HealthCheck{
			Name:    "config",
			OK:      false,
			Message: err.Error(),
		})
	} else if validateErr := cfg.Validate(); validateErr != nil {
		report.Healthy = false
		report.Status.Healthy = false
		report.Status.Config = cfg.Redacted()
		report.Checks = append(report.Checks, runtime.HealthCheck{
			Name:    "config",
			OK:      false,
			Message: validateErr.Error(),
		})
	} else {
		configOK = true
		report.Status.Config = cfg.Redacted()
		report.Checks = append(report.Checks, runtime.HealthCheck{
			Name:    "config",
			OK:      true,
			Message: "loaded and valid",
		})
	}

	storageOK := false
	storagePath, err := storage.DefaultPath()
	if err != nil {
		return runtime.DoctorReport{}, err
	}
	report.Status.StoragePath = storagePath

	if info, statErr := os.Stat(storagePath); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			report.Healthy = false
			report.Status.Healthy = false
			report.Status.Running = false
			report.Checks = append(report.Checks, runtime.HealthCheck{
				Name:    "storage",
				OK:      false,
				Message: "storage database is missing",
			})
		} else {
			return runtime.DoctorReport{}, fmt.Errorf("stat storage database: %w", statErr)
		}
	} else {
		if info.IsDir() {
			return runtime.DoctorReport{}, fmt.Errorf("storage database path %q is a directory", storagePath)
		}

		store, err := storage.Open(ctx, storagePath)
		if err != nil {
			report.Healthy = false
			report.Status.Healthy = false
			report.Checks = append(report.Checks, runtime.HealthCheck{
				Name:    "storage",
				OK:      false,
				Message: err.Error(),
			})
		} else {
			defer func() {
				_ = store.Close()
			}()

			projects, err := store.Projects().List(ctx)
			if err != nil {
				return runtime.DoctorReport{}, err
			}
			projectCount := len(projects)
			activeThreads, err := store.Runtime().CountThreads(ctx)
			if err != nil {
				return runtime.DoctorReport{}, err
			}
			dedupeCount, err := store.Runtime().CountEventDedupe(ctx)
			if err != nil {
				return runtime.DoctorReport{}, err
			}

			report.Status.Running = true
			report.Status.ProjectCount = projectCount
			report.Status.ActiveThreads = activeThreads
			report.Status.EventDedupeCount = dedupeCount
			report.Status.Projects = cloneProjects(projects)
			storageOK = true
			report.Checks = append(report.Checks, runtime.HealthCheck{
				Name:    "storage",
				OK:      true,
				Message: "initialized and readable",
			})
		}
	}

	if !configOK || !storageOK {
		report.Healthy = false
		report.Status.Healthy = false
		report.Status.Running = configOK && storageOK
		report.Message = "runtime diagnostics report issues"
	} else {
		report.Status.Healthy = true
		report.Status.Running = true
	}

	return report, nil
}

func storePath(store *config.FileStore) (string, error) {
	if store == nil {
		return "", errors.New("config store unavailable")
	}

	path := store.Path
	if path != "" {
		return path, nil
	}

	return config.DefaultPath()
}

func (s runtimeSnapshot) statusCopy() runtime.Status {
	status := s.status
	status.Projects = cloneProjects(status.Projects)
	return status
}

func cloneProjects(projects []registry.Project) []registry.Project {
	if len(projects) == 0 {
		return nil
	}

	cloned := make([]registry.Project, len(projects))
	copy(cloned, projects)
	return cloned
}
