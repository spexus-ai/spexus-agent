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
	runtimeRepo runtime.Store
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

type renderedPromptStreamError struct {
	err error
}

type promptExecutionResult struct {
	sessionName string
	events      []runtime.ACPXTurnEvent
	cancelled   bool
	checkpoint  runtime.ExecutionCheckpoint
}

func (e *renderedPromptStreamError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *renderedPromptStreamError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
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
	if len(args) != 0 {
		return fmt.Errorf("runtime status does not accept arguments")
	}

	status, err := h.currentStatus(ctx)
	if err != nil {
		return err
	}

	return writeJSON(h.out, status)
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
		return fmt.Errorf("runtime cancel requires exactly one thread timestamp argument")
	}

	identifier := strings.TrimSpace(args[0])
	if identifier == "" {
		return fmt.Errorf("runtime cancel requires exactly one thread timestamp argument")
	}

	if _, _, err := h.loadValidatedConfig(ctx); err != nil {
		return err
	}

	runtimeRepo := h.runtimeRepo
	if runtimeRepo == nil {
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

		runtimeRepo = store.Runtime()
	}
	if runtimeRepo == nil {
		return errors.New("runtime repository unavailable")
	}

	executionStore, ok := any(runtimeRepo).(runtime.ExecutionStateStore)
	if !ok {
		return errors.New("execution state store is required")
	}
	recoveryStore, ok := any(runtimeRepo).(runtime.ExecutionRecoveryStore)
	if !ok {
		return errors.New("execution recovery store is required")
	}

	execution, err := executionStore.LoadExecutionState(ctx, identifier)
	if err == nil {
		return h.cancelExecution(ctx, runtimeRepo, executionStore, execution, nil, nil)
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return err
	}

	threadTS := identifier
	var (
		state    runtime.ThreadState
		statePtr *runtime.ThreadState
	)
	state, err = runtimeRepo.LoadThreadState(ctx, threadTS)
	if err != nil {
		if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
	} else {
		statePtr = &state
	}

	var (
		lock    runtime.ThreadLock
		lockPtr *runtime.ThreadLock
	)
	lock, err = runtimeRepo.LoadThreadLock(ctx, threadTS)
	if err != nil {
		if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
	} else {
		lockPtr = &lock
	}

	execution, err = findRunningExecutionForThread(ctx, recoveryStore, threadTS)
	if err != nil {
		if errors.Is(err, runtime.ErrNotFound) {
			sessionName := ""
			if statePtr != nil {
				sessionName = state.SessionName
			}
			return h.writeCancelNoOp(ctx, threadTS, sessionName, "thread is inactive")
		}
		return err
	}
	return h.cancelExecution(ctx, runtimeRepo, executionStore, execution, statePtr, lockPtr)
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
		source:           source,
		client:           client,
		renderer:         runtime.SlackThreadRenderer{Client: client},
		adapter:          h.adapter,
		projectRepo:      projectRepo,
		runtimeRepo:      runtimeRepo,
		executionTracker: runtime.NewExecutionLifecycleTracker(runtimeRepo),
		store:            store,
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
	executionTracker *runtime.ExecutionLifecycleTracker
	store            interface{ Close() error }
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
	manager := runtime.NewExecutionManager(ctx, runtime.ExecutionManagerConfig{
		LifecycleRecorder: s.lifecycleTracker(),
	}, func(ctx context.Context, request runtime.ExecutionRequest) {
		s.executeQueuedInvocation(ctx, coordinator, request)
	})
	defer manager.Wait()
	if err := s.reconcileNonTerminalExecutions(ctx); err != nil {
		return err
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

func (s *foregroundRuntimeStarter) handleMentionInvocation(ctx context.Context, coordinator *runtime.SlackTurnCoordinator, queue runtime.ExecutionQueue, invocation slack.InboundInvocation) error {
	prepared, err := runtime.PrepareSlackMentionEvent(ctx, s.projectRepo, invocation)
	if err != nil {
		return s.handleInvocationRejection(ctx, invocation, err)
	}
	command := runtime.ParseSlackCommand(prepared.Event.Text)
	switch command.Kind {
	case runtime.SlackCommandClose, runtime.SlackCommandStatus, runtime.SlackCommandHelp, runtime.SlackCommandInvalid:
		return s.handleLocalMentionCommand(ctx, coordinator, prepared, command)
	}
	request := runtime.NewMentionExecutionRequest(prepared)
	result, err := coordinator.ClaimExecution(ctx, request)
	if err != nil {
		return err
	}
	if result.Duplicate {
		s.logLifecycle("duplicate_skipped", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "duplicate")
		s.logf("runtime.loop: duplicate mention skipped delivery=%s", invocation.DeliveryID)
		return nil
	}
	if err := queue.Enqueue(ctx, request); err != nil {
		if tracker := s.lifecycleTracker(); tracker != nil {
			_ = tracker.RecordFailed(ctx, request, err)
		}
		s.markDeliveryCompleted(ctx, request.SourceType, request.DeliveryID, "failed")
		s.logLifecycle("execution_failed", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "failed", "session="+prepared.SessionName, "error="+strconv.Quote(err.Error()))
		_ = s.postTurnError(ctx, prepared, err)
		return err
	}
	s.logLifecycle("execution_enqueued", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "queued", "session="+prepared.SessionName)
	s.logf("runtime.loop: mention enqueued delivery=%s session=%s", invocation.DeliveryID, prepared.SessionName)
	return nil
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
		s.markDeliveryCompleted(ctx, prepared.SourceType, prepared.DeliveryID, "failed")
		s.logLifecycle("execution_completed", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "failed", "session="+prepared.SessionName, "command="+string(command.Kind), "error="+strconv.Quote(err.Error()))
		_ = s.postTurnError(ctx, prepared, err)
		return err
	}

	s.markDeliveryCompleted(ctx, prepared.SourceType, prepared.DeliveryID, "processed")
	s.logLifecycle("execution_completed", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "processed", "session="+prepared.SessionName, "command="+string(command.Kind))
	s.logf("runtime.loop: local mention processed delivery=%s session=%s command=%s", prepared.DeliveryID, prepared.SessionName, command.Kind)
	return nil
}

func (s *foregroundRuntimeStarter) handleMessageInvocation(ctx context.Context, coordinator *runtime.SlackTurnCoordinator, queue runtime.ExecutionQueue, invocation slack.InboundInvocation) error {
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
	request := runtime.NewMentionExecutionRequest(prepared)
	result, err := coordinator.ClaimExecution(ctx, request)
	if err != nil {
		return err
	}
	if result.Duplicate {
		s.logLifecycle("duplicate_skipped", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "duplicate")
		s.logf("runtime.loop: duplicate message skipped delivery=%s", invocation.DeliveryID)
		return nil
	}
	if err := queue.Enqueue(ctx, request); err != nil {
		if tracker := s.lifecycleTracker(); tracker != nil {
			_ = tracker.RecordFailed(ctx, request, err)
		}
		s.markDeliveryCompleted(ctx, request.SourceType, request.DeliveryID, "failed")
		s.logLifecycle("execution_failed", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "failed", "session="+prepared.SessionName, "error="+strconv.Quote(err.Error()))
		_ = s.postTurnError(ctx, prepared, err)
		return err
	}
	s.logLifecycle("execution_enqueued", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "queued", "session="+prepared.SessionName)
	s.logf("runtime.loop: message enqueued delivery=%s session=%s", invocation.DeliveryID, prepared.SessionName)
	return nil
}

func (s *foregroundRuntimeStarter) handleSlashInvocation(ctx context.Context, coordinator *runtime.SlackTurnCoordinator, queue runtime.ExecutionQueue, invocation slack.InboundInvocation) error {
	if !invocation.Acked {
		return fmt.Errorf("slash invocation acknowledgement not completed for delivery %q", invocation.DeliveryID)
	}

	prepared, err := runtime.PrepareSlackInvocation(ctx, s.projectRepo, invocation)
	if err != nil {
		return s.handleInvocationRejection(ctx, invocation, err)
	}
	request := runtime.NewSlashExecutionRequest(prepared)

	claim, err := coordinator.ClaimExecution(ctx, request)
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
	if err := queue.Enqueue(ctx, request); err != nil {
		if tracker := s.lifecycleTracker(); tracker != nil {
			_ = tracker.RecordFailed(ctx, request, err)
		}
		s.markDeliveryCompleted(ctx, request.SourceType, request.DeliveryID, "failed")
		s.logLifecycle("execution_failed", prepared.Invocation.SourceType, prepared.Invocation.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "failed", "error="+strconv.Quote(err.Error()))
		return err
	}
	s.logLifecycle("execution_enqueued", prepared.Invocation.SourceType, prepared.Invocation.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "queued")
	s.logf("runtime.loop: slash enqueued delivery=%s", prepared.Invocation.DeliveryID)
	return nil
}

func (s *foregroundRuntimeStarter) executeQueuedInvocation(ctx context.Context, coordinator *runtime.SlackTurnCoordinator, request runtime.ExecutionRequest) {
	switch request.SourceType {
	case slack.InboundSourceMention:
		s.executeMentionRequest(ctx, coordinator, request)
	case slack.InboundSourceMessage:
		s.executeMessageRequest(ctx, coordinator, request)
	case slack.InboundSourceSlash:
		s.executeSlashRequest(ctx, coordinator, request)
	default:
		s.logf("runtime.loop: ignored execution request delivery=%s source=%s", request.DeliveryID, request.SourceType)
	}
}

func (s *foregroundRuntimeStarter) executeMentionRequest(ctx context.Context, coordinator *runtime.SlackTurnCoordinator, request runtime.ExecutionRequest) {
	tracker := s.lifecycleTracker()
	prepared, err := request.PreparedEvent()
	if err != nil {
		if tracker != nil {
			_ = tracker.RecordFailed(ctx, request, err)
		}
		s.markDeliveryCompleted(ctx, request.SourceType, request.DeliveryID, "failed")
		s.logf("runtime.loop: mention async failed delivery=%s: %v", request.DeliveryID, err)
		return
	}
	if tracker != nil {
		if err := tracker.RecordRunning(ctx, request); err != nil {
			s.markDeliveryCompleted(ctx, request.SourceType, request.DeliveryID, "failed")
			s.logf("runtime.loop: mention async failed delivery=%s: %v", request.DeliveryID, err)
			return
		}
	}
	command := runtime.ParseSlackCommand(request.CommandText)

	result, err := coordinator.ExecuteClaimed(ctx, prepared, func(ctx context.Context, prepared runtime.PreparedSlackEvent) error {
		s.logLifecycle("execution_started", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "processing", "session="+prepared.SessionName)
		s.logf(
			"runtime.loop: dispatching mention delivery=%s project=%s session=%s",
			prepared.Event.ID,
			prepared.Project.Name,
			prepared.SessionName,
		)

		if !command.ShouldExecute() {
			if tracker != nil {
				if err := tracker.RecordRendering(ctx, request, runtime.ExecutionCheckpoint{
					Kind:    "slack_usage",
					Summary: "rendering mention usage",
				}); err != nil {
					return err
				}
			}
			if err := s.client.PostThreadMessage(ctx, slack.Message{
				ChannelID: prepared.Project.SlackChannelID,
				ThreadTS:  prepared.ThreadTS,
				Text:      mentionUsageText(),
			}); err != nil {
				if tracker != nil {
					_ = tracker.RecordFailed(ctx, request, err)
				}
				return err
			}
			if tracker != nil {
				if err := tracker.RecordProcessed(ctx, request, runtime.ExecutionCheckpoint{
					Kind:    "slack_usage",
					Summary: "mention usage rendered",
				}); err != nil {
					return err
				}
			}
			s.logf("runtime.loop: rendered mention usage thread=%s", prepared.ThreadTS)
			return nil
		}

		promptResult, err := s.collectPromptEvents(ctx, prepared, request, command.ACPXPrompt(), command.Kind == runtime.SlackCommandAsk)
		if err != nil {
			if tracker != nil {
				_ = tracker.RecordFailed(ctx, request, err)
			}
			return err
		}
		if promptResult.cancelled {
			if tracker != nil {
				if err := tracker.RecordCancelled(ctx, request, "cancelled by operator", promptResult.checkpoint); err != nil {
					return err
				}
			}
			return runtime.ErrExecutionCancelled
		}
		if tracker != nil {
			if err := tracker.RecordProcessed(ctx, request, promptResult.checkpoint); err != nil {
				return err
			}
		}

		s.logf(
			"runtime.loop: acpx completed session=%s event_count=%d",
			promptResult.sessionName,
			len(promptResult.events),
		)
		s.logf(
			"runtime.loop: acpx events session=%s payload=%s",
			promptResult.sessionName,
			summarizeACPXEvents(promptResult.events, 12),
		)

		s.logf("runtime.loop: rendered slack reply thread=%s session=%s", prepared.ThreadTS, promptResult.sessionName)
		return nil
	})
	if err != nil {
		s.logLifecycle("execution_failed", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "failed", "session="+prepared.SessionName, "error="+strconv.Quote(err.Error()))
		s.logf("runtime.loop: mention async failed delivery=%s: %v", request.DeliveryID, err)
		var renderedErr *renderedPromptStreamError
		if !errors.As(err, &renderedErr) {
			_ = s.postTurnError(ctx, prepared, err)
		}
		return
	}
	if result.Duplicate {
		s.logLifecycle("duplicate_skipped", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "duplicate")
		s.logf("runtime.loop: duplicate mention skipped delivery=%s", request.DeliveryID)
		return
	}
	if result.Cancelled {
		s.logLifecycle("execution_cancelled", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "cancelled", "session="+prepared.SessionName)
		s.logf("runtime.loop: mention cancelled delivery=%s session=%s", request.DeliveryID, prepared.SessionName)
		return
	}
	s.logLifecycle("execution_completed", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "processed", "session="+prepared.SessionName)
	s.logf("runtime.loop: mention processed delivery=%s session=%s", request.DeliveryID, prepared.SessionName)
}

func (s *foregroundRuntimeStarter) executeMessageRequest(ctx context.Context, coordinator *runtime.SlackTurnCoordinator, request runtime.ExecutionRequest) {
	tracker := s.lifecycleTracker()
	prepared, err := request.PreparedEvent()
	if err != nil {
		if tracker != nil {
			_ = tracker.RecordFailed(ctx, request, err)
		}
		s.markDeliveryCompleted(ctx, request.SourceType, request.DeliveryID, "failed")
		s.logf("runtime.loop: message async failed delivery=%s: %v", request.DeliveryID, err)
		return
	}
	if tracker != nil {
		if err := tracker.RecordRunning(ctx, request); err != nil {
			s.markDeliveryCompleted(ctx, request.SourceType, request.DeliveryID, "failed")
			s.logf("runtime.loop: message async failed delivery=%s: %v", request.DeliveryID, err)
			return
		}
	}

	result, err := coordinator.ExecuteClaimed(ctx, prepared, func(ctx context.Context, prepared runtime.PreparedSlackEvent) error {
		s.logLifecycle("execution_started", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "processing", "session="+prepared.SessionName)
		s.logf(
			"runtime.loop: dispatching message delivery=%s project=%s session=%s",
			prepared.Event.ID,
			prepared.Project.Name,
			prepared.SessionName,
		)

		prompt := strings.TrimSpace(prepared.Event.Text)
		if prompt == "" {
			if tracker != nil {
				if err := tracker.RecordProcessed(ctx, request, runtime.ExecutionCheckpoint{
					Kind:    "empty_message",
					Summary: "ignored empty threaded message",
				}); err != nil {
					return err
				}
			}
			s.logf("runtime.loop: ignored empty message thread=%s", prepared.ThreadTS)
			return nil
		}

		promptResult, err := s.collectPromptEvents(ctx, prepared, request, prompt, false)
		if err != nil {
			if tracker != nil {
				_ = tracker.RecordFailed(ctx, request, err)
			}
			return err
		}
		if promptResult.cancelled {
			if tracker != nil {
				if err := tracker.RecordCancelled(ctx, request, "cancelled by operator", promptResult.checkpoint); err != nil {
					return err
				}
			}
			return runtime.ErrExecutionCancelled
		}
		if tracker != nil {
			if err := tracker.RecordProcessed(ctx, request, promptResult.checkpoint); err != nil {
				return err
			}
		}

		s.logf(
			"runtime.loop: acpx completed session=%s event_count=%d",
			promptResult.sessionName,
			len(promptResult.events),
		)
		s.logf(
			"runtime.loop: acpx events session=%s payload=%s",
			promptResult.sessionName,
			summarizeACPXEvents(promptResult.events, 12),
		)
		s.logf("runtime.loop: rendered message reply thread=%s session=%s", prepared.ThreadTS, promptResult.sessionName)
		return nil
	})
	if err != nil {
		s.logLifecycle("execution_failed", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "failed", "session="+prepared.SessionName, "error="+strconv.Quote(err.Error()))
		s.logf("runtime.loop: message async failed delivery=%s: %v", request.DeliveryID, err)
		var renderedErr *renderedPromptStreamError
		if !errors.As(err, &renderedErr) {
			_ = s.postTurnError(ctx, prepared, err)
		}
		return
	}
	if result.Duplicate {
		s.logLifecycle("duplicate_skipped", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "duplicate")
		s.logf("runtime.loop: duplicate message skipped delivery=%s", request.DeliveryID)
		return
	}
	if result.Cancelled {
		s.logLifecycle("execution_cancelled", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "cancelled", "session="+prepared.SessionName)
		s.logf("runtime.loop: message cancelled delivery=%s session=%s", request.DeliveryID, prepared.SessionName)
		return
	}
	s.logLifecycle("execution_completed", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "processed", "session="+prepared.SessionName)
	s.logf("runtime.loop: message processed delivery=%s session=%s", request.DeliveryID, prepared.SessionName)
}

func (s *foregroundRuntimeStarter) renderLocalMentionCommand(ctx context.Context, prepared runtime.PreparedSlackEvent, command runtime.SlackCommand) error {
	switch command.Kind {
	case runtime.SlackCommandClose:
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
		runtimeRepo: s.runtimeRepo,
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

	var latest runtime.ExecutionState
	if store, ok := s.runtimeRepo.(runtime.ExecutionStateStore); ok {
		latest, stateErr = store.LoadLatestExecutionByThread(ctx, threadTS)
		if stateErr != nil && !errors.Is(stateErr, storage.ErrNotFound) {
			return "", stateErr
		}
	} else {
		stateErr = storage.ErrNotFound
	}

	if state.ThreadTS == "" && errors.Is(stateErr, storage.ErrNotFound) {
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
	} else if !errors.Is(stateErr, storage.ErrNotFound) {
		lines = append(lines, "status: "+latest.Status)
		if sessionName := strings.TrimSpace(latest.SessionName); sessionName != "" {
			lines = append(lines, "session: "+sessionName)
		}
	}
	if !errors.Is(stateErr, storage.ErrNotFound) {
		lines = append(lines, fmt.Sprintf("execution: %s (%s)", latest.ExecutionID, latest.Status))
		if diagnostic := strings.TrimSpace(latest.LastError); diagnostic != "" {
			lines = append(lines, "detail: "+diagnostic)
		}
	}

	return strings.Join(lines, "\n"), nil
}

func (s *foregroundRuntimeStarter) executeSlashRequest(ctx context.Context, coordinator *runtime.SlackTurnCoordinator, request runtime.ExecutionRequest) {
	tracker := s.lifecycleTracker()
	command := runtime.ParseSlackCommand(request.CommandText)
	rawCommand := strings.TrimSpace(request.CommandText)

	rootMessage, err := s.client.PostMessage(ctx, slack.Message{
		ChannelID: request.Project.SlackChannelID,
		Text:      slashStartText(rawCommand),
	})
	if err != nil {
		if tracker != nil {
			_ = tracker.RecordFailed(ctx, request, err)
		}
		s.markDeliveryCompleted(ctx, request.SourceType, request.DeliveryID, "failed")
		s.logLifecycle("execution_completed", request.SourceType, request.DeliveryID, request.Project.SlackChannelID, request.Project.Name, "failed", "error="+strconv.Quote(err.Error()))
		s.logf("runtime.loop: slash start message failed delivery=%s: %v", request.DeliveryID, err)
		_ = s.postSlashBootstrapError(ctx, request, err)
		return
	}

	request = request.WithThread(rootMessage.Timestamp)
	if tracker != nil {
		if err := tracker.RecordRunning(ctx, request); err != nil {
			s.markDeliveryCompleted(ctx, request.SourceType, request.DeliveryID, "failed")
			s.logLifecycle("execution_completed", request.SourceType, request.DeliveryID, request.Project.SlackChannelID, request.Project.Name, "failed", "error="+strconv.Quote(err.Error()))
			s.logf("runtime.loop: slash async failed delivery=%s: %v", request.DeliveryID, err)
			return
		}
	}
	preparedEvent, err := request.PreparedEvent()
	if err != nil {
		if tracker != nil {
			_ = tracker.RecordFailed(ctx, request, err)
		}
		s.markDeliveryCompleted(ctx, request.SourceType, request.DeliveryID, "failed")
		s.logLifecycle("execution_completed", request.SourceType, request.DeliveryID, request.Project.SlackChannelID, request.Project.Name, "failed", "error="+strconv.Quote(err.Error()))
		s.logf("runtime.loop: slash async failed delivery=%s: %v", request.DeliveryID, err)
		return
	}

	s.logf(
		"runtime.loop: dispatching slash delivery=%s project=%s session=%s thread=%s",
		request.DeliveryID,
		request.Project.Name,
		preparedEvent.SessionName,
		preparedEvent.ThreadTS,
	)

	result, err := coordinator.ExecuteClaimed(ctx, preparedEvent, func(ctx context.Context, prepared runtime.PreparedSlackEvent) error {
		s.logLifecycle("execution_started", prepared.SourceType, prepared.DeliveryID, prepared.Project.SlackChannelID, prepared.Project.Name, "processing", "session="+prepared.SessionName)
		if !command.ShouldExecute() {
			if tracker != nil {
				if err := tracker.RecordRendering(ctx, request, runtime.ExecutionCheckpoint{
					Kind:    "slack_usage",
					Summary: "rendering slash usage",
				}); err != nil {
					return err
				}
			}
			if err := s.client.PostThreadMessage(ctx, slack.Message{
				ChannelID: prepared.Project.SlackChannelID,
				ThreadTS:  prepared.ThreadTS,
				Text:      slashUsageText(),
			}); err != nil {
				if tracker != nil {
					_ = tracker.RecordFailed(ctx, request, err)
				}
				return err
			}
			if tracker != nil {
				if err := tracker.RecordProcessed(ctx, request, runtime.ExecutionCheckpoint{
					Kind:    "slack_usage",
					Summary: "slash usage rendered",
				}); err != nil {
					return err
				}
			}
			s.logf("runtime.loop: rendered slash usage thread=%s", prepared.ThreadTS)
			return nil
		}

		promptResult, err := s.collectPromptEvents(ctx, prepared, request, command.ACPXPrompt(), command.Kind == runtime.SlackCommandAsk)
		if err != nil {
			if tracker != nil {
				_ = tracker.RecordFailed(ctx, request, err)
			}
			return err
		}
		if promptResult.cancelled {
			if tracker != nil {
				if err := tracker.RecordCancelled(ctx, request, "cancelled by operator", promptResult.checkpoint); err != nil {
					return err
				}
			}
			return runtime.ErrExecutionCancelled
		}
		if tracker != nil {
			if err := tracker.RecordProcessed(ctx, request, promptResult.checkpoint); err != nil {
				return err
			}
		}

		s.logf(
			"runtime.loop: acpx completed session=%s event_count=%d",
			promptResult.sessionName,
			len(promptResult.events),
		)
		s.logf(
			"runtime.loop: acpx events session=%s payload=%s",
			promptResult.sessionName,
			summarizeACPXEvents(promptResult.events, 12),
		)

		s.logf("runtime.loop: rendered slash reply thread=%s session=%s", prepared.ThreadTS, promptResult.sessionName)
		return nil
	})
	if err != nil {
		s.logLifecycle("execution_failed", preparedEvent.SourceType, preparedEvent.DeliveryID, request.Project.SlackChannelID, request.Project.Name, "failed", "session="+preparedEvent.SessionName, "error="+strconv.Quote(err.Error()))
		s.logf("runtime.loop: slash async failed delivery=%s: %v", request.DeliveryID, err)
		var renderedErr *renderedPromptStreamError
		if !errors.As(err, &renderedErr) {
			_ = s.postTurnError(ctx, preparedEvent, err)
		}
		return
	}
	if result.Duplicate {
		s.logLifecycle("duplicate_skipped", preparedEvent.SourceType, preparedEvent.DeliveryID, request.Project.SlackChannelID, request.Project.Name, "duplicate")
		s.logf("runtime.loop: duplicate slash skipped delivery=%s", request.DeliveryID)
		return
	}
	if result.Cancelled {
		s.logLifecycle("execution_cancelled", preparedEvent.SourceType, preparedEvent.DeliveryID, request.Project.SlackChannelID, request.Project.Name, "cancelled", "session="+preparedEvent.SessionName)
		s.logf("runtime.loop: slash cancelled delivery=%s session=%s", request.DeliveryID, preparedEvent.SessionName)
		return
	}
	s.logLifecycle("execution_completed", preparedEvent.SourceType, preparedEvent.DeliveryID, request.Project.SlackChannelID, request.Project.Name, "processed", "session="+preparedEvent.SessionName)
	s.logf("runtime.loop: slash processed delivery=%s session=%s", request.DeliveryID, preparedEvent.SessionName)
}

func (s *foregroundRuntimeStarter) collectPromptEvents(ctx context.Context, prepared runtime.PreparedSlackEvent, request runtime.ExecutionRequest, prompt string, forceNew bool) (promptExecutionResult, error) {
	stream, err := s.adapter.StartPrompt(ctx, acpxadapter.SessionRequest{
		ProjectPath: prepared.Project.LocalPath,
		ThreadTS:    prepared.ThreadTS,
		Prompt:      prompt,
		ForceNew:    forceNew,
	})
	if err != nil {
		return promptExecutionResult{}, err
	}
	defer func() {
		_ = stream.Close()
	}()

	publisher, err := s.renderer.NewProgressPublisher(runtime.SlackThreadRenderRequest{
		ChannelID:   prepared.Project.SlackChannelID,
		ThreadTS:    prepared.ThreadTS,
		SessionName: stream.SessionName(),
	}, runtime.SlackThreadProgressPublisherConfig{})
	if err != nil {
		return promptExecutionResult{}, err
	}

	request.SessionName = stream.SessionName()

	events := make([]runtime.ACPXTurnEvent, 0, 16)
	eventCh := stream.Events()
	timer := time.NewTimer(time.Hour)
	stopTimer(timer)
	var timerCh <-chan time.Time
	cancelled := false
	checkpoint := runtime.ExecutionCheckpoint{}
	tracker := s.lifecycleTracker()

	for {
		select {
		case <-ctx.Done():
			return promptExecutionResult{}, ctx.Err()
		case <-timerCh:
			timerCh = nil
			if err := s.flushProgress(ctx, prepared, request, stream.SessionName(), publisher, "timer"); err != nil {
				return promptExecutionResult{}, err
			}
		case event, ok := <-eventCh:
			if !ok {
				waitErr := stream.Wait()
				if publisher.HasPendingProgress() && (waitErr != nil || cancelled || publisher.HasNonAssistantProgress()) {
					if err := s.flushProgress(ctx, prepared, request, stream.SessionName(), publisher, "terminal"); err != nil {
						return promptExecutionResult{}, err
					}
				}
				if err := publisher.Finish(ctx, waitErr); err != nil {
					return promptExecutionResult{}, err
				}
				if cancelled {
					return promptExecutionResult{
						sessionName: stream.SessionName(),
						events:      events,
						cancelled:   true,
						checkpoint:  checkpoint,
					}, nil
				}
				if waitErr != nil {
					return promptExecutionResult{}, &renderedPromptStreamError{err: waitErr}
				}
				return promptExecutionResult{
					sessionName: stream.SessionName(),
					events:      events,
					checkpoint:  checkpoint,
				}, nil
			}

			events = append(events, event)
			if err := publisher.Consume(ctx, event); err != nil {
				return promptExecutionResult{}, err
			}
			checkpoint = executionCheckpointFromEvent(event)
			if event.Kind == runtime.ACPXEventSessionCancelled {
				cancelled = true
			}
			if tracker != nil {
				if err := tracker.RecordRendering(ctx, request, checkpoint); err != nil {
					return promptExecutionResult{}, err
				}
			}
			if shouldFlushProgressForTerminalEvent(event, publisher) {
				if err := s.flushProgress(ctx, prepared, request, stream.SessionName(), publisher, string(event.Kind)); err != nil {
					return promptExecutionResult{}, err
				}
			}

			if publisher.ShouldFlushByCount() {
				stopTimer(timer)
				timerCh = nil
				if err := s.flushProgress(ctx, prepared, request, stream.SessionName(), publisher, "count"); err != nil {
					return promptExecutionResult{}, err
				}
				continue
			}

			if publisher.HasPendingProgress() && timerCh == nil {
				timer.Reset(publisher.FlushInterval())
				timerCh = timer.C
			}
		}
	}
}

func (s *foregroundRuntimeStarter) flushProgress(ctx context.Context, prepared runtime.PreparedSlackEvent, request runtime.ExecutionRequest, sessionName string, publisher *runtime.SlackThreadProgressPublisher, reason string) error {
	if publisher == nil || !publisher.HasPendingProgress() {
		return nil
	}
	count := publisher.PendingProgressCount()
	if err := publisher.Flush(ctx); err != nil {
		return err
	}
	s.logLifecycle(
		"progress_flushed",
		prepared.SourceType,
		request.DeliveryID,
		prepared.Project.SlackChannelID,
		prepared.Project.Name,
		"rendering",
		"session="+sessionName,
		"count="+strconv.Itoa(count),
		"reason="+reason,
	)
	return nil
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

func (s *foregroundRuntimeStarter) postSlashBootstrapError(ctx context.Context, request runtime.ExecutionRequest, err error) error {
	if s == nil || s.client == nil || strings.TrimSpace(request.ResponseURL) == "" {
		return nil
	}

	client, ok := s.client.(slack.ResponseURLClient)
	if !ok {
		return nil
	}

	return client.PostResponseURLMessage(ctx, slack.ResponseURLMessage{
		ResponseURL:  strings.TrimSpace(request.ResponseURL),
		Text:         "Session error: " + err.Error(),
		ResponseType: slack.ResponseTypeEphemeral,
	})
}

func (s *foregroundRuntimeStarter) logf(format string, args ...any) {
	if s != nil && s.debugf != nil {
		s.debugf(format, args...)
	}
}

func (s *foregroundRuntimeStarter) lifecycleTracker() *runtime.ExecutionLifecycleTracker {
	if s == nil {
		return nil
	}
	if s.executionTracker != nil {
		return s.executionTracker
	}
	store, ok := s.runtimeRepo.(runtime.ExecutionStateStore)
	if !ok {
		return nil
	}
	s.executionTracker = runtime.NewExecutionLifecycleTracker(store)
	return s.executionTracker
}

func (s *foregroundRuntimeStarter) reconcileNonTerminalExecutions(ctx context.Context) error {
	recoveryStore, ok := s.runtimeRepo.(runtime.ExecutionRecoveryStore)
	if !ok {
		return nil
	}

	service := runtime.NewExecutionRecoveryService(recoveryStore, s.notifyRecoveredExecution)
	results, err := service.ReconcileNonTerminalExecutions(ctx)
	if err != nil {
		return err
	}
	for _, result := range results {
		fields := []string{
			"execution_id=" + result.ExecutionID,
			"previous_status=" + result.PreviousStatus,
			"recovered_status=" + result.RecoveredStatus,
		}
		if result.NotifiedSlack {
			fields = append(fields, "slack_notified=true")
		}
		s.logLifecycle("execution_reconciled", result.SourceType, result.DeliveryID, result.ChannelID, result.ProjectName, result.RecoveredStatus, fields...)
	}
	return nil
}

func (s *foregroundRuntimeStarter) notifyRecoveredExecution(ctx context.Context, state runtime.ExecutionState, reason string) error {
	if s == nil || s.client == nil {
		return nil
	}
	if strings.TrimSpace(state.ChannelID) == "" || strings.TrimSpace(state.ThreadTS) == "" {
		return nil
	}
	return s.client.PostThreadMessage(ctx, slack.Message{
		ChannelID: state.ChannelID,
		ThreadTS:  state.ThreadTS,
		Text:      "Session error: " + reason,
	})
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

func (s *foregroundRuntimeStarter) markDeliveryCompleted(ctx context.Context, sourceType, deliveryID, status string) {
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
	_ = s.runtimeRepo.SaveEventDedupe(ctx, dedupe)
}

func executionCheckpointFromEvent(event runtime.ACPXTurnEvent) runtime.ExecutionCheckpoint {
	checkpoint := runtime.ExecutionCheckpoint{
		Kind: string(event.Kind),
	}

	switch event.Kind {
	case runtime.ACPXEventToolStarted, runtime.ACPXEventToolFinished:
		checkpoint.Summary = strings.TrimSpace(event.ToolName)
		if text := strings.TrimSpace(event.Text); text != "" {
			if checkpoint.Summary != "" {
				checkpoint.Summary += ": "
			}
			checkpoint.Summary += text
		}
	default:
		checkpoint.Summary = strings.TrimSpace(event.Text)
	}

	return checkpoint
}

func summarizeACPXEvents(events []runtime.ACPXTurnEvent, limit int) string {
	if len(events) == 0 {
		return ""
	}
	truncated := false
	if limit > 0 && len(events) > limit {
		truncated = true
		events = events[:limit]
	}

	parts := make([]string, 0, len(events))
	for _, event := range events {
		part := string(event.Kind)
		if text := strings.TrimSpace(event.Text); text != "" {
			part += ":" + text
		}
		if tool := strings.TrimSpace(event.ToolName); tool != "" {
			part += ":" + tool
		}
		parts = append(parts, part)
	}

	summary := strings.Join(parts, " | ")
	if truncated {
		summary += " | ...(truncated)"
	}
	return summary
}

func stopTimer(timer *time.Timer) {
	if timer == nil {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func shouldFlushProgressForTerminalEvent(event runtime.ACPXTurnEvent, publisher *runtime.SlackThreadProgressPublisher) bool {
	switch event.Kind {
	case runtime.ACPXEventAssistantMessageFinal,
		runtime.ACPXEventSessionDone:
		return publisher != nil && publisher.HasNonAssistantProgress()
	case runtime.ACPXEventSessionError,
		runtime.ACPXEventSessionCancelled:
		return true
	default:
		return false
	}
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
	cfg, configPath, err := h.loadValidatedConfig(ctx)
	if err != nil {
		return runtimeSnapshot{}, err
	}

	storagePath, err := storage.DefaultPath()
	if err != nil {
		return runtimeSnapshot{}, err
	}

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

	store, err := storage.Open(ctx, storagePath)
	if err != nil {
		return runtimeSnapshot{}, err
	}
	defer func() {
		_ = store.Close()
	}()

	runtimeRepo := store.Runtime()
	if runtimeRepo == nil {
		return runtimeSnapshot{}, errors.New("runtime repository unavailable")
	}
	if err := h.recoverStartupExecutions(ctx, runtimeRepo); err != nil {
		return runtimeSnapshot{}, err
	}

	projectRepo := store.Projects()
	if projectRepo == nil {
		return runtimeSnapshot{}, errors.New("project repository unavailable")
	}

	projects, err := projectRepo.List(ctx)
	if err != nil {
		return runtimeSnapshot{}, err
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
			Message:          "runtime start loaded",
			ActiveThreads:    activeThreads,
			ProjectCount:     len(projects),
			EventDedupeCount: dedupeCount,
			ConfigPath:       configPath,
			StoragePath:      storagePath,
			LoadedAt:         time.Now().UTC(),
			Config:           cfg,
			Projects:         projects,
		},
		projectByChannel: make(map[string]registry.Project, len(projects)),
	}
	for _, project := range projects {
		if channelID := strings.TrimSpace(project.SlackChannelID); channelID != "" {
			snapshot.projectByChannel[channelID] = project
		}
	}

	if !snapshot.status.Config.Slack.Status().Configured {
		return runtimeSnapshot{}, errors.New("slack authentication is required for runtime start")
	}

	return snapshot, nil
}

func (h *runtimeCommandHandler) recoverStartupExecutions(ctx context.Context, runtimeRepo runtime.ExecutionRecoveryStore) error {
	if runtimeRepo == nil {
		return nil
	}

	_, err := runtime.NewExecutionRecoveryService(runtimeRepo, nil).ReconcileNonTerminalExecutions(ctx)
	return err
}

func findRunningExecutionForThread(ctx context.Context, store runtime.ExecutionRecoveryStore, threadTS string) (runtime.ExecutionState, error) {
	if err := ctx.Err(); err != nil {
		return runtime.ExecutionState{}, err
	}
	if store == nil {
		return runtime.ExecutionState{}, errors.New("execution recovery store is required")
	}
	threadTS = strings.TrimSpace(threadTS)
	if threadTS == "" {
		return runtime.ExecutionState{}, fmt.Errorf("thread timestamp is required")
	}

	states, err := store.ListExecutionStatesByStatus(ctx, runtime.ExecutionStatusRunning, runtime.ExecutionStatusRendering)
	if err != nil {
		return runtime.ExecutionState{}, err
	}

	var selected runtime.ExecutionState
	for _, state := range states {
		if state.ThreadTS != threadTS {
			continue
		}
		if selected.ExecutionID == "" || state.UpdatedAt.After(selected.UpdatedAt) {
			selected = state
		}
	}
	if selected.ExecutionID == "" {
		return runtime.ExecutionState{}, runtime.ErrNotFound
	}
	return selected, nil
}

func (h *runtimeCommandHandler) cancelExecution(ctx context.Context, runtimeRepo runtime.Store, executionStore runtime.ExecutionStateStore, execution runtime.ExecutionState, state *runtime.ThreadState, lock *runtime.ThreadLock) error {
	if execution.ThreadTS == "" {
		return h.writeCancelNoOp(ctx, "", execution.SessionName, "thread is inactive")
	}
	if h.adapter == nil {
		return errors.New("acpx adapter is required")
	}
	if err := h.adapter.Cancel(ctx, execution.ThreadTS); err != nil {
		return err
	}

	cancelledAt := time.Now().UTC()
	execution.Status = runtime.ExecutionStatusCancelled
	execution.UpdatedAt = cancelledAt
	execution.CompletedAt = &cancelledAt
	execution.CancelledAt = &cancelledAt
	execution.LastError = "cancelled by operator"
	if err := executionStore.SaveExecutionState(ctx, execution); err != nil {
		return fmt.Errorf("persist cancelled execution state for %q: %w", execution.ExecutionID, err)
	}

	if state != nil {
		state.LastStatus = runtime.ExecutionStatusCancelled
		state.UpdatedAt = cancelledAt
		if err := runtimeRepo.SaveThreadState(ctx, *state); err != nil {
			return fmt.Errorf("persist cancelled thread state for %q: %w", execution.ThreadTS, err)
		}
	}

	if lock != nil {
		if err := runtimeRepo.DeleteThreadLock(ctx, lock.ThreadTS); err != nil {
			return fmt.Errorf("release thread lock for %q: %w", lock.ThreadTS, err)
		}
	} else if err := runtimeRepo.DeleteThreadLock(ctx, execution.ThreadTS); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("release thread lock for %q: %w", execution.ThreadTS, err)
	}

	snapshot, err := h.reloadSnapshot(ctx)
	if err != nil {
		return err
	}

	report := runtime.CancelReport{
		RequestedAt: cancelledAt,
		ThreadTS:    execution.ThreadTS,
		SessionName: execution.SessionName,
		Result:      "cancelled",
		NoOp:        false,
		Message:     "active ACPX execution cancelled",
		Status:      snapshot.statusCopy(),
	}
	report.Status.Message = report.Message
	return writeJSON(h.out, report)
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
