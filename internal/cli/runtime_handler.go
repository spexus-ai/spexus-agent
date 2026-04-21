package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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

	threadTS := strings.TrimSpace(args[0])
	if threadTS == "" {
		return fmt.Errorf("runtime cancel requires exactly one thread timestamp argument")
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

	state.LastStatus = "cancelled"
	state.UpdatedAt = time.Now().UTC()
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
		RequestedAt: time.Now().UTC(),
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
	source      slack.EventSource
	client      slack.Client
	renderer    runtime.SlackThreadRenderer
	adapter     acpxadapter.Adapter
	projectRepo runtime.ProjectContextResolver
	runtimeRepo runtime.Store
	store       interface{ Close() error }
	debugf      func(string, ...any)
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

	events, err := s.source.Events(ctx)
	if err != nil {
		return err
	}

	coordinator := runtime.NewSlackTurnCoordinator(s.runtimeRepo, "runtime-start")
	s.logf("runtime.loop: connected to slack socket mode")

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-events:
			if !ok {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return errors.New("slack event source stopped")
			}
			s.logf(
				"runtime.loop: received slack event id=%s channel=%s thread=%s user=%s text=%q",
				event.ID,
				event.ChannelID,
				event.ThreadTimestamp(),
				event.UserID,
				strings.TrimSpace(event.Text),
			)

			prepared, err := runtime.PrepareSlackEvent(ctx, s.projectRepo, event)
			if err != nil {
				s.logf("runtime.loop: ignored slack event id=%s: %v", event.ID, err)
				continue
			}

			result, err := coordinator.Execute(ctx, prepared, func(ctx context.Context, prepared runtime.PreparedSlackEvent) error {
				s.logf(
					"runtime.loop: dispatching event id=%s project=%s session=%s",
					prepared.Event.ID,
					prepared.Project.Name,
					prepared.SessionName,
				)

				session, err := s.adapter.SendPrompt(ctx, acpxadapter.SessionRequest{
					ProjectPath: prepared.Project.LocalPath,
					ThreadTS:    prepared.ThreadTS,
					Prompt:      prepared.Event.Text,
				})
				if err != nil {
					return err
				}

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

				if err := runtime.RenderACPXTurnOutput(ctx, s.renderer, runtime.SlackThreadRenderRequest{
					ChannelID:   prepared.Project.SlackChannelID,
					ThreadTS:    prepared.ThreadTS,
					SessionName: session.SessionName,
				}, session.Output); err != nil {
					return err
				}

				s.logf("runtime.loop: rendered slack reply thread=%s session=%s", prepared.ThreadTS, session.SessionName)
				return nil
			})
			if err != nil {
				s.logf("runtime.loop: event failed id=%s: %v", event.ID, err)
				_ = s.postTurnError(ctx, prepared, err)
				continue
			}
			if result.Duplicate {
				s.logf("runtime.loop: duplicate slack event skipped id=%s", event.ID)
				continue
			}
			s.logf("runtime.loop: event processed id=%s session=%s", event.ID, prepared.SessionName)
		}
	}
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

func summarizeACPXOutput(output string, limit int) string {
	output = strings.TrimSpace(output)
	if limit <= 0 || len(output) <= limit {
		return output
	}
	return output[:limit] + "...(truncated)"
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
