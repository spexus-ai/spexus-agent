package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/acpxadapter"
	"github.com/spexus-ai/spexus-agent/internal/config"
	"github.com/spexus-ai/spexus-agent/internal/registry"
	runtimemodel "github.com/spexus-ai/spexus-agent/internal/runtime"
	"github.com/spexus-ai/spexus-agent/internal/slack"
	"github.com/spexus-ai/spexus-agent/internal/storage"
)

type fakeRuntimeCancelAdapter struct {
	cancelCalls []string
	cancelErr   error
}

type recordingRuntimeStarter struct {
	statuses []runtimemodel.Status
}

type fakeSlackInvocationSource struct {
	invocations []slack.InboundInvocation
}

func (s *fakeSlackInvocationSource) InboundInvocations(ctx context.Context) (<-chan slack.InboundInvocation, error) {
	out := make(chan slack.InboundInvocation, len(s.invocations))
	for _, invocation := range s.invocations {
		out <- invocation
	}
	go func() {
		<-ctx.Done()
		close(out)
	}()
	return out, nil
}

func (s *fakeSlackInvocationSource) Close() error { return nil }

type recordingExecutionQueue struct {
	mu       sync.Mutex
	requests []runtimemodel.ExecutionRequest
	enqueue  func(context.Context, runtimemodel.ExecutionRequest) error
}

func (q *recordingExecutionQueue) Enqueue(ctx context.Context, request runtimemodel.ExecutionRequest) error {
	q.mu.Lock()
	q.requests = append(q.requests, request)
	enqueue := q.enqueue
	q.mu.Unlock()
	if enqueue != nil {
		return enqueue(ctx, request)
	}
	return nil
}

func (q *recordingExecutionQueue) snapshotRequests() []runtimemodel.ExecutionRequest {
	q.mu.Lock()
	defer q.mu.Unlock()

	return append([]runtimemodel.ExecutionRequest(nil), q.requests...)
}

type recordingSlackClient struct {
	mu                  sync.Mutex
	messages            []slack.Message
	timestamps          []string
	updates             []slack.MessageUpdate
	responseURLMessages []slack.ResponseURLMessage
	postMessageErr      error
}

func (c *recordingSlackClient) PostMessage(_ context.Context, message slack.Message) (slack.PostedMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.postMessageErr != nil {
		return slack.PostedMessage{}, c.postMessageErr
	}
	timestamp := fmt.Sprintf("1713686400.%06d", len(c.messages)+100)
	c.messages = append(c.messages, message)
	c.timestamps = append(c.timestamps, timestamp)
	return slack.PostedMessage{
		ChannelID: message.ChannelID,
		Timestamp: timestamp,
	}, nil
}

func (c *recordingSlackClient) PostThreadMessage(_ context.Context, message slack.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.messages = append(c.messages, message)
	c.timestamps = append(c.timestamps, fmt.Sprintf("1713686400.%06d", len(c.messages)+100))
	return nil
}

func (c *recordingSlackClient) UpdateMessage(_ context.Context, update slack.MessageUpdate) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.updates = append(c.updates, update)
	for i, timestamp := range c.timestamps {
		if timestamp == update.Timestamp {
			c.messages[i].Text = update.Text
			return nil
		}
	}
	return fmt.Errorf("message timestamp %q not found", update.Timestamp)
}

func (c *recordingSlackClient) PostResponseURLMessage(_ context.Context, message slack.ResponseURLMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.responseURLMessages = append(c.responseURLMessages, message)
	return nil
}

func (c *recordingSlackClient) snapshotMessages() []slack.Message {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]slack.Message(nil), c.messages...)
}

func (c *recordingSlackClient) snapshotResponseURLMessages() []slack.ResponseURLMessage {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]slack.ResponseURLMessage(nil), c.responseURLMessages...)
}

func (c *recordingSlackClient) CreateChannel(context.Context, slack.CreateChannelRequest) (slack.Channel, error) {
	return slack.Channel{}, nil
}

func (c *recordingSlackClient) FindChannelByName(context.Context, string) (slack.Channel, error) {
	return slack.Channel{}, nil
}

func (c *recordingSlackClient) Close() error { return nil }

type fakePromptAdapter struct {
	mu          sync.Mutex
	results     []acpxadapter.SessionResult
	calls       []acpxadapter.SessionRequest
	sendPrompt  func(context.Context, acpxadapter.SessionRequest) (acpxadapter.SessionResult, error)
	startPrompt func(context.Context, acpxadapter.SessionRequest) (acpxadapter.PromptStream, error)
}

func (a *fakePromptAdapter) EnsureSession(context.Context, acpxadapter.SessionRequest) (acpxadapter.SessionResult, error) {
	return acpxadapter.SessionResult{}, nil
}

func (a *fakePromptAdapter) SendPrompt(ctx context.Context, req acpxadapter.SessionRequest) (acpxadapter.SessionResult, error) {
	return a.nextPromptResult(ctx, req)
}

func (a *fakePromptAdapter) StartPrompt(ctx context.Context, req acpxadapter.SessionRequest) (acpxadapter.PromptStream, error) {
	a.mu.Lock()
	startPrompt := a.startPrompt
	a.mu.Unlock()
	if startPrompt != nil {
		a.recordCall(req)
		return startPrompt(ctx, req)
	}

	result, err := a.nextPromptResult(ctx, req)
	if err != nil {
		return nil, err
	}

	events, err := acpxadapter.TranslatePromptOutput(result.Output)
	if err != nil {
		return nil, err
	}

	return &fakePromptStream{
		sessionName: result.SessionName,
		events:      events,
	}, nil
}

func (a *fakePromptAdapter) recordCall(req acpxadapter.SessionRequest) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, req)
}

func (a *fakePromptAdapter) nextPromptResult(ctx context.Context, req acpxadapter.SessionRequest) (acpxadapter.SessionResult, error) {
	a.mu.Lock()
	a.calls = append(a.calls, req)
	sendPrompt := a.sendPrompt
	if sendPrompt != nil {
		a.mu.Unlock()
		return sendPrompt(ctx, req)
	}
	if len(a.results) == 0 {
		a.mu.Unlock()
		return acpxadapter.SessionResult{SessionName: acpxadapter.SessionName(req.ThreadTS)}, nil
	}
	result := a.results[0]
	a.results = a.results[1:]
	a.mu.Unlock()
	return result, nil
}

func (a *fakePromptAdapter) snapshotCalls() []acpxadapter.SessionRequest {
	a.mu.Lock()
	defer a.mu.Unlock()

	return append([]acpxadapter.SessionRequest(nil), a.calls...)
}

func (a *fakePromptAdapter) Status(context.Context, string) (acpxadapter.SessionResult, error) {
	return acpxadapter.SessionResult{}, nil
}

func (a *fakePromptAdapter) Cancel(context.Context, string) error { return nil }

func (f *fakeRuntimeCancelAdapter) EnsureSession(context.Context, acpxadapter.SessionRequest) (acpxadapter.SessionResult, error) {
	return acpxadapter.SessionResult{}, nil
}

func (f *fakeRuntimeCancelAdapter) SendPrompt(context.Context, acpxadapter.SessionRequest) (acpxadapter.SessionResult, error) {
	return acpxadapter.SessionResult{}, nil
}

func (f *fakeRuntimeCancelAdapter) StartPrompt(context.Context, acpxadapter.SessionRequest) (acpxadapter.PromptStream, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeRuntimeCancelAdapter) Status(context.Context, string) (acpxadapter.SessionResult, error) {
	return acpxadapter.SessionResult{}, nil
}

func (f *fakeRuntimeCancelAdapter) Cancel(_ context.Context, threadTS string) error {
	f.cancelCalls = append(f.cancelCalls, threadTS)
	return f.cancelErr
}

func (r *recordingRuntimeStarter) Start(_ context.Context, status runtimemodel.Status) error {
	r.statuses = append(r.statuses, status)
	return nil
}

type fakePromptStream struct {
	sessionName string
	events      []acpxadapter.Event
	waitErr     error
}

func (s *fakePromptStream) SessionName() string {
	return s.sessionName
}

func (s *fakePromptStream) Events() <-chan acpxadapter.Event {
	out := make(chan acpxadapter.Event, len(s.events))
	for _, event := range s.events {
		out <- event
	}
	close(out)
	return out
}

func (s *fakePromptStream) Wait() error {
	return s.waitErr
}

func (s *fakePromptStream) Close() error {
	return nil
}

var _ io.Closer = (*fakePromptStream)(nil)

type controlledPromptStream struct {
	sessionName string
	events      chan acpxadapter.Event
	waitCh      chan error
	closeErr    error
}

func (s *controlledPromptStream) SessionName() string {
	return s.sessionName
}

func (s *controlledPromptStream) Events() <-chan acpxadapter.Event {
	return s.events
}

func (s *controlledPromptStream) Wait() error {
	return <-s.waitCh
}

func (s *controlledPromptStream) Close() error {
	return s.closeErr
}

// Test: runtime start rejects unexpected arguments before attempting daemon startup.
// Validates: AC-1797 (REQ-1157 - runtime start is exposed as an explicit command with a stable CLI contract)
func TestRuntimeStartRejectsArguments(t *testing.T) {
	t.Parallel()

	handler := &runtimeCommandHandler{}
	err := handler.Start(context.Background(), []string{"unexpected"})
	if err == nil || !strings.Contains(err.Error(), `runtime start does not accept argument "unexpected"`) {
		t.Fatalf("Start() error = %v, want argument rejection", err)
	}
}

// Test: runtime startup loads valid config, initializes storage.sqlite3 if absent, and reports a healthy status snapshot.
// Validates: AC-1780 (REQ-1141 - runtime operates as a Linux user-space process), AC-1781 (REQ-1142 - runtime loads config at startup), AC-1782 (REQ-1143 - runtime loads storage at startup), AC-1783 (REQ-1144 - runtime initializes storage if absent)
func TestRuntimeStatusBootstrapsStorageAndReportsHealthyStatus(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	workspacePath := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspacePath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	cfgStore := config.NewFileStore("")
	if _, err := cfgStore.SetBaseWorkspacePath(ctx, workspacePath); err != nil {
		t.Fatalf("SetBaseWorkspacePath() error = %v", err)
	}

	var out bytes.Buffer
	handler := &runtimeCommandHandler{
		configStore: cfgStore,
		out:         &out,
	}

	app := &App{
		Runtime: handler,
		Out:     &out,
		Err:     &out,
	}

	if code := app.Run([]string{"runtime"}); code != 0 {
		t.Fatalf("Run() exit code = %d, want 0; output=%s", code, out.String())
	}

	var status runtimemodel.Status
	if err := json.Unmarshal(out.Bytes(), &status); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; output=%s", err, out.String())
	}

	if !status.Running || !status.Healthy {
		t.Fatalf("status = %#v, want running and healthy", status)
	}
	if status.Config.BaseWorkspacePath != workspacePath {
		t.Fatalf("status config base path = %q, want %q", status.Config.BaseWorkspacePath, workspacePath)
	}
	if status.ProjectCount != 0 {
		t.Fatalf("status project count = %d, want 0", status.ProjectCount)
	}
	if len(status.Projects) != 0 {
		t.Fatalf("status projects = %#v, want empty snapshot", status.Projects)
	}

	storagePath, err := storage.DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath() error = %v", err)
	}
	if _, err := os.Stat(storagePath); err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
}

// Test: runtime start loads config, Slack auth, and project registry before handing off to the steady-state starter.
// Validates: AC-1796 (REQ-1158 - runtime start runs as a regular user-level process), AC-1797 (REQ-1157 - explicit start command), AC-1798 (REQ-1159 - runtime start loads required state before event loop)
func TestRuntimeStartLoadsStartupStateBeforeSteadyState(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	workspacePath := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspacePath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	cfgStore := config.NewFileStore("")
	if _, err := cfgStore.SetBaseWorkspacePath(ctx, workspacePath); err != nil {
		t.Fatalf("SetBaseWorkspacePath() error = %v", err)
	}
	if _, err := cfgStore.SetSlackAuth(ctx, config.SlackAuth{
		BotToken:    "xoxb-secret",
		AppToken:    "xapp-secret",
		WorkspaceID: "T123",
	}); err != nil {
		t.Fatalf("SetSlackAuth() error = %v", err)
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	if err := store.Projects().Upsert(ctx, registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(workspacePath, "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C12345678",
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	var out bytes.Buffer
	starter := &recordingRuntimeStarter{}
	handler := &runtimeCommandHandler{
		configStore: cfgStore,
		starter:     starter,
		out:         &out,
	}

	if err := handler.Start(ctx, nil); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	if len(starter.statuses) != 1 {
		t.Fatalf("starter calls = %d, want 1", len(starter.statuses))
	}

	status := starter.statuses[0]
	if !status.Running || !status.Healthy {
		t.Fatalf("startup status = %#v, want running and healthy", status)
	}
	if status.Config.BaseWorkspacePath != workspacePath {
		t.Fatalf("startup config base path = %q, want %q", status.Config.BaseWorkspacePath, workspacePath)
	}
	if !status.Config.Slack.Status().Configured {
		t.Fatalf("startup status did not load Slack auth: %#v", status.Config.Slack)
	}
	if status.ProjectCount != 1 {
		t.Fatalf("startup project count = %d, want 1", status.ProjectCount)
	}
	if len(status.Projects) != 1 || status.Projects[0].Name != "alpha" {
		t.Fatalf("startup projects = %#v, want alpha snapshot", status.Projects)
	}
}

// Test: runtime start fails fast when Slack auth has not been configured.
// Validates: AC-1799 (REQ-1160 - invalid startup state fails fast without entering a partial running state)
func TestRuntimeStartFailsWithoutSlackAuth(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	workspacePath := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspacePath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	cfgStore := config.NewFileStore("")
	if _, err := cfgStore.SetBaseWorkspacePath(ctx, workspacePath); err != nil {
		t.Fatalf("SetBaseWorkspacePath() error = %v", err)
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	var out bytes.Buffer
	handler := &runtimeCommandHandler{
		configStore: cfgStore,
		starter:     &recordingRuntimeStarter{},
		out:         &out,
	}

	if err := handler.Start(ctx, nil); err == nil || !strings.Contains(err.Error(), "slack authentication is required") {
		t.Fatalf("Start() error = %v, want slack authentication failure", err)
	}
}

// Test: runtime start fails fast when storage.sqlite3 has not been initialized yet.
// Validates: AC-1799 (REQ-1160 - missing runtime dependencies fail fast without entering a partial running state)
func TestRuntimeStartFailsWhenStorageIsMissing(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	workspacePath := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspacePath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	cfgStore := config.NewFileStore("")
	if _, err := cfgStore.SetBaseWorkspacePath(ctx, workspacePath); err != nil {
		t.Fatalf("SetBaseWorkspacePath() error = %v", err)
	}
	if _, err := cfgStore.SetSlackAuth(ctx, config.SlackAuth{
		BotToken:    "xoxb-secret",
		AppToken:    "xapp-secret",
		WorkspaceID: "T123",
	}); err != nil {
		t.Fatalf("SetSlackAuth() error = %v", err)
	}

	handler := &runtimeCommandHandler{
		configStore: cfgStore,
		starter:     &recordingRuntimeStarter{},
		out:         &bytes.Buffer{},
	}

	err := handler.Start(ctx, nil)
	if err == nil || !strings.Contains(err.Error(), "storage database") || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("Start() error = %v, want missing storage failure", err)
	}
}

// Test: runtime start rejects unexpected arguments other than the supported debug flag.
// Validates: AC-1797 (REQ-1157 - runtime start exposes an explicit, bounded CLI contract)
func TestRuntimeStartRejectsUnexpectedArgs(t *testing.T) {
	t.Parallel()

	handler := &runtimeCommandHandler{out: &bytes.Buffer{}}

	err := handler.Start(context.Background(), []string{"--nope"})
	if err == nil || !strings.Contains(err.Error(), `runtime start does not accept argument "--nope"`) {
		t.Fatalf("Start() error = %v, want unexpected argument failure", err)
	}
}

func TestRawSocketDebugEnabledDefaultsOff(t *testing.T) {
	t.Setenv(rawSocketDebugEnvVar, "")

	if rawSocketDebugEnabled() {
		t.Fatalf("rawSocketDebugEnabled() = true, want false")
	}
}

func TestRawSocketDebugEnabledRecognizesTruthyValues(t *testing.T) {
	for _, value := range []string{"1", "true", "TRUE", "yes", "on"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv(rawSocketDebugEnvVar, value)
			if !rawSocketDebugEnabled() {
				t.Fatalf("rawSocketDebugEnabled() = false for %q, want true", value)
			}
		})
	}
}

// Test: runtime start emits debug trace lines to stdout when requested.
// Validates: AC-1797 (REQ-1157 - runtime start provides explicit operator-visible startup tracing), AC-1798 (REQ-1159 - runtime start reports loaded config and registry state before steady state), AC-1801 (REQ-1162 - runtime lifecycle surface provides an operator-visible stop-compatible path)
func TestRuntimeStartDebugWritesForegroundTrace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	home := t.TempDir()
	t.Setenv("HOME", home)

	workspacePath := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspacePath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	cfgStore := config.NewFileStore("")
	if _, err := cfgStore.SetBaseWorkspacePath(ctx, workspacePath); err != nil {
		t.Fatalf("SetBaseWorkspacePath() error = %v", err)
	}
	if _, err := cfgStore.SetSlackAuth(ctx, config.SlackAuth{
		BotToken:    "xoxb-secret",
		AppToken:    "xapp-secret",
		WorkspaceID: "T123",
	}); err != nil {
		t.Fatalf("SetSlackAuth() error = %v", err)
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	var out bytes.Buffer
	done := make(chan struct{})
	handler := &runtimeCommandHandler{
		configStore: cfgStore,
		out:         &out,
		starter: recordingRuntimeStarterFunc(func(ctx context.Context, status runtimemodel.Status) error {
			if !status.Running || !status.Healthy {
				t.Fatalf("status = %#v, want running and healthy", status)
			}
			close(done)
			<-ctx.Done()
			return ctx.Err()
		}),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- handler.Start(ctx, []string{"--debug"})
	}()

	<-done
	cancel()

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context canceled", err)
	}

	output := out.String()
	for _, fragment := range []string{
		"runtime.start: validating startup state",
		"runtime.start: startup loaded config=",
		"runtime.start: entering foreground runtime loop",
		"runtime.start: shutdown signal received; exiting cleanly",
	} {
		if !strings.Contains(output, fragment) {
			t.Fatalf("debug output missing %q: %s", fragment, output)
		}
	}
}

// Test: runtime startup recovers stale queued/running executions from a previous service instance before entering the foreground loop.
// Validates: startup recovery clears impossible non-terminal executions and releases stale thread locks after a runtime restart
func TestRuntimeStartRecoversStaleNonTerminalExecutions(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	workspacePath := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspacePath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	cfgStore := config.NewFileStore("")
	if _, err := cfgStore.SetBaseWorkspacePath(ctx, workspacePath); err != nil {
		t.Fatalf("SetBaseWorkspacePath() error = %v", err)
	}
	if _, err := cfgStore.SetSlackAuth(ctx, config.SlackAuth{
		BotToken:    "xoxb-secret",
		AppToken:    "xapp-secret",
		WorkspaceID: "T123",
	}); err != nil {
		t.Fatalf("SetSlackAuth() error = %v", err)
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	project := registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(workspacePath, "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C12345678",
	}
	if err := os.MkdirAll(project.LocalPath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := store.Projects().Upsert(ctx, project); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	threadTS := "1713686400.000500"
	if err := store.Runtime().SaveThreadState(ctx, runtimemodel.ThreadState{
		ThreadTS:      threadTS,
		ChannelID:     project.SlackChannelID,
		ProjectName:   project.Name,
		SessionName:   "slack-1713686400.000500",
		LastStatus:    "queued",
		LastRequestID: "Ev-stale-queued",
		UpdatedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveThreadState() error = %v", err)
	}
	lockTime := time.Now().UTC().Add(-10 * time.Minute)
	lockExpires := lockTime.Add(5 * time.Minute)
	if err := store.Runtime().SaveThreadLock(ctx, runtimemodel.ThreadLock{
		ThreadTS:       threadTS,
		LockOwner:      "runtime-start",
		LockedAt:       lockTime,
		LeaseExpiresAt: &lockExpires,
		UpdatedAt:      lockTime,
	}); err != nil {
		t.Fatalf("SaveThreadLock() error = %v", err)
	}

	if err := store.Runtime().CreateExecution(ctx, runtimemodel.ExecutionRequest{
		ExecutionID: "exec-stale-running",
		SourceType:  "message",
		DeliveryID:  "Ev-stale-running",
		ChannelID:   project.SlackChannelID,
		ProjectName: project.Name,
		SessionKey:  "slack-1713686400.000500",
		ThreadTS:    threadTS,
		CommandText: "stale running execution",
		CreatedAt:   lockTime.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("CreateExecution(running) error = %v", err)
	}
	startedAt := lockTime.Add(-30 * time.Second)
	if err := store.Runtime().UpdateExecutionState(ctx, runtimemodel.ExecutionState{
		ExecutionID: "exec-stale-running",
		Status:      runtimemodel.ExecutionStateRunning,
		StartedAt:   &startedAt,
		UpdatedAt:   startedAt,
	}); err != nil {
		t.Fatalf("UpdateExecutionState(running) error = %v", err)
	}

	if err := store.Runtime().CreateExecution(ctx, runtimemodel.ExecutionRequest{
		ExecutionID: "exec-stale-queued",
		SourceType:  "message",
		DeliveryID:  "Ev-stale-queued",
		ChannelID:   project.SlackChannelID,
		ProjectName: project.Name,
		SessionKey:  "slack-1713686400.000500",
		ThreadTS:    threadTS,
		CommandText: "stale queued execution",
		CreatedAt:   lockTime.Add(-45 * time.Second),
	}); err != nil {
		t.Fatalf("CreateExecution(queued) error = %v", err)
	}

	handler := &runtimeCommandHandler{
		configStore: cfgStore,
		out:         io.Discard,
	}

	snapshot, err := handler.loadStartupSnapshot(ctx)
	if err != nil {
		t.Fatalf("loadStartupSnapshot() error = %v", err)
	}
	if !snapshot.status.Running || !snapshot.status.Healthy {
		t.Fatalf("snapshot status = %#v, want running and healthy", snapshot.status)
	}

	runningExecution, err := store.Runtime().LoadExecution(ctx, "exec-stale-running")
	if err != nil {
		t.Fatalf("LoadExecution(exec-stale-running) error = %v", err)
	}
	if runningExecution.Status != runtimemodel.ExecutionStateFailed {
		t.Fatalf("running execution status = %q, want failed", runningExecution.Status)
	}
	if runningExecution.CompletedAt == nil {
		t.Fatalf("running execution completed_at = nil, want terminal timestamp")
	}
	if runningExecution.DiagnosticContext != "startup recovery: execution interrupted by runtime restart" {
		t.Fatalf("running execution diagnostic context = %q, want startup recovery detail", runningExecution.DiagnosticContext)
	}

	queuedExecution, err := store.Runtime().LoadExecution(ctx, "exec-stale-queued")
	if err != nil {
		t.Fatalf("LoadExecution(exec-stale-queued) error = %v", err)
	}
	if queuedExecution.Status != runtimemodel.ExecutionStateFailed {
		t.Fatalf("queued execution status = %q, want failed", queuedExecution.Status)
	}
	if queuedExecution.CompletedAt == nil {
		t.Fatalf("queued execution completed_at = nil, want terminal timestamp")
	}
	if queuedExecution.DiagnosticContext != "startup recovery: previous runtime stopped before execution reached a terminal state" {
		t.Fatalf("queued execution diagnostic context = %q, want startup recovery detail", queuedExecution.DiagnosticContext)
	}

	state, err := store.Runtime().LoadThreadState(ctx, threadTS)
	if err != nil {
		t.Fatalf("LoadThreadState() error = %v", err)
	}
	if state.LastStatus != "failed" {
		t.Fatalf("LoadThreadState() last status = %q, want failed", state.LastStatus)
	}
	if _, err := store.Runtime().LoadThreadLock(ctx, threadTS); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("LoadThreadLock() error = %v, want not found", err)
	}
}

type recordingRuntimeStarterFunc func(context.Context, runtimemodel.Status) error

func (fn recordingRuntimeStarterFunc) Start(ctx context.Context, status runtimemodel.Status) error {
	return fn(ctx, status)
}

func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("condition not met within %s", timeout)
}

// Test: root app_mention invocations are treated as command execution, dispatched through ACPX with the parsed command text, and rendered back into the root thread.
// Validates: AC-1815 (REQ-1181 - root mentions start a new thread execution), AC-1815 (REQ-1183 - mention command text is parsed from the payload)
func TestForegroundRuntimeStarterProcessesRootMentionCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	project := registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(home, "workspace", "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C123",
	}
	if err := os.MkdirAll(project.LocalPath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := store.Projects().Upsert(ctx, project); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	client := &recordingSlackClient{}
	adapter := &fakePromptAdapter{
		results: []acpxadapter.SessionResult{
			{
				SessionName: acpxadapter.SessionName("1713686400.000100"),
				Output: `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"019db13d-f733-7ce0-8186-5aced7cdb2a7","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello from acpx"}}}}
{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`,
			},
		},
	}

	var debug bytes.Buffer
	starter := &foregroundRuntimeStarter{
		source: &fakeSlackInvocationSource{
			invocations: []slack.InboundInvocation{
				{
					SourceType:  slack.InboundSourceMention,
					DeliveryID:  "Ev123",
					ChannelID:   project.SlackChannelID,
					ThreadTS:    "1713686400.000100",
					UserID:      "U123",
					CommandText: "<@Ubot> ask status",
				},
			},
		},
		client:      client,
		renderer:    runtimemodel.SlackThreadRenderer{Client: client},
		adapter:     adapter,
		projectRepo: store.Projects(),
		runtimeRepo: store.Runtime(),
	}
	starter.debugf = func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		_, _ = debug.WriteString(line + "\n")
		if strings.Contains(line, "mention processed delivery=Ev123") {
			cancel()
		}
	}

	err = starter.Start(ctx, runtimemodel.Status{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context canceled", err)
	}

	if got, want := len(adapter.calls), 1; got != want {
		t.Fatalf("adapter call count = %d, want %d", got, want)
	}
	if adapter.calls[0].Prompt != "status" || adapter.calls[0].ProjectPath != project.LocalPath {
		t.Fatalf("adapter call = %#v, want prompt and local path propagated", adapter.calls[0])
	}
	if adapter.calls[0].ThreadTS != "1713686400.000100" {
		t.Fatalf("adapter call thread ts = %q, want root thread anchor", adapter.calls[0].ThreadTS)
	}

	if got, want := len(client.messages), 1; got != want {
		t.Fatalf("slack message count = %d, want %d", got, want)
	}
	if client.messages[0].Text != "hello from acpx" {
		t.Fatalf("slack reply text = %q, want %q", client.messages[0].Text, "hello from acpx")
	}

	output := debug.String()
	for _, fragment := range []string{
		"runtime.loop: connected to slack socket mode",
		"runtime.loop: received slack invocation delivery=Ev123 source=mention",
		"runtime.loop: dispatching mention delivery=Ev123 project=alpha session=slack-1713686400.000100",
		"runtime.loop: acpx events session=slack-1713686400.000100 payload=assistant_message_chunk:hello from acpx | assistant_message_final:hello from acpx | session_done",
		"runtime.loop: rendered slack reply thread=1713686400.000100 session=slack-1713686400.000100",
		"runtime.loop: mention processed delivery=Ev123 session=slack-1713686400.000100",
	} {
		if !strings.Contains(output, fragment) {
			t.Fatalf("debug output missing %q: %s", fragment, output)
		}
	}
}

func TestForegroundRuntimeStarterCollectPromptEventsPublishesLiveProgressBeforeCompletion(t *testing.T) {
	t.Parallel()

	client := &recordingSlackClient{}
	stream := &controlledPromptStream{
		sessionName: "slack-1713686400.000100",
		events:      make(chan acpxadapter.Event, 8),
		waitCh:      make(chan error, 1),
	}
	adapter := &fakePromptAdapter{
		startPrompt: func(context.Context, acpxadapter.SessionRequest) (acpxadapter.PromptStream, error) {
			return stream, nil
		},
	}
	starter := &foregroundRuntimeStarter{
		client:   client,
		renderer: runtimemodel.SlackThreadRenderer{Client: client},
		adapter:  adapter,
	}
	var debug bytes.Buffer
	starter.debugf = func(format string, args ...any) {
		_, _ = debug.WriteString(fmt.Sprintf(format, args...) + "\n")
	}

	prepared := runtimemodel.PreparedSlackEvent{
		SourceType: slack.InboundSourceMention,
		Project: registry.Project{
			Name:           "alpha",
			LocalPath:      "/workspace/alpha",
			SlackChannelID: "C123",
		},
		ThreadTS:    "1713686400.000100",
		SessionName: "slack-1713686400.000100",
	}

	type result struct {
		sessionName string
		events      []runtimemodel.ACPXTurnEvent
		err         error
	}
	resultCh := make(chan result, 1)
	request := runtimemodel.ExecutionRequest{
		SourceType:  slack.InboundSourceMention,
		DeliveryID:  "Ev-progress",
		ChannelID:   "C123",
		CommandText: "status",
		Project: registry.Project{
			Name:           "alpha",
			LocalPath:      "/workspace/alpha",
			SlackChannelID: "C123",
		},
		ThreadTS:    prepared.ThreadTS,
		SessionName: prepared.SessionName,
	}
	go func() {
		got, err := starter.collectPromptEvents(context.Background(), prepared, request, "status")
		resultCh <- result{sessionName: got.sessionName, events: got.events, err: err}
	}()

	stream.events <- acpxadapter.Event{Kind: acpxadapter.EventSessionStarted, Text: "slack-1713686400.000100"}
	stream.events <- acpxadapter.Event{Kind: acpxadapter.EventAssistantThinking, Text: "analyzing"}
	stream.events <- acpxadapter.Event{Kind: acpxadapter.EventToolStarted, ToolName: "grep", Text: "searching"}
	stream.events <- acpxadapter.Event{Kind: acpxadapter.EventToolFinished, ToolName: "grep", ToolStatus: "completed"}

	waitForCondition(t, 2*time.Second, func() bool {
		messages := client.snapshotMessages()
		return len(messages) == 1 && strings.Contains(messages[0].Text, "Progress:")
	})

	messages := client.snapshotMessages()
	if messages[0].Text != "Progress:\n- Session started: slack-1713686400.000100\n- Thinking: analyzing\n- Tool started: grep - searching\n- Tool finished: grep" {
		t.Fatalf("live progress message = %q", messages[0].Text)
	}

	stream.events <- acpxadapter.Event{Kind: acpxadapter.EventAssistantMessageFinal, Text: "final answer"}
	stream.events <- acpxadapter.Event{Kind: acpxadapter.EventSessionDone}
	close(stream.events)
	stream.waitCh <- nil

	got := <-resultCh
	if got.err != nil {
		t.Fatalf("collectPromptEvents() error = %v", got.err)
	}
	if got.sessionName != "slack-1713686400.000100" {
		t.Fatalf("collectPromptEvents() session = %q, want slack session", got.sessionName)
	}
	if gotCount := len(got.events); gotCount != 6 {
		t.Fatalf("collectPromptEvents() event count = %d, want 6", gotCount)
	}

	messages = client.snapshotMessages()
	if gotCount := len(messages); gotCount != 2 {
		t.Fatalf("slack message count = %d, want 2", gotCount)
	}
	if messages[1].Text != "final answer" {
		t.Fatalf("final slack message = %q, want final answer", messages[1].Text)
	}
	if !strings.Contains(debug.String(), "runtime.loop: lifecycle event=progress_flushed source=mention delivery_id=Ev-progress channel_id=C123 project=alpha status=rendering session=slack-1713686400.000100 count=4 reason=count") {
		t.Fatalf("debug output missing progress flush event: %s", debug.String())
	}
}

// Test: assistant-only streamed chunks are published to Slack before terminal completion without waiting for a tool event boundary.
// Validates: AC-1978 (REQ-1425 - assistant progress is published before ACPX reaches a terminal outcome), AC-1980 (REQ-1427 - terminal success does not duplicate an identical live answer)
func TestForegroundRuntimeStarterCollectPromptEventsPublishesAssistantOnlyProgressBeforeCompletion(t *testing.T) {
	t.Parallel()

	client := &recordingSlackClient{}
	stream := &controlledPromptStream{
		sessionName: "slack-1713686400.000100",
		events:      make(chan acpxadapter.Event, 8),
		waitCh:      make(chan error, 1),
	}
	adapter := &fakePromptAdapter{
		startPrompt: func(context.Context, acpxadapter.SessionRequest) (acpxadapter.PromptStream, error) {
			return stream, nil
		},
	}
	starter := &foregroundRuntimeStarter{
		client:   client,
		renderer: runtimemodel.SlackThreadRenderer{Client: client},
		adapter:  adapter,
	}

	prepared := runtimemodel.PreparedSlackEvent{
		SourceType: slack.InboundSourceMention,
		Project: registry.Project{
			Name:           "alpha",
			LocalPath:      "/workspace/alpha",
			SlackChannelID: "C123",
		},
		ThreadTS:    "1713686400.000100",
		SessionName: "slack-1713686400.000100",
	}
	request := runtimemodel.ExecutionRequest{
		SourceType:  slack.InboundSourceMention,
		DeliveryID:  "Ev-assistant-progress",
		ChannelID:   "C123",
		CommandText: "status",
		Project: registry.Project{
			Name:           "alpha",
			LocalPath:      "/workspace/alpha",
			SlackChannelID: "C123",
		},
		ThreadTS:    prepared.ThreadTS,
		SessionName: prepared.SessionName,
	}

	resultCh := make(chan error, 1)
	go func() {
		_, err := starter.collectPromptEvents(context.Background(), prepared, request, "status")
		resultCh <- err
	}()

	stream.events <- acpxadapter.Event{Kind: acpxadapter.EventAssistantMessageChunk, Text: "hello"}
	stream.events <- acpxadapter.Event{Kind: acpxadapter.EventAssistantMessageChunk, Text: " world"}
	stream.events <- acpxadapter.Event{Kind: acpxadapter.EventAssistantMessageChunk, Text: " from"}
	stream.events <- acpxadapter.Event{Kind: acpxadapter.EventAssistantMessageChunk, Text: " acpx"}

	waitForCondition(t, 2*time.Second, func() bool {
		messages := client.snapshotMessages()
		return len(messages) == 1 && messages[0].Text == "hello world from acpx"
	})

	stream.events <- acpxadapter.Event{Kind: acpxadapter.EventAssistantMessageFinal, Text: "hello world from acpx"}
	stream.events <- acpxadapter.Event{Kind: acpxadapter.EventSessionDone}
	close(stream.events)
	stream.waitCh <- nil

	if err := <-resultCh; err != nil {
		t.Fatalf("collectPromptEvents() error = %v", err)
	}

	messages := client.snapshotMessages()
	if got, want := len(messages), 1; got != want {
		t.Fatalf("slack message count = %d, want %d", got, want)
	}
	if messages[0].Text != "hello world from acpx" {
		t.Fatalf("final slack message = %q, want final answer", messages[0].Text)
	}
}

// Test: streamed mention progress that later fails still produces one terminal Slack error and persists failed lifecycle state.
// Validates: AC-1978 (REQ-1425 - progress is published before ACPX reaches a terminal outcome), AC-1981 (REQ-1429 - execution state keeps a unique failed lifecycle record), AC-1981 (REQ-1430 - lifecycle timestamps and publisher checkpoints persist through failure)
func TestForegroundRuntimeStarterReportsRenderedStreamFailureOnlyOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	project := registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(home, "workspace", "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C123",
	}
	if err := os.MkdirAll(project.LocalPath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := store.Projects().Upsert(ctx, project); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	client := &recordingSlackClient{}
	adapter := &fakePromptAdapter{
		startPrompt: func(_ context.Context, req acpxadapter.SessionRequest) (acpxadapter.PromptStream, error) {
			return &fakePromptStream{
				sessionName: acpxadapter.SessionName(req.ThreadTS),
				events: []acpxadapter.Event{
					{Kind: acpxadapter.EventAssistantMessageChunk, Text: "working"},
				},
				waitErr: errors.New("acpx async failure"),
			}, nil
		},
	}

	starter := &foregroundRuntimeStarter{
		source: &fakeSlackInvocationSource{
			invocations: []slack.InboundInvocation{
				{
					SourceType:  slack.InboundSourceMention,
					DeliveryID:  "Ev-stream-fail",
					ChannelID:   project.SlackChannelID,
					ThreadTS:    "1713686400.000100",
					UserID:      "U123",
					CommandText: "<@Ubot> ask status",
				},
			},
		},
		client:      client,
		renderer:    runtimemodel.SlackThreadRenderer{Client: client},
		adapter:     adapter,
		projectRepo: store.Projects(),
		runtimeRepo: store.Runtime(),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- starter.Start(ctx, runtimemodel.Status{})
	}()

	waitForCondition(t, 2*time.Second, func() bool {
		messages := client.snapshotMessages()
		return len(messages) == 2 && messages[1].Text == "Session error: acpx async failure"
	})
	cancel()

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context canceled", err)
	}

	messages := client.snapshotMessages()
	if got, want := len(messages), 2; got != want {
		t.Fatalf("slack message count = %d, want %d", got, want)
	}
	if messages[0].Text != "working" {
		t.Fatalf("progress message = %q", messages[0].Text)
	}
	if messages[1].Text != "Session error: acpx async failure" {
		t.Fatalf("terminal message = %q, want one rendered stream failure", messages[1].Text)
	}

	executionState, err := store.Runtime().LoadExecutionStateByDelivery(context.Background(), "mention", "Ev-stream-fail")
	if err != nil {
		t.Fatalf("LoadExecutionStateByDelivery() error = %v", err)
	}
	if executionState.Status != runtimemodel.ExecutionStatusFailed || executionState.CompletedAt == nil {
		t.Fatalf("LoadExecutionStateByDelivery() = %#v, want failed execution", executionState)
	}
	if executionState.ThreadTS != "1713686400.000100" || executionState.SessionName != "slack-1713686400.000100" {
		t.Fatalf("LoadExecutionStateByDelivery() thread/session = (%q, %q), want mention thread/session", executionState.ThreadTS, executionState.SessionName)
	}
	if executionState.QueuedAt.IsZero() || executionState.StartedAt == nil || executionState.RenderingStartedAt == nil {
		t.Fatalf("LoadExecutionStateByDelivery() timestamps = %#v, want queued/running/rendering markers before failure", executionState)
	}
	if executionState.LastError != "acpx async failure" {
		t.Fatalf("LoadExecutionStateByDelivery() last error = %q, want acpx async failure", executionState.LastError)
	}
	if executionState.PublisherCheckpointKind != string(runtimemodel.ACPXEventAssistantMessageChunk) || executionState.PublisherCheckpointSummary != "working" {
		t.Fatalf("LoadExecutionStateByDelivery() checkpoint = (%q, %q), want assistant chunk/working", executionState.PublisherCheckpointKind, executionState.PublisherCheckpointSummary)
	}

	threadState, err := store.Runtime().LoadThreadState(context.Background(), "1713686400.000100")
	if err != nil {
		t.Fatalf("LoadThreadState() error = %v", err)
	}
	if threadState.LastStatus != "failed" || threadState.LastRequestID != "Ev-stream-fail" {
		t.Fatalf("LoadThreadState() = %#v, want failed mention thread state", threadState)
	}

	dedupe, err := store.Runtime().LoadEventDedupe(context.Background(), "mention", "Ev-stream-fail")
	if err != nil {
		t.Fatalf("LoadEventDedupe() error = %v", err)
	}
	if dedupe.Status != "failed" || dedupe.ProcessedAt == nil {
		t.Fatalf("LoadEventDedupe() = %#v, want failed mention delivery", dedupe)
	}
}

// Test: accepted mention invocations are enqueued so the intake loop can continue processing a later mention without waiting for the first ACPX prompt to complete.
// Validates: AC-1975 (REQ-1417 - mention ingestion does not wait for ACPX completion), AC-1975 (REQ-1418 - accepted mentions are enqueued for async execution), AC-1975 (REQ-1419 - intake returns immediately after normalization, resolution, and dedupe claim)
func TestForegroundRuntimeStarterEnqueuesMentionExecutionWithoutBlockingNextInvocation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	project := registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(home, "workspace", "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C123",
	}
	if err := os.MkdirAll(project.LocalPath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := store.Projects().Upsert(ctx, project); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := make(chan struct{})

	client := &recordingSlackClient{}
	adapter := &fakePromptAdapter{}
	var adapterCalls int
	var adapterCallsMu sync.Mutex
	adapter.sendPrompt = func(_ context.Context, req acpxadapter.SessionRequest) (acpxadapter.SessionResult, error) {
		adapterCallsMu.Lock()
		adapterCalls++
		callIndex := adapterCalls
		adapterCallsMu.Unlock()

		switch callIndex {
		case 1:
			close(firstStarted)
			<-releaseFirst
			return acpxadapter.SessionResult{
				SessionName: acpxadapter.SessionName(req.ThreadTS),
				Output:      `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"019db13d-f733-7ce0-8186-5aced7cdb2a7","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"first async result"}}}}`,
			}, nil
		case 2:
			close(secondStarted)
			return acpxadapter.SessionResult{
				SessionName: acpxadapter.SessionName(req.ThreadTS),
				Output:      `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"019db13d-f733-7ce0-8186-5aced7cdb2a8","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"second async result"}}}}`,
			}, nil
		default:
			return acpxadapter.SessionResult{}, fmt.Errorf("unexpected SendPrompt call %d", callIndex)
		}
	}

	starter := &foregroundRuntimeStarter{
		source: &fakeSlackInvocationSource{
			invocations: []slack.InboundInvocation{
				{
					SourceType:  slack.InboundSourceMention,
					DeliveryID:  "Ev130",
					ChannelID:   project.SlackChannelID,
					ThreadTS:    "1713686400.000100",
					UserID:      "U123",
					CommandText: "<@Ubot> ask status",
				},
				{
					SourceType:  slack.InboundSourceMention,
					DeliveryID:  "Ev131",
					ChannelID:   project.SlackChannelID,
					ThreadTS:    "1713686401.000200",
					UserID:      "U123",
					CommandText: "<@Ubot> ask status",
				},
			},
		},
		client:      client,
		renderer:    runtimemodel.SlackThreadRenderer{Client: client},
		adapter:     adapter,
		projectRepo: store.Projects(),
		runtimeRepo: store.Runtime(),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- starter.Start(ctx, runtimemodel.Status{})
	}()

	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first SendPrompt() did not start")
	}

	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("second SendPrompt() did not start before the first prompt was released")
	}

	close(releaseFirst)

	waitForCondition(t, 2*time.Second, func() bool {
		messages := client.snapshotMessages()
		gotTexts := map[string]bool{}
		for _, message := range messages {
			gotTexts[message.Text] = true
		}
		return gotTexts["first async result"] && gotTexts["second async result"]
	})
	cancel()

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context canceled", err)
	}

	calls := adapter.snapshotCalls()
	if got, want := len(calls), 2; got != want {
		t.Fatalf("adapter call count = %d, want %d", got, want)
	}
	gotThreads := map[string]bool{}
	for _, call := range calls {
		gotThreads[call.ThreadTS] = true
	}
	for _, threadTS := range []string{"1713686400.000100", "1713686401.000200"} {
		if !gotThreads[threadTS] {
			t.Fatalf("adapter calls = %#v, want both mention threads to execute independently", calls)
		}
	}

	firstDedupe, err := store.Runtime().LoadEventDedupe(context.Background(), "mention", "Ev130")
	if err != nil {
		t.Fatalf("LoadEventDedupe(first) error = %v", err)
	}
	if firstDedupe.Status != "processed" {
		t.Fatalf("first dedupe = %#v, want processed", firstDedupe)
	}

	secondDedupe, err := store.Runtime().LoadEventDedupe(context.Background(), "mention", "Ev131")
	if err != nil {
		t.Fatalf("LoadEventDedupe(second) error = %v", err)
	}
	if secondDedupe.Status != "processed" {
		t.Fatalf("second dedupe = %#v, want processed", secondDedupe)
	}
}

// Test: threaded app_mention invocations keep using the existing Slack thread anchor while dispatching only the parsed command text to ACPX.
// Validates: AC-1816 (REQ-1182 - threaded mentions continue the existing thread), AC-1816 (REQ-1183 - mention command text is parsed from the payload)
func TestForegroundRuntimeStarterProcessesThreadedMentionCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	project := registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(home, "workspace", "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C123",
	}
	if err := os.MkdirAll(project.LocalPath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := store.Projects().Upsert(ctx, project); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	client := &recordingSlackClient{}
	adapter := &fakePromptAdapter{
		results: []acpxadapter.SessionResult{
			{
				SessionName: acpxadapter.SessionName("1713686400.000100"),
				Output: `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"019db13d-f733-7ce0-8186-5aced7cdb2a7","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"thread reply from acpx"}}}}
{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`,
			},
		},
	}

	starter := &foregroundRuntimeStarter{
		source: &fakeSlackInvocationSource{
			invocations: []slack.InboundInvocation{
				{
					SourceType:  slack.InboundSourceMention,
					DeliveryID:  "Ev124",
					ChannelID:   project.SlackChannelID,
					ThreadTS:    "1713686400.000100",
					UserID:      "U123",
					CommandText: "<@Ubot> ask summarize current project state",
				},
			},
		},
		client:      client,
		renderer:    runtimemodel.SlackThreadRenderer{Client: client},
		adapter:     adapter,
		projectRepo: store.Projects(),
		runtimeRepo: store.Runtime(),
	}
	starter.debugf = func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		if strings.Contains(line, "mention processed delivery=Ev124") {
			cancel()
		}
	}

	err = starter.Start(ctx, runtimemodel.Status{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context canceled", err)
	}

	if got, want := len(adapter.calls), 1; got != want {
		t.Fatalf("adapter call count = %d, want %d", got, want)
	}
	if adapter.calls[0].Prompt != "summarize current project state" {
		t.Fatalf("adapter call prompt = %q, want ask payload without command prefix", adapter.calls[0].Prompt)
	}
	if adapter.calls[0].ThreadTS != "1713686400.000100" {
		t.Fatalf("adapter call thread ts = %q, want existing thread anchor", adapter.calls[0].ThreadTS)
	}

	if got, want := len(client.messages), 1; got != want {
		t.Fatalf("slack message count = %d, want %d", got, want)
	}
	if client.messages[0].ThreadTS != "1713686400.000100" {
		t.Fatalf("slack reply thread ts = %q, want existing thread anchor", client.messages[0].ThreadTS)
	}
}

// Test: accepted mention invocations are enqueued so a blocked execution in one thread does not stop intake or another thread in the same project.
// Validates: AC-1975 (REQ-1417 - mention ingestion does not wait for ACPX completion), AC-1975 (REQ-1418 - accepted mentions are enqueued), AC-1975 (REQ-1419 - intake continues immediately without project-wide serialization)
func TestForegroundRuntimeStarterEnqueuesMentionsWithoutBlockingLaterThreads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	project := registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(home, "workspace", "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C123",
	}
	if err := os.MkdirAll(project.LocalPath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := store.Projects().Upsert(ctx, project); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	client := &recordingSlackClient{}
	firstStarted := make(chan struct{}, 1)
	secondStarted := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	adapter := &fakePromptAdapter{
		sendPrompt: func(_ context.Context, req acpxadapter.SessionRequest) (acpxadapter.SessionResult, error) {
			switch req.ThreadTS {
			case "1713686400.000100":
				firstStarted <- struct{}{}
				<-releaseFirst
				return acpxadapter.SessionResult{
					SessionName: acpxadapter.SessionName(req.ThreadTS),
					Output: `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"019db13d-f733-7ce0-8186-5aced7cdb2a7","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"first async mention result"}}}}
{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`,
				}, nil
			case "1713686400.000200":
				secondStarted <- struct{}{}
				<-releaseSecond
				return acpxadapter.SessionResult{
					SessionName: acpxadapter.SessionName(req.ThreadTS),
					Output: `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"019db13d-f733-7ce0-8186-5aced7cdb2a7","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"second async mention result"}}}}
{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`,
				}, nil
			default:
				return acpxadapter.SessionResult{}, fmt.Errorf("unexpected thread %s", req.ThreadTS)
			}
		},
	}

	var debug bytes.Buffer
	var debugMu sync.Mutex
	starter := &foregroundRuntimeStarter{
		source: &fakeSlackInvocationSource{
			invocations: []slack.InboundInvocation{
				{
					SourceType:  slack.InboundSourceMention,
					DeliveryID:  "Ev126",
					ChannelID:   project.SlackChannelID,
					ThreadTS:    "1713686400.000100",
					UserID:      "U123",
					CommandText: "<@Ubot> ask status",
				},
				{
					SourceType:  slack.InboundSourceMention,
					DeliveryID:  "Ev127",
					ChannelID:   project.SlackChannelID,
					ThreadTS:    "1713686400.000200",
					UserID:      "U123",
					CommandText: "<@Ubot> ask summarize current project state",
				},
			},
		},
		client:      client,
		renderer:    runtimemodel.SlackThreadRenderer{Client: client},
		adapter:     adapter,
		projectRepo: store.Projects(),
		runtimeRepo: store.Runtime(),
	}
	starter.debugf = func(format string, args ...any) {
		debugMu.Lock()
		defer debugMu.Unlock()
		_, _ = debug.WriteString(fmt.Sprintf(format, args...) + "\n")
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- starter.Start(ctx, runtimemodel.Status{})
	}()

	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first mention execution did not start")
	}

	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("second mention execution did not start while the first thread was still blocked")
	}

	close(releaseSecond)
	close(releaseFirst)

	waitForCondition(t, 2*time.Second, func() bool {
		return len(client.snapshotMessages()) == 2
	})
	cancel()

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context canceled", err)
	}

	calls := adapter.snapshotCalls()
	if got, want := len(calls), 2; got != want {
		t.Fatalf("adapter call count = %d, want %d", got, want)
	}

	gotThreads := map[string]bool{}
	for _, call := range calls {
		gotThreads[call.ThreadTS] = true
	}
	for _, threadTS := range []string{"1713686400.000100", "1713686400.000200"} {
		if !gotThreads[threadTS] {
			t.Fatalf("adapter calls missing thread %s: %#v", threadTS, calls)
		}
	}

	messages := client.snapshotMessages()
	if got, want := len(messages), 2; got < want {
		t.Fatalf("slack message count = %d, want at least %d", got, want)
	}
	gotTexts := map[string]bool{}
	gotMessageThreads := map[string]bool{}
	for _, message := range messages {
		gotTexts[message.Text] = true
		gotMessageThreads[message.ThreadTS] = true
	}
	for _, text := range []string{"first async mention result", "second async mention result"} {
		if !gotTexts[text] {
			t.Fatalf("slack messages missing %q: %#v", text, messages)
		}
	}
	for _, threadTS := range []string{"1713686400.000100", "1713686400.000200"} {
		if !gotMessageThreads[threadTS] {
			t.Fatalf("slack messages missing thread %s: %#v", threadTS, messages)
		}
	}

	for _, deliveryID := range []string{"Ev126", "Ev127"} {
		dedupe, err := store.Runtime().LoadEventDedupe(context.Background(), "mention", deliveryID)
		if err != nil {
			t.Fatalf("LoadEventDedupe(%s) error = %v", deliveryID, err)
		}
		if dedupe.Status != "processed" || dedupe.ProcessedAt == nil {
			t.Fatalf("LoadEventDedupe(%s) = %#v, want processed mention delivery", deliveryID, dedupe)
		}
	}

	debugMu.Lock()
	output := debug.String()
	debugMu.Unlock()
	for _, fragment := range []string{
		"runtime.loop: mention enqueued delivery=Ev126 session=slack-1713686400.000100",
		"runtime.loop: mention enqueued delivery=Ev127 session=slack-1713686400.000200",
		"runtime.loop: mention processed delivery=Ev126 session=slack-1713686400.000100",
		"runtime.loop: mention processed delivery=Ev127 session=slack-1713686400.000200",
	} {
		if !strings.Contains(output, fragment) {
			t.Fatalf("debug output missing %q: %s", fragment, output)
		}
	}
}

// Test: accepted mention invocations for one Slack session stay serialized while a later invocation for another thread in the same project can still start.
// Validates: AC-1977 (REQ-1421 - the same session key is serialized until the earlier execution reaches terminal state), AC-1976 (REQ-1420 - a different session key can run concurrently), AC-1977 (REQ-1423 - runtime does not serialize the whole project)
func TestForegroundRuntimeStarterSerializesSameMentionSessionWithoutBlockingOtherThread(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	project := registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(home, "workspace", "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C123",
	}
	if err := os.MkdirAll(project.LocalPath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := store.Projects().Upsert(ctx, project); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	client := &recordingSlackClient{}
	firstSharedStarted := make(chan struct{}, 1)
	secondSharedStarted := make(chan struct{}, 1)
	otherStarted := make(chan struct{}, 1)
	releaseFirstShared := make(chan struct{})
	releaseSecondShared := make(chan struct{})
	releaseOther := make(chan struct{})

	var sharedCalls int
	var sharedCallsMu sync.Mutex
	adapter := &fakePromptAdapter{
		sendPrompt: func(_ context.Context, req acpxadapter.SessionRequest) (acpxadapter.SessionResult, error) {
			switch req.ThreadTS {
			case "1713686400.000100":
				sharedCallsMu.Lock()
				sharedCalls++
				callIndex := sharedCalls
				sharedCallsMu.Unlock()

				switch callIndex {
				case 1:
					firstSharedStarted <- struct{}{}
					<-releaseFirstShared
					return acpxadapter.SessionResult{
						SessionName: acpxadapter.SessionName(req.ThreadTS),
						Output:      `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"019db13d-f733-7ce0-8186-5aced7cdb2a7","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"first shared mention result"}}}}` + "\n" + `{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`,
					}, nil
				case 2:
					secondSharedStarted <- struct{}{}
					<-releaseSecondShared
					return acpxadapter.SessionResult{
						SessionName: acpxadapter.SessionName(req.ThreadTS),
						Output:      `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"019db13d-f733-7ce0-8186-5aced7cdb2a7","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"second shared mention result"}}}}` + "\n" + `{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`,
					}, nil
				default:
					return acpxadapter.SessionResult{}, fmt.Errorf("unexpected shared-session SendPrompt call %d", callIndex)
				}
			case "1713686400.000200":
				otherStarted <- struct{}{}
				<-releaseOther
				return acpxadapter.SessionResult{
					SessionName: acpxadapter.SessionName(req.ThreadTS),
					Output:      `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"019db13d-f733-7ce0-8186-5aced7cdb2a8","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"other thread mention result"}}}}` + "\n" + `{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`,
				}, nil
			default:
				return acpxadapter.SessionResult{}, fmt.Errorf("unexpected thread %s", req.ThreadTS)
			}
		},
	}

	starter := &foregroundRuntimeStarter{
		source: &fakeSlackInvocationSource{
			invocations: []slack.InboundInvocation{
				{
					SourceType:  slack.InboundSourceMention,
					DeliveryID:  "Ev140",
					ChannelID:   project.SlackChannelID,
					ThreadTS:    "1713686400.000100",
					UserID:      "U123",
					CommandText: "<@Ubot> ask status",
				},
				{
					SourceType:  slack.InboundSourceMention,
					DeliveryID:  "Ev141",
					ChannelID:   project.SlackChannelID,
					ThreadTS:    "1713686400.000100",
					UserID:      "U123",
					CommandText: "<@Ubot> ask summarize shared thread",
				},
				{
					SourceType:  slack.InboundSourceMention,
					DeliveryID:  "Ev142",
					ChannelID:   project.SlackChannelID,
					ThreadTS:    "1713686400.000200",
					UserID:      "U123",
					CommandText: "<@Ubot> ask summarize parallel thread",
				},
			},
		},
		client:      client,
		renderer:    runtimemodel.SlackThreadRenderer{Client: client},
		adapter:     adapter,
		projectRepo: store.Projects(),
		runtimeRepo: store.Runtime(),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- starter.Start(ctx, runtimemodel.Status{})
	}()

	select {
	case <-firstSharedStarted:
	case <-time.After(time.Second):
		t.Fatal("first shared-session execution did not start")
	}

	select {
	case <-otherStarted:
	case <-time.After(time.Second):
		t.Fatal("other-thread execution did not start while the first shared session was still running")
	}

	select {
	case <-secondSharedStarted:
		t.Fatal("second shared-session execution started before the first shared execution reached terminal state")
	case <-time.After(150 * time.Millisecond):
	}

	close(releaseFirstShared)

	select {
	case <-secondSharedStarted:
	case <-time.After(time.Second):
		t.Fatal("second shared-session execution did not start after the first shared execution completed")
	}

	close(releaseOther)
	close(releaseSecondShared)

	waitForCondition(t, 2*time.Second, func() bool {
		return len(client.snapshotMessages()) == 3
	})
	cancel()

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context canceled", err)
	}

	calls := adapter.snapshotCalls()
	if got, want := len(calls), 3; got != want {
		t.Fatalf("adapter call count = %d, want %d", got, want)
	}
	if calls[2].ThreadTS != "1713686400.000100" {
		t.Fatalf("adapter calls = %#v, want the deferred shared-session execution to remain last", calls)
	}
	firstTwo := map[string]bool{
		calls[0].ThreadTS: true,
		calls[1].ThreadTS: true,
	}
	if !firstTwo["1713686400.000100"] || !firstTwo["1713686400.000200"] {
		t.Fatalf("adapter calls = %#v, want the first two starts to cover one shared-session execution and the other thread in either order", calls)
	}

	for _, deliveryID := range []string{"Ev140", "Ev141", "Ev142"} {
		dedupe, err := store.Runtime().LoadEventDedupe(context.Background(), "mention", deliveryID)
		if err != nil {
			t.Fatalf("LoadEventDedupe(%s) error = %v", deliveryID, err)
		}
		if dedupe.Status != "processed" || dedupe.ProcessedAt == nil {
			t.Fatalf("LoadEventDedupe(%s) = %#v, want processed mention delivery", deliveryID, dedupe)
		}
	}
}

// Test: accepted mention invocations beyond the default global concurrency stay queued until an earlier execution releases its worker slot.
// Validates: AC-1976 (REQ-1420 - different session keys can run concurrently), AC-1976 (REQ-1422 - global concurrency defers additional executions once capacity is full)
func TestForegroundRuntimeStarterDefersQueuedMentionsWhenGlobalConcurrencyIsFull(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	project := registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(home, "workspace", "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C123",
	}
	if err := os.MkdirAll(project.LocalPath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := store.Projects().Upsert(ctx, project); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	client := &recordingSlackClient{}
	started := make(chan string, 5)
	releaseByThread := map[string]chan struct{}{
		"1713686400.000100": make(chan struct{}),
		"1713686400.000200": make(chan struct{}),
		"1713686400.000300": make(chan struct{}),
		"1713686400.000400": make(chan struct{}),
		"1713686400.000500": make(chan struct{}),
	}
	adapter := &fakePromptAdapter{
		sendPrompt: func(_ context.Context, req acpxadapter.SessionRequest) (acpxadapter.SessionResult, error) {
			started <- req.ThreadTS
			<-releaseByThread[req.ThreadTS]
			return acpxadapter.SessionResult{
				SessionName: acpxadapter.SessionName(req.ThreadTS),
				Output: fmt.Sprintf(
					`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"019db13d-f733-7ce0-8186-5aced7cdb2a7","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"result for %s"}}}}`+"\n"+`{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`,
					req.ThreadTS,
				),
			}, nil
		},
	}

	starter := &foregroundRuntimeStarter{
		source: &fakeSlackInvocationSource{
			invocations: []slack.InboundInvocation{
				{SourceType: slack.InboundSourceMention, DeliveryID: "Ev150", ChannelID: project.SlackChannelID, ThreadTS: "1713686400.000100", UserID: "U123", CommandText: "<@Ubot> ask status"},
				{SourceType: slack.InboundSourceMention, DeliveryID: "Ev151", ChannelID: project.SlackChannelID, ThreadTS: "1713686400.000200", UserID: "U123", CommandText: "<@Ubot> ask status"},
				{SourceType: slack.InboundSourceMention, DeliveryID: "Ev152", ChannelID: project.SlackChannelID, ThreadTS: "1713686400.000300", UserID: "U123", CommandText: "<@Ubot> ask status"},
				{SourceType: slack.InboundSourceMention, DeliveryID: "Ev153", ChannelID: project.SlackChannelID, ThreadTS: "1713686400.000400", UserID: "U123", CommandText: "<@Ubot> ask status"},
				{SourceType: slack.InboundSourceMention, DeliveryID: "Ev154", ChannelID: project.SlackChannelID, ThreadTS: "1713686400.000500", UserID: "U123", CommandText: "<@Ubot> ask status"},
			},
		},
		client:      client,
		renderer:    runtimemodel.SlackThreadRenderer{Client: client},
		adapter:     adapter,
		projectRepo: store.Projects(),
		runtimeRepo: store.Runtime(),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- starter.Start(ctx, runtimemodel.Status{})
	}()

	firstFour := make(map[string]bool, 4)
	for len(firstFour) < 4 {
		select {
		case threadTS := <-started:
			firstFour[threadTS] = true
		case <-time.After(time.Second):
			t.Fatalf("only %d executions started before timeout, want 4 at the default global concurrency limit", len(firstFour))
		}
	}

	for _, threadTS := range []string{"1713686400.000100", "1713686400.000200", "1713686400.000300", "1713686400.000400"} {
		if !firstFour[threadTS] {
			t.Fatalf("started threads = %#v, want initial worker slots filled by the first four unique sessions", firstFour)
		}
	}

	select {
	case threadTS := <-started:
		t.Fatalf("execution for %s started before a global worker slot was released", threadTS)
	case <-time.After(150 * time.Millisecond):
	}

	close(releaseByThread["1713686400.000100"])

	select {
	case threadTS := <-started:
		if threadTS != "1713686400.000500" {
			t.Fatalf("thread started after releasing one worker slot = %q, want queued fifth session", threadTS)
		}
	case <-time.After(time.Second):
		t.Fatal("queued fifth execution did not start after a global worker slot was released")
	}

	for _, threadTS := range []string{"1713686400.000200", "1713686400.000300", "1713686400.000400", "1713686400.000500"} {
		close(releaseByThread[threadTS])
	}

	waitForCondition(t, 2*time.Second, func() bool {
		if len(client.snapshotMessages()) != 5 {
			return false
		}
		for _, deliveryID := range []string{"Ev150", "Ev151", "Ev152", "Ev153", "Ev154"} {
			dedupe, err := store.Runtime().LoadEventDedupe(context.Background(), "mention", deliveryID)
			if err != nil || dedupe.Status != "processed" || dedupe.ProcessedAt == nil {
				return false
			}
		}
		return true
	})
	cancel()

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context canceled", err)
	}

	calls := adapter.snapshotCalls()
	if got, want := len(calls), 5; got != want {
		t.Fatalf("adapter call count = %d, want %d", got, want)
	}

	for _, deliveryID := range []string{"Ev150", "Ev151", "Ev152", "Ev153", "Ev154"} {
		dedupe, err := store.Runtime().LoadEventDedupe(context.Background(), "mention", deliveryID)
		if err != nil {
			t.Fatalf("LoadEventDedupe(%s) error = %v", deliveryID, err)
		}
		if dedupe.Status != "processed" || dedupe.ProcessedAt == nil {
			t.Fatalf("LoadEventDedupe(%s) = %#v, want processed mention delivery", deliveryID, dedupe)
		}
	}
}

// Test: empty app_mention invocations do not call ACPX and instead return usage guidance in the mention thread.
// Validates: AC-1817 (REQ-1184 - empty mention invocations return usage-oriented guidance)
func TestForegroundRuntimeStarterRendersUsageForEmptyMentionCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	project := registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(home, "workspace", "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C123",
	}
	if err := os.MkdirAll(project.LocalPath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := store.Projects().Upsert(ctx, project); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	client := &recordingSlackClient{}
	adapter := &fakePromptAdapter{}

	var debug bytes.Buffer
	starter := &foregroundRuntimeStarter{
		source: &fakeSlackInvocationSource{
			invocations: []slack.InboundInvocation{
				{
					SourceType:  slack.InboundSourceMention,
					DeliveryID:  "Ev125",
					ChannelID:   project.SlackChannelID,
					ThreadTS:    "1713686400.000100",
					UserID:      "U123",
					CommandText: "<@Ubot>",
				},
			},
		},
		client:      client,
		renderer:    runtimemodel.SlackThreadRenderer{Client: client},
		adapter:     adapter,
		projectRepo: store.Projects(),
		runtimeRepo: store.Runtime(),
	}
	starter.debugf = func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		_, _ = debug.WriteString(line + "\n")
		if strings.Contains(line, "mention processed delivery=Ev125") {
			cancel()
		}
	}

	err = starter.Start(ctx, runtimemodel.Status{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context canceled", err)
	}

	if got := len(adapter.calls); got != 0 {
		t.Fatalf("adapter call count = %d, want 0 for empty mention", got)
	}
	if got, want := len(client.messages), 1; got != want {
		t.Fatalf("slack message count = %d, want %d", got, want)
	}
	if client.messages[0].Text != mentionUsageText() {
		t.Fatalf("slack usage text = %q, want %q", client.messages[0].Text, mentionUsageText())
	}
	if client.messages[0].ThreadTS != "1713686400.000100" {
		t.Fatalf("slack usage thread ts = %q, want root thread anchor", client.messages[0].ThreadTS)
	}
	if !strings.Contains(debug.String(), "runtime.loop: rendered mention usage thread=1713686400.000100") {
		t.Fatalf("debug output missing usage render line: %s", debug.String())
	}
}

// Test: unregistered mention invocations render a human-readable Slack message and do not start ACPX execution.
// Validates: AC-1821 (REQ-1190 - unregistered channels reject before execution starts), AC-1821 (REQ-1191 - mention rejections are regular Slack messages)
func TestForegroundRuntimeStarterRendersUnregisteredMentionRejection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	client := &recordingSlackClient{}
	adapter := &fakePromptAdapter{}

	var debug bytes.Buffer
	var debugMu sync.Mutex
	starter := &foregroundRuntimeStarter{
		source: &fakeSlackInvocationSource{
			invocations: []slack.InboundInvocation{
				{
					SourceType:  slack.InboundSourceMention,
					DeliveryID:  "Ev-unregistered-mention",
					ChannelID:   "C404",
					ThreadTS:    "1713686400.000100",
					UserID:      "U123",
					CommandText: "<@Ubot> ask status",
				},
			},
		},
		client:      client,
		renderer:    runtimemodel.SlackThreadRenderer{Client: client},
		adapter:     adapter,
		projectRepo: store.Projects(),
		runtimeRepo: store.Runtime(),
	}
	starter.debugf = func(format string, args ...any) {
		debugMu.Lock()
		defer debugMu.Unlock()
		_, _ = debug.WriteString(fmt.Sprintf(format, args...) + "\n")
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- starter.Start(ctx, runtimemodel.Status{})
	}()

	waitForCondition(t, time.Second, func() bool {
		return len(client.snapshotMessages()) == 1
	})
	cancel()

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context canceled", err)
	}

	if got := len(adapter.calls); got != 0 {
		t.Fatalf("adapter call count = %d, want 0 for rejected mention", got)
	}

	messages := client.snapshotMessages()
	if got, want := len(messages), 1; got != want {
		t.Fatalf("slack message count = %d, want %d", got, want)
	}
	if messages[0].ChannelID != "C404" || messages[0].ThreadTS != "1713686400.000100" {
		t.Fatalf("mention rejection message = %#v, want channel/thread preserved", messages[0])
	}
	if messages[0].Text != "This Slack channel is not registered in the project registry." {
		t.Fatalf("mention rejection text = %q, want rejection message", messages[0].Text)
	}
	if got := len(client.snapshotResponseURLMessages()); got != 0 {
		t.Fatalf("response_url message count = %d, want 0 for mention rejection", got)
	}

	if _, err := store.Runtime().LoadEventDedupe(context.Background(), "mention", "Ev-unregistered-mention"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("LoadEventDedupe() error = %v, want storage.ErrNotFound", err)
	}

	debugMu.Lock()
	output := debug.String()
	debugMu.Unlock()
	for _, fragment := range []string{
		"runtime.loop: lifecycle event=execution_rejected source=mention delivery_id=Ev-unregistered-mention channel_id=C404 project=unknown status=rejected",
		"runtime.loop: rendered slack rejection delivery=Ev-unregistered-mention source=mention channel=C404 ephemeral=false",
	} {
		if !strings.Contains(output, fragment) {
			t.Fatalf("debug output missing %q: %s", fragment, output)
		}
	}
}

// Test: unregistered slash invocations render an ephemeral response_url rejection and do not start ACPX execution.
// Validates: AC-1822 (REQ-1190 - unregistered channels reject before execution starts), AC-1822 (REQ-1192 - slash rejections are ephemeral)
func TestForegroundRuntimeStarterRendersUnregisteredSlashRejection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	client := &recordingSlackClient{}
	adapter := &fakePromptAdapter{}

	var debug bytes.Buffer
	var debugMu sync.Mutex
	starter := &foregroundRuntimeStarter{
		source: &fakeSlackInvocationSource{
			invocations: []slack.InboundInvocation{
				{
					SourceType:    slack.InboundSourceSlash,
					DeliveryID:    "Ev-unregistered-slash",
					ChannelID:     "C404",
					UserID:        "U123",
					CommandText:   "status",
					ResponseURL:   "https://hooks.slack.test/response",
					AckEnvelopeID: "Ev-unregistered-slash",
					Acked:         true,
				},
			},
		},
		client:      client,
		renderer:    runtimemodel.SlackThreadRenderer{Client: client},
		adapter:     adapter,
		projectRepo: store.Projects(),
		runtimeRepo: store.Runtime(),
	}
	starter.debugf = func(format string, args ...any) {
		debugMu.Lock()
		defer debugMu.Unlock()
		_, _ = debug.WriteString(fmt.Sprintf(format, args...) + "\n")
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- starter.Start(ctx, runtimemodel.Status{})
	}()

	waitForCondition(t, time.Second, func() bool {
		return len(client.snapshotResponseURLMessages()) == 1
	})
	cancel()

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context canceled", err)
	}

	if got := len(adapter.calls); got != 0 {
		t.Fatalf("adapter call count = %d, want 0 for rejected slash", got)
	}
	if got := len(client.snapshotMessages()); got != 0 {
		t.Fatalf("slack channel message count = %d, want 0 for slash rejection", got)
	}

	responseURLMessages := client.snapshotResponseURLMessages()
	if got, want := len(responseURLMessages), 1; got != want {
		t.Fatalf("response_url message count = %d, want %d", got, want)
	}
	if responseURLMessages[0].ResponseURL != "https://hooks.slack.test/response" {
		t.Fatalf("response_url = %q, want response url", responseURLMessages[0].ResponseURL)
	}
	if responseURLMessages[0].Text != "This Slack channel is not registered in the project registry." {
		t.Fatalf("slash rejection text = %q, want rejection message", responseURLMessages[0].Text)
	}
	if responseURLMessages[0].ResponseType != slack.ResponseTypeEphemeral {
		t.Fatalf("response type = %q, want ephemeral", responseURLMessages[0].ResponseType)
	}

	if _, err := store.Runtime().LoadEventDedupe(context.Background(), "slash", "Ev-unregistered-slash"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("LoadEventDedupe() error = %v, want storage.ErrNotFound", err)
	}

	debugMu.Lock()
	output := debug.String()
	debugMu.Unlock()
	for _, fragment := range []string{
		"runtime.loop: lifecycle event=execution_rejected source=slash delivery_id=Ev-unregistered-slash channel_id=C404 project=unknown status=rejected",
		"runtime.loop: rendered slack rejection delivery=Ev-unregistered-slash source=slash channel=C404 ephemeral=true",
	} {
		if !strings.Contains(output, fragment) {
			t.Fatalf("debug output missing %q: %s", fragment, output)
		}
	}
}

// Test: slash invocations acknowledge before async execution, publish the final result into the worker-owned thread, and persist the full processed lifecycle.
// Validates: AC-1983 (REQ-1433 - slash acknowledgement is sent before async execution starts), AC-1983 (REQ-1435 - accepted slash execution completes asynchronously in the execution thread), AC-1981 (REQ-1429 - slash execution state is persisted), AC-1981 (REQ-1430 - slash lifecycle timestamps and terminal checkpoints are recorded)
func TestForegroundRuntimeStarterAcknowledgesSlashAndPublishesAsyncThreadResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	project := registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(home, "workspace", "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C123",
	}
	if err := os.MkdirAll(project.LocalPath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := store.Projects().Upsert(ctx, project); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	client := &recordingSlackClient{}
	sendPromptStarted := make(chan struct{}, 1)
	releasePrompt := make(chan struct{})
	adapter := &fakePromptAdapter{
		sendPrompt: func(_ context.Context, req acpxadapter.SessionRequest) (acpxadapter.SessionResult, error) {
			sendPromptStarted <- struct{}{}
			<-releasePrompt
			return acpxadapter.SessionResult{
				SessionName: acpxadapter.SessionName(req.ThreadTS),
				Output: `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"019db13d-f733-7ce0-8186-5aced7cdb2a7","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"async slash result"}}}}
{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`,
			}, nil
		},
	}

	var debug bytes.Buffer
	var debugMu sync.Mutex
	starter := &foregroundRuntimeStarter{
		source: &fakeSlackInvocationSource{
			invocations: []slack.InboundInvocation{
				{
					SourceType:    slack.InboundSourceSlash,
					DeliveryID:    "3-fwdc2",
					ChannelID:     project.SlackChannelID,
					UserID:        "U123",
					CommandText:   "ask summarize open work",
					ResponseURL:   "https://hooks.slack.test/response",
					AckEnvelopeID: "3-fwdc2",
					Acked:         true,
				},
			},
		},
		client:      client,
		renderer:    runtimemodel.SlackThreadRenderer{Client: client},
		adapter:     adapter,
		projectRepo: store.Projects(),
		runtimeRepo: store.Runtime(),
	}
	starter.debugf = func(format string, args ...any) {
		debugMu.Lock()
		defer debugMu.Unlock()
		_, _ = debug.WriteString(fmt.Sprintf(format, args...) + "\n")
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- starter.Start(ctx, runtimemodel.Status{})
	}()

	waitForCondition(t, time.Second, func() bool {
		debugMu.Lock()
		defer debugMu.Unlock()
		return strings.Contains(debug.String(), "runtime.loop: slash ack sent delivery=3-fwdc2")
	})

	select {
	case <-sendPromptStarted:
	case <-time.After(time.Second):
		t.Fatal("SendPrompt() was not started for slash invocation")
	}

	messages := client.snapshotMessages()
	if got, want := len(messages), 1; got != want {
		t.Fatalf("message count before releasing async slash result = %d, want %d", got, want)
	}
	if messages[0].ThreadTS != "" {
		t.Fatalf("slash start message thread ts = %q, want empty root channel post", messages[0].ThreadTS)
	}
	if messages[0].Text != slashStartText("ask summarize open work") {
		t.Fatalf("slash start message text = %q, want %q", messages[0].Text, slashStartText("ask summarize open work"))
	}

	close(releasePrompt)

	waitForCondition(t, 2*time.Second, func() bool {
		messages := client.snapshotMessages()
		return len(messages) == 2 && messages[1].ThreadTS == "1713686400.000100" && messages[1].Text == "async slash result"
	})
	cancel()

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context canceled", err)
	}

	if got, want := len(adapter.calls), 1; got != want {
		t.Fatalf("adapter call count = %d, want %d", got, want)
	}
	if adapter.calls[0].Prompt != "summarize open work" {
		t.Fatalf("adapter call prompt = %q, want ask payload without command prefix", adapter.calls[0].Prompt)
	}
	if adapter.calls[0].ThreadTS != "1713686400.000100" {
		t.Fatalf("adapter call thread ts = %q, want execution thread timestamp", adapter.calls[0].ThreadTS)
	}

	executionState, err := store.Runtime().LoadExecutionStateByDelivery(context.Background(), "slash", "3-fwdc2")
	if err != nil {
		t.Fatalf("LoadExecutionStateByDelivery() error = %v", err)
	}
	if executionState.Status != runtimemodel.ExecutionStatusProcessed || executionState.CompletedAt == nil {
		t.Fatalf("LoadExecutionStateByDelivery() = %#v, want processed slash execution", executionState)
	}
	if executionState.ThreadTS != "1713686400.000100" || executionState.SessionName != "slack-1713686400.000100" {
		t.Fatalf("LoadExecutionStateByDelivery() thread/session = (%q, %q), want worker-owned slash thread/session", executionState.ThreadTS, executionState.SessionName)
	}
	if executionState.QueuedAt.IsZero() || executionState.StartedAt == nil || executionState.RenderingStartedAt == nil {
		t.Fatalf("LoadExecutionStateByDelivery() timestamps = %#v, want queued/running/rendering markers", executionState)
	}
	if executionState.PublisherCheckpointKind != string(runtimemodel.ACPXEventSessionDone) || executionState.PublisherCheckpointSummary != "" {
		t.Fatalf("LoadExecutionStateByDelivery() checkpoint = (%q, %q), want session_done/empty summary", executionState.PublisherCheckpointKind, executionState.PublisherCheckpointSummary)
	}

	threadState, err := store.Runtime().LoadThreadState(context.Background(), "1713686400.000100")
	if err != nil {
		t.Fatalf("LoadThreadState() error = %v", err)
	}
	if threadState.LastStatus != "processed" || threadState.LastRequestID != "3-fwdc2" {
		t.Fatalf("LoadThreadState() = %#v, want processed slash thread state", threadState)
	}

	dedupe, err := store.Runtime().LoadEventDedupe(context.Background(), "slash", "3-fwdc2")
	if err != nil {
		t.Fatalf("LoadEventDedupe() error = %v", err)
	}
	if dedupe.Status != "processed" || dedupe.ProcessedAt == nil {
		t.Fatalf("LoadEventDedupe() = %#v, want processed slash delivery", dedupe)
	}

	debugMu.Lock()
	output := debug.String()
	debugMu.Unlock()
	for _, fragment := range []string{
		"runtime.loop: slash ack sent delivery=3-fwdc2",
		"runtime.loop: dispatching slash delivery=3-fwdc2 project=alpha session=slack-1713686400.000100 thread=1713686400.000100",
		"runtime.loop: rendered slash reply thread=1713686400.000100 session=slack-1713686400.000100",
		"runtime.loop: slash processed delivery=3-fwdc2 session=slack-1713686400.000100",
	} {
		if !strings.Contains(output, fragment) {
			t.Fatalf("debug output missing %q: %s", fragment, output)
		}
	}
	if strings.Index(output, "runtime.loop: slash ack sent delivery=3-fwdc2") > strings.Index(output, "runtime.loop: slash processed delivery=3-fwdc2 session=slack-1713686400.000100") {
		t.Fatalf("debug output did not show slash ack before completion: %s", output)
	}
}

// Test: slash execution is never enqueued or started until the inbound source confirms acknowledgement completion.
// Validates: AC-1984 (REQ-1434 - ack failure prevents async execution start)
func TestForegroundRuntimeStarterDoesNotStartSlashExecutionWithoutAcknowledgement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	project := registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(home, "workspace", "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C123",
	}
	if err := os.MkdirAll(project.LocalPath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := store.Projects().Upsert(ctx, project); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	client := &recordingSlackClient{}
	adapter := &fakePromptAdapter{}

	var debug bytes.Buffer
	var debugMu sync.Mutex
	starter := &foregroundRuntimeStarter{
		source: &fakeSlackInvocationSource{
			invocations: []slack.InboundInvocation{
				{
					SourceType:    slack.InboundSourceSlash,
					DeliveryID:    "3-unacked",
					ChannelID:     project.SlackChannelID,
					UserID:        "U123",
					CommandText:   "status",
					ResponseURL:   "https://hooks.slack.test/response",
					AckEnvelopeID: "3-unacked",
				},
			},
		},
		client:      client,
		renderer:    runtimemodel.SlackThreadRenderer{Client: client},
		adapter:     adapter,
		projectRepo: store.Projects(),
		runtimeRepo: store.Runtime(),
	}
	starter.debugf = func(format string, args ...any) {
		debugMu.Lock()
		defer debugMu.Unlock()
		_, _ = debug.WriteString(fmt.Sprintf(format, args...) + "\n")
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- starter.Start(ctx, runtimemodel.Status{})
	}()

	waitForCondition(t, 2*time.Second, func() bool {
		debugMu.Lock()
		output := debug.String()
		debugMu.Unlock()
		return strings.Contains(output, `runtime.loop: slash failed delivery=3-unacked: slash invocation acknowledgement not completed for delivery "3-unacked"`)
	})
	cancel()

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context canceled", err)
	}

	if got := len(adapter.snapshotCalls()); got != 0 {
		t.Fatalf("adapter call count = %d, want 0 when slash ack did not complete", got)
	}
	if got := len(client.snapshotMessages()); got != 0 {
		t.Fatalf("slack channel message count = %d, want 0 when slash ack did not complete", got)
	}
	if got := len(client.snapshotResponseURLMessages()); got != 0 {
		t.Fatalf("response_url message count = %d, want 0 when slash ack did not complete", got)
	}

	if _, err := store.Runtime().LoadEventDedupe(context.Background(), "slash", "3-unacked"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("LoadEventDedupe() error = %v, want not found when slash ack did not complete", err)
	}
}

// Test: /spexus status is acknowledged immediately, dispatched through channel-level execution, and rendered back into the new execution thread.
// Validates: AC-1818 (REQ-1186 - slash invocations acknowledge within the Slack window), AC-1818 (REQ-1187 - slash commands execute as channel-level commands), AC-1824 (REQ-1198 - slash lifecycle logs record source and status)
func TestForegroundRuntimeStarterProcessesSlashStatusCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	project := registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(home, "workspace", "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C123",
	}
	if err := os.MkdirAll(project.LocalPath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := store.Projects().Upsert(ctx, project); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	client := &recordingSlackClient{}
	adapter := &fakePromptAdapter{
		results: []acpxadapter.SessionResult{
			{
				SessionName: acpxadapter.SessionName("1713686400.000100"),
				Output: `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"019db13d-f733-7ce0-8186-5aced7cdb2a7","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"slash status result"}}}}
{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`,
			},
		},
	}

	var debug bytes.Buffer
	var debugMu sync.Mutex
	starter := &foregroundRuntimeStarter{
		source: &fakeSlackInvocationSource{
			invocations: []slack.InboundInvocation{
				{
					SourceType:    slack.InboundSourceSlash,
					DeliveryID:    "3-fwdc5",
					ChannelID:     project.SlackChannelID,
					UserID:        "U123",
					CommandText:   "status",
					ResponseURL:   "https://hooks.slack.test/response",
					AckEnvelopeID: "3-fwdc5",
					Acked:         true,
				},
			},
		},
		client:      client,
		renderer:    runtimemodel.SlackThreadRenderer{Client: client},
		adapter:     adapter,
		projectRepo: store.Projects(),
		runtimeRepo: store.Runtime(),
	}
	starter.debugf = func(format string, args ...any) {
		debugMu.Lock()
		defer debugMu.Unlock()
		_, _ = debug.WriteString(fmt.Sprintf(format, args...) + "\n")
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- starter.Start(ctx, runtimemodel.Status{})
	}()

	waitForCondition(t, 2*time.Second, func() bool {
		messages := client.snapshotMessages()
		return len(messages) == 2 && messages[1].ThreadTS == "1713686400.000100" && messages[1].Text == "slash status result"
	})
	cancel()

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context canceled", err)
	}

	if got, want := len(adapter.calls), 1; got != want {
		t.Fatalf("adapter call count = %d, want %d", got, want)
	}
	if adapter.calls[0].Prompt != "status" {
		t.Fatalf("adapter call prompt = %q, want status", adapter.calls[0].Prompt)
	}
	if adapter.calls[0].ThreadTS != "1713686400.000100" {
		t.Fatalf("adapter call thread ts = %q, want execution thread timestamp", adapter.calls[0].ThreadTS)
	}

	messages := client.snapshotMessages()
	if got, want := len(messages), 2; got != want {
		t.Fatalf("slack message count = %d, want %d", got, want)
	}
	if messages[0].ThreadTS != "" {
		t.Fatalf("slash start message thread ts = %q, want empty root channel post", messages[0].ThreadTS)
	}
	if messages[0].Text != slashStartText("status") {
		t.Fatalf("slash start message text = %q, want %q", messages[0].Text, slashStartText("status"))
	}
	if messages[1].ThreadTS != "1713686400.000100" {
		t.Fatalf("slash result thread ts = %q, want execution thread timestamp", messages[1].ThreadTS)
	}
	if messages[1].Text != "slash status result" {
		t.Fatalf("slash result text = %q, want rendered ACPX output", messages[1].Text)
	}

	debugMu.Lock()
	output := debug.String()
	debugMu.Unlock()
	for _, fragment := range []string{
		"runtime.loop: slash ack sent delivery=3-fwdc5",
		"runtime.loop: lifecycle event=ack_sent source=slash delivery_id=3-fwdc5 channel_id=C123 project=alpha status=acked",
		"runtime.loop: lifecycle event=execution_started source=slash delivery_id=3-fwdc5 channel_id=C123 project=alpha status=processing session=slack-1713686400.000100",
		"runtime.loop: lifecycle event=execution_completed source=slash delivery_id=3-fwdc5 channel_id=C123 project=alpha status=processed session=slack-1713686400.000100",
		"runtime.loop: rendered slash reply thread=1713686400.000100 session=slack-1713686400.000100",
		"runtime.loop: slash processed delivery=3-fwdc5 session=slack-1713686400.000100",
	} {
		if !strings.Contains(output, fragment) {
			t.Fatalf("debug output missing %q: %s", fragment, output)
		}
	}
	if strings.Index(output, "runtime.loop: slash ack sent delivery=3-fwdc5") > strings.Index(output, "runtime.loop: slash processed delivery=3-fwdc5 session=slack-1713686400.000100") {
		t.Fatalf("debug output did not show slash ack before completion: %s", output)
	}
}

// Test: async slash execution failures after a successful ack are reported back into the execution thread.
// Validates: AC-1818 (REQ-1186 - slash invocations acknowledge within the Slack window), AC-1819 (REQ-1188 - failures after ack are published asynchronously), AC-1819 (REQ-1195 - slash commands use the execution thread created after ack)
func TestForegroundRuntimeStarterReportsSlashAsyncFailureInExecutionThread(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	project := registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(home, "workspace", "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C123",
	}
	if err := os.MkdirAll(project.LocalPath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := store.Projects().Upsert(ctx, project); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	client := &recordingSlackClient{}
	adapter := &fakePromptAdapter{
		sendPrompt: func(_ context.Context, req acpxadapter.SessionRequest) (acpxadapter.SessionResult, error) {
			return acpxadapter.SessionResult{}, errors.New("acpx async failure")
		},
	}

	var debug bytes.Buffer
	var debugMu sync.Mutex
	starter := &foregroundRuntimeStarter{
		source: &fakeSlackInvocationSource{
			invocations: []slack.InboundInvocation{
				{
					SourceType:    slack.InboundSourceSlash,
					DeliveryID:    "3-fwdc3",
					ChannelID:     project.SlackChannelID,
					UserID:        "U123",
					CommandText:   "status",
					ResponseURL:   "https://hooks.slack.test/response",
					AckEnvelopeID: "3-fwdc3",
					Acked:         true,
				},
			},
		},
		client:      client,
		renderer:    runtimemodel.SlackThreadRenderer{Client: client},
		adapter:     adapter,
		projectRepo: store.Projects(),
		runtimeRepo: store.Runtime(),
	}
	starter.debugf = func(format string, args ...any) {
		debugMu.Lock()
		defer debugMu.Unlock()
		_, _ = debug.WriteString(fmt.Sprintf(format, args...) + "\n")
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- starter.Start(ctx, runtimemodel.Status{})
	}()

	waitForCondition(t, 2*time.Second, func() bool {
		messages := client.snapshotMessages()
		return len(messages) == 2 && messages[1].ThreadTS == "1713686400.000100" && strings.Contains(messages[1].Text, "Session error: acpx async failure")
	})
	cancel()

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context canceled", err)
	}

	messages := client.snapshotMessages()
	if got, want := len(messages), 2; got != want {
		t.Fatalf("message count = %d, want %d", got, want)
	}
	if messages[0].Text != slashStartText("status") {
		t.Fatalf("slash start message text = %q, want %q", messages[0].Text, slashStartText("status"))
	}
	if messages[1].Text != "Session error: acpx async failure" {
		t.Fatalf("slash failure message text = %q, want session error", messages[1].Text)
	}

	debugMu.Lock()
	output := debug.String()
	debugMu.Unlock()
	for _, fragment := range []string{
		"runtime.loop: slash ack sent delivery=3-fwdc3",
		"runtime.loop: slash async failed delivery=3-fwdc3: acpx async failure",
	} {
		if !strings.Contains(output, fragment) {
			t.Fatalf("debug output missing %q: %s", fragment, output)
		}
	}
}

// Test: if the worker cannot create the slash execution root message after ack, the runtime reports the failure through response_url and never starts ACPX.
// Validates: AC-1983 (REQ-1435 - accepted slash execution is bound to a worker-owned execution thread), AC-1984 (REQ-1434 - post-ack bootstrap failures are deterministic and do not start execution)
func TestForegroundRuntimeStarterReportsSlashBootstrapFailureViaResponseURL(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	project := registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(home, "workspace", "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C123",
	}
	if err := os.MkdirAll(project.LocalPath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := store.Projects().Upsert(ctx, project); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	client := &recordingSlackClient{postMessageErr: errors.New("slack post failed")}
	adapter := &fakePromptAdapter{}

	var debug bytes.Buffer
	var debugMu sync.Mutex
	starter := &foregroundRuntimeStarter{
		source: &fakeSlackInvocationSource{
			invocations: []slack.InboundInvocation{
				{
					SourceType:    slack.InboundSourceSlash,
					DeliveryID:    "3-bootstrap-fail",
					ChannelID:     project.SlackChannelID,
					UserID:        "U123",
					CommandText:   "status",
					ResponseURL:   "https://hooks.slack.test/response",
					AckEnvelopeID: "3-bootstrap-fail",
					Acked:         true,
				},
			},
		},
		client:      client,
		renderer:    runtimemodel.SlackThreadRenderer{Client: client},
		adapter:     adapter,
		projectRepo: store.Projects(),
		runtimeRepo: store.Runtime(),
	}
	starter.debugf = func(format string, args ...any) {
		debugMu.Lock()
		defer debugMu.Unlock()
		_, _ = debug.WriteString(fmt.Sprintf(format, args...) + "\n")
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- starter.Start(ctx, runtimemodel.Status{})
	}()

	waitForCondition(t, 2*time.Second, func() bool {
		responseURLMessages := client.snapshotResponseURLMessages()
		return len(responseURLMessages) == 1 && responseURLMessages[0].Text == "Session error: slack post failed"
	})
	cancel()

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context canceled", err)
	}

	if got := len(adapter.snapshotCalls()); got != 0 {
		t.Fatalf("adapter call count = %d, want 0 when slash bootstrap fails", got)
	}
	if got := len(client.snapshotMessages()); got != 0 {
		t.Fatalf("slack channel message count = %d, want 0 when root message bootstrap fails", got)
	}

	responseURLMessages := client.snapshotResponseURLMessages()
	if got, want := len(responseURLMessages), 1; got != want {
		t.Fatalf("response_url message count = %d, want %d", got, want)
	}
	if responseURLMessages[0].ResponseURL != "https://hooks.slack.test/response" {
		t.Fatalf("response_url = %q, want response url", responseURLMessages[0].ResponseURL)
	}
	if responseURLMessages[0].ResponseType != slack.ResponseTypeEphemeral {
		t.Fatalf("response type = %q, want ephemeral", responseURLMessages[0].ResponseType)
	}

	dedupe, err := store.Runtime().LoadEventDedupe(context.Background(), "slash", "3-bootstrap-fail")
	if err != nil {
		t.Fatalf("LoadEventDedupe() error = %v", err)
	}
	if dedupe.Status != "failed" || dedupe.ProcessedAt == nil {
		t.Fatalf("LoadEventDedupe() = %#v, want failed slash delivery after bootstrap error", dedupe)
	}

	debugMu.Lock()
	output := debug.String()
	debugMu.Unlock()
	for _, fragment := range []string{
		"runtime.loop: slash ack sent delivery=3-bootstrap-fail",
		"runtime.loop: slash start message failed delivery=3-bootstrap-fail: slack post failed",
	} {
		if !strings.Contains(output, fragment) {
			t.Fatalf("debug output missing %q: %s", fragment, output)
		}
	}
}

// Test: slash ingestion records the acknowledgement before enqueueing the async execution request.
// Validates: AC-1983 (REQ-1433 - slash acknowledgement happens before async execution begins), AC-1983 (REQ-1435 - accepted slash work is handed off to async execution)
func TestForegroundRuntimeStarterHandleSlashInvocationAcknowledgesBeforeEnqueue(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	project := registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(home, "workspace", "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C123",
	}
	if err := os.MkdirAll(project.LocalPath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := store.Projects().Upsert(ctx, project); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	var debug bytes.Buffer
	var debugMu sync.Mutex
	ackLoggedBeforeEnqueue := false
	queue := &recordingExecutionQueue{
		enqueue: func(_ context.Context, request runtimemodel.ExecutionRequest) error {
			debugMu.Lock()
			ackLoggedBeforeEnqueue = strings.Contains(debug.String(), "runtime.loop: slash ack sent delivery=3-order-check")
			debugMu.Unlock()

			if request.SourceType != slack.InboundSourceSlash {
				t.Fatalf("Enqueue() source type = %q, want slash", request.SourceType)
			}
			if request.CommandText != "status" {
				t.Fatalf("Enqueue() command text = %q, want status", request.CommandText)
			}
			return nil
		},
	}
	starter := &foregroundRuntimeStarter{
		projectRepo: store.Projects(),
	}
	starter.debugf = func(format string, args ...any) {
		debugMu.Lock()
		defer debugMu.Unlock()
		_, _ = debug.WriteString(fmt.Sprintf(format, args...) + "\n")
	}

	coordinator := runtimemodel.NewSlackTurnCoordinator(store.Runtime(), "runtime-start")
	err = starter.handleSlashInvocation(ctx, coordinator, queue, slack.InboundInvocation{
		SourceType:    slack.InboundSourceSlash,
		DeliveryID:    "3-order-check",
		ChannelID:     project.SlackChannelID,
		UserID:        "U123",
		CommandText:   "status",
		ResponseURL:   "https://hooks.slack.test/response",
		AckEnvelopeID: "3-order-check",
		Acked:         true,
	})
	if err != nil {
		t.Fatalf("handleSlashInvocation() error = %v", err)
	}
	if !ackLoggedBeforeEnqueue {
		t.Fatal("handleSlashInvocation() enqueued the slash request before logging acknowledgement")
	}

	requests := queue.snapshotRequests()
	if got, want := len(requests), 1; got != want {
		t.Fatalf("enqueued request count = %d, want %d", got, want)
	}
	if requests[0].ThreadTS != "" || requests[0].SessionName != "" {
		t.Fatalf("enqueued slash request = %#v, want worker-owned thread/session bootstrap", requests[0])
	}
}

// Test: duplicate mention deliveries reuse the source-aware dedupe record and do not execute ACPX twice.
// Validates: AC-1825 (REQ-1193 - mention invocations are classified as source=mention), AC-1825 (REQ-1196 - duplicate mention deliveries are deduplicated), AC-1823 (REQ-1197 - mention lifecycle logs record source and status)
func TestForegroundRuntimeStarterSkipsDuplicateMentionDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := storage.OpenDefault(context.Background())
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	project := registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(home, "workspace", "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C123",
	}
	if err := os.MkdirAll(project.LocalPath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := store.Projects().Upsert(context.Background(), project); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	client := &recordingSlackClient{}
	adapter := &fakePromptAdapter{}

	var debug bytes.Buffer
	var debugMu sync.Mutex
	starter := &foregroundRuntimeStarter{
		source: &fakeSlackInvocationSource{
			invocations: []slack.InboundInvocation{
				{
					SourceType:  slack.InboundSourceMention,
					DeliveryID:  "Ev126",
					ChannelID:   project.SlackChannelID,
					ThreadTS:    "1713686400.000100",
					UserID:      "U123",
					CommandText: "<@Ubot> ask status",
				},
				{
					SourceType:  slack.InboundSourceMention,
					DeliveryID:  "Ev126",
					ChannelID:   project.SlackChannelID,
					ThreadTS:    "1713686400.000100",
					UserID:      "U123",
					CommandText: "<@Ubot> ask status",
				},
			},
		},
		client:      client,
		renderer:    runtimemodel.SlackThreadRenderer{Client: client},
		adapter:     adapter,
		projectRepo: store.Projects(),
		runtimeRepo: store.Runtime(),
	}
	starter.debugf = func(format string, args ...any) {
		debugMu.Lock()
		defer debugMu.Unlock()
		line := fmt.Sprintf(format, args...)
		_, _ = debug.WriteString(line + "\n")
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- starter.Start(ctx, runtimemodel.Status{})
	}()

	waitForCondition(t, 2*time.Second, func() bool {
		debugMu.Lock()
		output := debug.String()
		debugMu.Unlock()
		return strings.Contains(output, "runtime.loop: duplicate mention skipped delivery=Ev126") &&
			len(adapter.snapshotCalls()) == 0 &&
			len(client.snapshotMessages()) == 1
	})
	cancel()

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context canceled", err)
	}

	if got, want := len(adapter.calls), 0; got != want {
		t.Fatalf("adapter call count = %d, want %d", got, want)
	}
	if got, want := len(client.messages), 1; got != want {
		t.Fatalf("slack message count = %d, want %d", got, want)
	}
	if client.messages[0].Text != "thread is inactive" {
		t.Fatalf("slack message text = %q, want local status reply", client.messages[0].Text)
	}

	dedupe, err := store.Runtime().LoadEventDedupe(context.Background(), "mention", "Ev126")
	if err != nil {
		t.Fatalf("LoadEventDedupe() error = %v", err)
	}
	if dedupe.Status != "processed" || dedupe.ProcessedAt == nil {
		t.Fatalf("LoadEventDedupe() = %#v, want processed mention delivery", dedupe)
	}
	executions, err := store.Runtime().ListExecutions(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListExecutions() error = %v", err)
	}
	if got := len(executions); got != 0 {
		t.Fatalf("execution count = %d, want no ACPX execution records for local mention status", got)
	}

	executionState, err := store.Runtime().LoadExecutionStateByDelivery(context.Background(), "mention", "Ev126")
	if err != nil {
		t.Fatalf("LoadExecutionStateByDelivery() error = %v", err)
	}
	if executionState.ExecutionID != "mention:Ev126" || executionState.Status != runtimemodel.ExecutionStatusProcessed {
		t.Fatalf("LoadExecutionStateByDelivery() = %#v, want processed mention execution", executionState)
	}
	if executionState.ThreadTS != "1713686400.000100" || executionState.SessionName != "slack-1713686400.000100" {
		t.Fatalf("LoadExecutionStateByDelivery() thread/session = (%q, %q), want mention thread/session", executionState.ThreadTS, executionState.SessionName)
	}
	if executionState.QueuedAt.IsZero() || executionState.StartedAt == nil || executionState.RenderingStartedAt == nil || executionState.CompletedAt == nil {
		t.Fatalf("LoadExecutionStateByDelivery() timestamps = %#v, want queued/running/rendering/completed markers", executionState)
	}
	if executionState.PublisherCheckpointKind != string(runtimemodel.ACPXEventSessionDone) {
		t.Fatalf("LoadExecutionStateByDelivery() checkpoint kind = %q, want %q", executionState.PublisherCheckpointKind, runtimemodel.ACPXEventSessionDone)
	}

	debugMu.Lock()
	output := debug.String()
	debugMu.Unlock()
	for _, fragment := range []string{
		"runtime.loop: lifecycle event=execution_enqueued source=mention delivery_id=Ev126 channel_id=C123 project=alpha status=queued session=slack-1713686400.000100",
		"runtime.loop: lifecycle event=execution_started source=mention delivery_id=Ev126 channel_id=C123 project=alpha status=processing",
		"runtime.loop: lifecycle event=execution_completed source=mention delivery_id=Ev126 channel_id=C123 project=alpha status=processed",
		"runtime.loop: lifecycle event=duplicate_skipped source=mention delivery_id=Ev126 channel_id=C123 project=alpha status=duplicate",
	} {
		if !strings.Contains(output, fragment) {
			t.Fatalf("debug output missing %q: %s", fragment, output)
		}
	}
}

// Test: startup reconciliation marks multiple stale non-terminal executions failed, clears runtime locks, and only notifies Slack when a thread anchor exists.
// Validates: AC-1982 (REQ-1431 - startup recovery reconciles stale queued and running executions), AC-1982 (REQ-1432 - recovery remains observable and posts terminal Slack failure only when it has a delivery thread)
func TestForegroundRuntimeStarterReconcilesNonTerminalExecutionsOnStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	project := registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(home, "workspace", "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C123",
	}
	if err := os.MkdirAll(project.LocalPath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := store.Projects().Upsert(ctx, project); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	threadTS := "1713686400.000100"
	threadState := runtimemodel.ThreadState{
		ThreadTS:      threadTS,
		ChannelID:     project.SlackChannelID,
		ProjectName:   project.Name,
		SessionName:   "slack-" + threadTS,
		LastStatus:    "processing",
		LastRequestID: "Ev-recover",
	}
	if err := store.Runtime().SaveThreadState(ctx, threadState); err != nil {
		t.Fatalf("SaveThreadState() error = %v", err)
	}

	receivedAt := time.Now().UTC().Add(-3 * time.Minute)
	if err := store.Runtime().SaveEventDedupe(ctx, runtimemodel.EventDedupe{
		SourceType: "mention",
		DeliveryID: "Ev-recover",
		ReceivedAt: receivedAt,
		Status:     "processing",
	}); err != nil {
		t.Fatalf("SaveEventDedupe() error = %v", err)
	}

	queuedAt := time.Now().UTC().Add(-2 * time.Minute)
	startedAt := time.Now().UTC().Add(-time.Minute)
	if err := store.Runtime().SaveExecutionState(ctx, runtimemodel.ExecutionState{
		ExecutionID: "mention:Ev-recover",
		SourceType:  "mention",
		DeliveryID:  "Ev-recover",
		ProjectName: project.Name,
		ChannelID:   project.SlackChannelID,
		ThreadTS:    threadTS,
		SessionName: threadState.SessionName,
		Status:      runtimemodel.ExecutionStatusRunning,
		QueuedAt:    queuedAt,
		StartedAt:   &startedAt,
		UpdatedAt:   startedAt,
	}); err != nil {
		t.Fatalf("SaveExecutionState() error = %v", err)
	}

	lockExpires := time.Now().UTC().Add(time.Minute)
	if err := store.Runtime().SaveThreadLock(ctx, runtimemodel.ThreadLock{
		ThreadTS:       threadTS,
		LockOwner:      "runtime-start",
		LockedAt:       startedAt,
		LeaseExpiresAt: &lockExpires,
		UpdatedAt:      startedAt,
	}); err != nil {
		t.Fatalf("SaveThreadLock() error = %v", err)
	}

	if err := store.Runtime().SaveEventDedupe(ctx, runtimemodel.EventDedupe{
		SourceType: "slash",
		DeliveryID: "3-recover-queued",
		ReceivedAt: receivedAt.Add(30 * time.Second),
		Status:     "acked",
	}); err != nil {
		t.Fatalf("SaveEventDedupe(queued slash) error = %v", err)
	}

	queuedSlashAt := time.Now().UTC().Add(-90 * time.Second)
	if err := store.Runtime().SaveExecutionState(ctx, runtimemodel.ExecutionState{
		ExecutionID: "slash:3-recover-queued",
		SourceType:  "slash",
		DeliveryID:  "3-recover-queued",
		ProjectName: project.Name,
		ChannelID:   project.SlackChannelID,
		Status:      runtimemodel.ExecutionStatusQueued,
		QueuedAt:    queuedSlashAt,
		UpdatedAt:   queuedSlashAt,
	}); err != nil {
		t.Fatalf("SaveExecutionState(queued slash) error = %v", err)
	}

	client := &recordingSlackClient{}
	var debug bytes.Buffer
	var debugMu sync.Mutex
	starter := &foregroundRuntimeStarter{
		source:      &fakeSlackInvocationSource{},
		client:      client,
		renderer:    runtimemodel.SlackThreadRenderer{Client: client},
		adapter:     &fakePromptAdapter{},
		projectRepo: store.Projects(),
		runtimeRepo: store.Runtime(),
	}
	starter.debugf = func(format string, args ...any) {
		debugMu.Lock()
		defer debugMu.Unlock()
		_, _ = debug.WriteString(fmt.Sprintf(format, args...) + "\n")
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- starter.Start(ctx, runtimemodel.Status{})
	}()

	waitForCondition(t, 2*time.Second, func() bool {
		executionState, err := store.Runtime().LoadExecutionStateByDelivery(context.Background(), "mention", "Ev-recover")
		if err != nil {
			return false
		}
		queuedSlashState, err := store.Runtime().LoadExecutionStateByDelivery(context.Background(), "slash", "3-recover-queued")
		if err != nil {
			return false
		}
		return executionState.Status == runtimemodel.ExecutionStatusFailed &&
			executionState.CompletedAt != nil &&
			queuedSlashState.Status == runtimemodel.ExecutionStatusFailed &&
			queuedSlashState.CompletedAt != nil &&
			len(client.snapshotMessages()) == 1
	})
	cancel()

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context canceled", err)
	}

	executionState, err := store.Runtime().LoadExecutionStateByDelivery(context.Background(), "mention", "Ev-recover")
	if err != nil {
		t.Fatalf("LoadExecutionStateByDelivery() error = %v", err)
	}
	if executionState.Status != runtimemodel.ExecutionStatusFailed || executionState.CompletedAt == nil {
		t.Fatalf("LoadExecutionStateByDelivery() = %#v, want failed terminal execution", executionState)
	}
	if executionState.LastError != runtimemodel.StartupRecoveryFailureReason {
		t.Fatalf("LoadExecutionStateByDelivery() last error = %q, want startup recovery reason", executionState.LastError)
	}

	queuedSlashState, err := store.Runtime().LoadExecutionStateByDelivery(context.Background(), "slash", "3-recover-queued")
	if err != nil {
		t.Fatalf("LoadExecutionStateByDelivery(queued slash) error = %v", err)
	}
	if queuedSlashState.Status != runtimemodel.ExecutionStatusFailed || queuedSlashState.CompletedAt == nil {
		t.Fatalf("LoadExecutionStateByDelivery(queued slash) = %#v, want failed terminal execution", queuedSlashState)
	}
	if queuedSlashState.LastError != runtimemodel.StartupRecoveryFailureReason {
		t.Fatalf("LoadExecutionStateByDelivery(queued slash) last error = %q, want startup recovery reason", queuedSlashState.LastError)
	}

	recoveredThreadState, err := store.Runtime().LoadThreadState(context.Background(), threadTS)
	if err != nil {
		t.Fatalf("LoadThreadState() error = %v", err)
	}
	if recoveredThreadState.LastStatus != runtimemodel.ExecutionStatusFailed {
		t.Fatalf("LoadThreadState() last status = %q, want failed", recoveredThreadState.LastStatus)
	}

	dedupe, err := store.Runtime().LoadEventDedupe(context.Background(), "mention", "Ev-recover")
	if err != nil {
		t.Fatalf("LoadEventDedupe() error = %v", err)
	}
	if dedupe.Status != runtimemodel.ExecutionStatusFailed || dedupe.ProcessedAt == nil {
		t.Fatalf("LoadEventDedupe() = %#v, want failed dedupe with processedAt", dedupe)
	}

	queuedSlashDedupe, err := store.Runtime().LoadEventDedupe(context.Background(), "slash", "3-recover-queued")
	if err != nil {
		t.Fatalf("LoadEventDedupe(queued slash) error = %v", err)
	}
	if queuedSlashDedupe.Status != runtimemodel.ExecutionStatusFailed || queuedSlashDedupe.ProcessedAt == nil {
		t.Fatalf("LoadEventDedupe(queued slash) = %#v, want failed dedupe with processedAt", queuedSlashDedupe)
	}

	if _, err := store.Runtime().LoadThreadLock(context.Background(), threadTS); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("LoadThreadLock() error = %v, want not found", err)
	}

	messages := client.snapshotMessages()
	if got, want := len(messages), 1; got != want {
		t.Fatalf("slack message count = %d, want %d", got, want)
	}
	if messages[0].Text != "Session error: "+runtimemodel.StartupRecoveryFailureReason {
		t.Fatalf("recovery slack message = %q", messages[0].Text)
	}

	debugMu.Lock()
	output := debug.String()
	debugMu.Unlock()
	for _, fragment := range []string{
		"runtime.loop: lifecycle event=execution_reconciled source=mention delivery_id=Ev-recover channel_id=C123 project=alpha status=failed execution_id=mention:Ev-recover previous_status=running recovered_status=failed slack_notified=true",
		"runtime.loop: lifecycle event=execution_reconciled source=slash delivery_id=3-recover-queued channel_id=C123 project=alpha status=failed execution_id=slash:3-recover-queued previous_status=queued recovered_status=failed",
	} {
		if !strings.Contains(output, fragment) {
			t.Fatalf("debug output missing %q: %s", fragment, output)
		}
	}
}

// Test: cancelled ACPX prompt streams persist cancelled execution state instead of being overwritten as processed.
// Validates: AC-1981 (REQ-1429 - cancelled lifecycle status is persisted), AC-1981 (REQ-1430 - publisher checkpoints and cancellation details are retained)
func TestForegroundRuntimeStarterPersistsCancelledExecutionState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	project := registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(home, "workspace", "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C123",
	}
	if err := os.MkdirAll(project.LocalPath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := store.Projects().Upsert(ctx, project); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	stream := &controlledPromptStream{
		sessionName: acpxadapter.SessionName("1713686400.000100"),
		events:      make(chan acpxadapter.Event, 4),
		waitCh:      make(chan error, 1),
	}
	client := &recordingSlackClient{}
	adapter := &fakePromptAdapter{
		startPrompt: func(context.Context, acpxadapter.SessionRequest) (acpxadapter.PromptStream, error) {
			return stream, nil
		},
	}

	starter := &foregroundRuntimeStarter{
		source: &fakeSlackInvocationSource{
			invocations: []slack.InboundInvocation{{
				SourceType:  slack.InboundSourceMention,
				DeliveryID:  "Ev-cancel",
				ChannelID:   project.SlackChannelID,
				ThreadTS:    "1713686400.000100",
				UserID:      "U123",
					CommandText: "<@Ubot> ask status",
			}},
		},
		client:      client,
		renderer:    runtimemodel.SlackThreadRenderer{Client: client},
		adapter:     adapter,
		projectRepo: store.Projects(),
		runtimeRepo: store.Runtime(),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- starter.Start(ctx, runtimemodel.Status{})
	}()

	stream.events <- acpxadapter.Event{Kind: acpxadapter.EventAssistantMessageChunk, Text: "working"}
	stream.events <- acpxadapter.Event{Kind: acpxadapter.EventSessionCancelled, Text: "cancelled by operator"}
	close(stream.events)
	stream.waitCh <- nil

	waitForCondition(t, 2*time.Second, func() bool {
		executionState, err := store.Runtime().LoadExecutionStateByDelivery(context.Background(), "mention", "Ev-cancel")
		return err == nil && executionState.Status == runtimemodel.ExecutionStatusCancelled
	})
	cancel()

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context canceled", err)
	}

	executionState, err := store.Runtime().LoadExecutionStateByDelivery(context.Background(), "mention", "Ev-cancel")
	if err != nil {
		t.Fatalf("LoadExecutionStateByDelivery() error = %v", err)
	}
	if executionState.Status != runtimemodel.ExecutionStatusCancelled || executionState.CancelledAt == nil || executionState.CompletedAt == nil {
		t.Fatalf("LoadExecutionStateByDelivery() = %#v, want cancelled execution", executionState)
	}
	if executionState.LastError != "cancelled by operator" {
		t.Fatalf("LoadExecutionStateByDelivery() last error = %q, want cancellation reason", executionState.LastError)
	}
	if executionState.PublisherCheckpointKind != string(runtimemodel.ACPXEventSessionCancelled) {
		t.Fatalf("LoadExecutionStateByDelivery() checkpoint kind = %q, want %q", executionState.PublisherCheckpointKind, runtimemodel.ACPXEventSessionCancelled)
	}

	threadState, err := store.Runtime().LoadThreadState(context.Background(), "1713686400.000100")
	if err != nil {
		t.Fatalf("LoadThreadState() error = %v", err)
	}
	if threadState.LastStatus != "cancelled" {
		t.Fatalf("LoadThreadState() last status = %q, want cancelled", threadState.LastStatus)
	}

	dedupe, err := store.Runtime().LoadEventDedupe(context.Background(), "mention", "Ev-cancel")
	if err != nil {
		t.Fatalf("LoadEventDedupe() error = %v", err)
	}
	if dedupe.Status != "cancelled" || dedupe.ProcessedAt == nil {
		t.Fatalf("LoadEventDedupe() = %#v, want cancelled dedupe", dedupe)
	}

	messages := client.snapshotMessages()
	if got, want := len(messages), 2; got != want {
		t.Fatalf("slack message count = %d, want %d", got, want)
	}
	if messages[1].Text != "Session cancelled: cancelled by operator" {
		t.Fatalf("terminal message = %q, want cancellation notice", messages[1].Text)
	}
}

// Test: duplicate slash deliveries reuse the persisted slash dedupe key and do not create a second execution thread.
// Validates: AC-1826 (REQ-1193 - slash invocations are classified as source=slash), AC-1826 (REQ-1199 - duplicate slash deliveries execute only once), AC-1824 (REQ-1198 - slash lifecycle logs record source and status)
func TestForegroundRuntimeStarterSkipsDuplicateSlashDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	home := t.TempDir()
	t.Setenv("HOME", home)

	store, err := storage.OpenDefault(context.Background())
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	project := registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(home, "workspace", "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C123",
	}
	if err := os.MkdirAll(project.LocalPath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := store.Projects().Upsert(context.Background(), project); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	client := &recordingSlackClient{}
	adapter := &fakePromptAdapter{
		results: []acpxadapter.SessionResult{
			{
				SessionName: acpxadapter.SessionName("1713686400.000100"),
				Output: `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"019db13d-f733-7ce0-8186-5aced7cdb2a7","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"deduped slash result"}}}}
{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`,
			},
		},
	}

	var debug bytes.Buffer
	var debugMu sync.Mutex
	starter := &foregroundRuntimeStarter{
		source: &fakeSlackInvocationSource{
			invocations: []slack.InboundInvocation{
				{
					SourceType:    slack.InboundSourceSlash,
					DeliveryID:    "3-fwdc4",
					ChannelID:     project.SlackChannelID,
					UserID:        "U123",
					CommandText:   "status",
					ResponseURL:   "https://hooks.slack.test/response",
					AckEnvelopeID: "3-fwdc4",
					Acked:         true,
				},
				{
					SourceType:    slack.InboundSourceSlash,
					DeliveryID:    "3-fwdc4",
					ChannelID:     project.SlackChannelID,
					UserID:        "U123",
					CommandText:   "status",
					ResponseURL:   "https://hooks.slack.test/response",
					AckEnvelopeID: "3-fwdc4",
					Acked:         true,
				},
			},
		},
		client:      client,
		renderer:    runtimemodel.SlackThreadRenderer{Client: client},
		adapter:     adapter,
		projectRepo: store.Projects(),
		runtimeRepo: store.Runtime(),
	}
	starter.debugf = func(format string, args ...any) {
		debugMu.Lock()
		defer debugMu.Unlock()
		_, _ = debug.WriteString(fmt.Sprintf(format, args...) + "\n")
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- starter.Start(ctx, runtimemodel.Status{})
	}()

	waitForCondition(t, 2*time.Second, func() bool {
		debugMu.Lock()
		output := debug.String()
		debugMu.Unlock()
		messages := client.snapshotMessages()
		return strings.Contains(output, "runtime.loop: duplicate slash skipped delivery=3-fwdc4") &&
			len(messages) == 2 &&
			messages[1].Text == "deduped slash result"
	})
	cancel()

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context canceled", err)
	}

	if got, want := len(adapter.calls), 1; got != want {
		t.Fatalf("adapter call count = %d, want %d", got, want)
	}

	messages := client.snapshotMessages()
	if got, want := len(messages), 2; got != want {
		t.Fatalf("slack message count = %d, want %d", got, want)
	}
	if messages[0].Text != slashStartText("status") {
		t.Fatalf("slash start message text = %q, want %q", messages[0].Text, slashStartText("status"))
	}
	if messages[1].Text != "deduped slash result" {
		t.Fatalf("slash result message text = %q, want %q", messages[1].Text, "deduped slash result")
	}

	dedupe, err := store.Runtime().LoadEventDedupe(context.Background(), "slash", "3-fwdc4")
	if err != nil {
		t.Fatalf("LoadEventDedupe() error = %v", err)
	}
	if dedupe.Status != "processed" || dedupe.ProcessedAt == nil {
		t.Fatalf("LoadEventDedupe() = %#v, want processed slash delivery", dedupe)
	}

	debugMu.Lock()
	output := debug.String()
	debugMu.Unlock()
	for _, fragment := range []string{
		"runtime.loop: lifecycle event=ack_sent source=slash delivery_id=3-fwdc4 channel_id=C123 project=alpha status=acked",
		"runtime.loop: lifecycle event=execution_started source=slash delivery_id=3-fwdc4 channel_id=C123 project=alpha status=processing",
		"runtime.loop: lifecycle event=execution_completed source=slash delivery_id=3-fwdc4 channel_id=C123 project=alpha status=processed",
		"runtime.loop: lifecycle event=duplicate_skipped source=slash delivery_id=3-fwdc4 channel_id=C123 project=alpha status=duplicate",
	} {
		if !strings.Contains(output, fragment) {
			t.Fatalf("debug output missing %q: %s", fragment, output)
		}
	}
}

// Test: runtime reload re-reads the current SQLite registry snapshot without requiring a process restart.
// Validates: AC-1784 (REQ-1145 - runtime applies valid registry changes without restart) and AC-1782 (REQ-1143 - runtime loads project registry from SQLite)
func TestRuntimeReloadReflectsUpdatedStorage(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	workspacePath := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspacePath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	cfgStore := config.NewFileStore("")
	if _, err := cfgStore.SetBaseWorkspacePath(ctx, workspacePath); err != nil {
		t.Fatalf("SetBaseWorkspacePath() error = %v", err)
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	if err := store.Projects().Upsert(ctx, registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(workspacePath, "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C12345678",
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	var out bytes.Buffer
	handler := &runtimeCommandHandler{
		configStore: cfgStore,
		out:         &out,
	}

	if err := handler.Status(ctx, nil); err != nil {
		t.Fatalf("Status() error = %v", err)
	}

	var status runtimemodel.Status
	if err := json.Unmarshal(out.Bytes(), &status); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; output=%s", err, out.String())
	}
	if status.ProjectCount != 1 {
		t.Fatalf("status project count = %d, want 1", status.ProjectCount)
	}
	if len(status.Projects) != 1 || status.Projects[0].Name != "alpha" {
		t.Fatalf("status projects = %#v, want cached alpha snapshot", status.Projects)
	}

	out.Reset()
	if err := store.Projects().Upsert(ctx, registry.Project{
		Name:             "beta",
		LocalPath:        filepath.Join(workspacePath, "beta"),
		SlackChannelName: "spexus-beta",
		SlackChannelID:   "C87654321",
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	if err := handler.Status(ctx, nil); err != nil {
		t.Fatalf("Status() error after registry update = %v", err)
	}

	var cached runtimemodel.Status
	if err := json.Unmarshal(out.Bytes(), &cached); err != nil {
		t.Fatalf("json.Unmarshal() cached status error = %v; output=%s", err, out.String())
	}
	if cached.ProjectCount != 1 {
		t.Fatalf("cached status project count = %d, want 1", cached.ProjectCount)
	}
	if len(cached.Projects) != 1 || cached.Projects[0].Name != "alpha" {
		t.Fatalf("cached status projects = %#v, want alpha only", cached.Projects)
	}

	out.Reset()
	if err := handler.Reload(ctx, nil); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}

	var reload runtimemodel.ReloadReport
	if err := json.Unmarshal(out.Bytes(), &reload); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; output=%s", err, out.String())
	}
	if reload.ReloadedAt.IsZero() {
		t.Fatalf("ReloadedAt is zero")
	}
	if reload.Status.ProjectCount != 2 {
		t.Fatalf("reload project count = %d, want 2", reload.Status.ProjectCount)
	}
	if len(reload.Status.Projects) != 2 || reload.Status.Projects[0].Name != "alpha" || reload.Status.Projects[1].Name != "beta" {
		t.Fatalf("reload projects = %#v, want deterministic alpha/beta snapshot", reload.Status.Projects)
	}
}

// Test: invalid config reload keeps the last known good runtime snapshot instead of corrupting the active state.
// Validates: AC-1785 (REQ-1146 - runtime preserves last valid active state on invalid reload), AC-1784 (REQ-1145 - runtime applies valid config changes without restart)
func TestRuntimeReloadPreservesStateOnInvalidConfigChange(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	workspacePath := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspacePath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	cfgStore := config.NewFileStore("")
	if _, err := cfgStore.SetBaseWorkspacePath(ctx, workspacePath); err != nil {
		t.Fatalf("SetBaseWorkspacePath() error = %v", err)
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	if err := store.Projects().Upsert(ctx, registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(workspacePath, "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C12345678",
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	var out bytes.Buffer
	handler := &runtimeCommandHandler{
		configStore: cfgStore,
		out:         &out,
	}

	if err := handler.Status(ctx, nil); err != nil {
		t.Fatalf("Status() error = %v", err)
	}

	var statusBefore runtimemodel.Status
	if err := json.Unmarshal(out.Bytes(), &statusBefore); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; output=%s", err, out.String())
	}

	cfgPath, err := config.DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath() error = %v", err)
	}
	if err := os.WriteFile(cfgPath, []byte(`{"runtime":{"reloadMode":"auto","logLevel":"info"}}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	out.Reset()
	if err := handler.Reload(ctx, nil); err == nil {
		t.Fatalf("Reload() error = nil, want invalid config failure")
	}

	out.Reset()
	if err := handler.Status(ctx, nil); err != nil {
		t.Fatalf("Status() error after invalid reload = %v", err)
	}

	var statusAfter runtimemodel.Status
	if err := json.Unmarshal(out.Bytes(), &statusAfter); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; output=%s", err, out.String())
	}

	if statusAfter.ProjectCount != statusBefore.ProjectCount {
		t.Fatalf("status project count = %d, want preserved %d", statusAfter.ProjectCount, statusBefore.ProjectCount)
	}
	if statusAfter.Config.BaseWorkspacePath != workspacePath {
		t.Fatalf("status config base path = %q, want preserved %q", statusAfter.Config.BaseWorkspacePath, workspacePath)
	}
	if len(statusAfter.Projects) != 1 || statusAfter.Projects[0].Name != "alpha" {
		t.Fatalf("status projects = %#v, want preserved alpha snapshot", statusAfter.Projects)
	}
}

// Test: canceling an inactive thread returns an explicit no-op status and does not invoke ACPX.
// Validates: AC-1795 (REQ-1156 - cancel requests cancellation without corrupting the thread/session mapping)
func TestRuntimeCancelReturnsNoOpForInactiveThread(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	workspacePath := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspacePath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	cfgStore := config.NewFileStore("")
	if _, err := cfgStore.SetBaseWorkspacePath(ctx, workspacePath); err != nil {
		t.Fatalf("SetBaseWorkspacePath() error = %v", err)
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	if err := store.Projects().Upsert(ctx, registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(workspacePath, "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C12345678",
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	var out bytes.Buffer
	cancelAdapter := &fakeRuntimeCancelAdapter{}
	handler := &runtimeCommandHandler{
		configStore: cfgStore,
		adapter:     cancelAdapter,
		out:         &out,
	}

	if err := handler.Cancel(ctx, []string{"1713686400.000100"}); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}

	var report runtimemodel.CancelReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; output=%s", err, out.String())
	}
	if !report.NoOp || report.Result != "no-op" {
		t.Fatalf("cancel report = %#v, want no-op", report)
	}
	if report.ThreadTS != "1713686400.000100" {
		t.Fatalf("cancel report thread ts = %q, want thread timestamp", report.ThreadTS)
	}
	if len(cancelAdapter.cancelCalls) != 0 {
		t.Fatalf("Cancel() invoked ACPX for inactive thread: %#v", cancelAdapter.cancelCalls)
	}
}

// Test: canceling an active thread interrupts ACPX, preserves the thread/session mapping, and updates runtime status.
// Validates: AC-1795 (REQ-1156 - cancel requests cancellation without corrupting the thread/session mapping)
func TestRuntimeCancelCancelsActiveThreadWithoutCorruptingMapping(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	workspacePath := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspacePath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	cfgStore := config.NewFileStore("")
	if _, err := cfgStore.SetBaseWorkspacePath(ctx, workspacePath); err != nil {
		t.Fatalf("SetBaseWorkspacePath() error = %v", err)
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	if err := store.Projects().Upsert(ctx, registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(workspacePath, "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C12345678",
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	threadTS := "1713686400.000100"
	state := runtimemodel.ThreadState{
		ThreadTS:      threadTS,
		ChannelID:     "C12345678",
		ProjectName:   "alpha",
		SessionName:   "slack-1713686400.000100",
		LastStatus:    "processing",
		LastRequestID: "Ev123",
	}
	if err := store.Runtime().SaveThreadState(ctx, state); err != nil {
		t.Fatalf("SaveThreadState() error = %v", err)
	}
	queuedAt := time.Now().UTC().Add(-2 * time.Minute)
	startedAt := time.Now().UTC().Add(-time.Minute)
	if err := store.Runtime().SaveExecutionState(ctx, runtimemodel.ExecutionState{
		ExecutionID: "mention:Ev123",
		SourceType:  "mention",
		DeliveryID:  "Ev123",
		ProjectName: "alpha",
		ChannelID:   "C12345678",
		ThreadTS:    threadTS,
		SessionName: state.SessionName,
		Status:      runtimemodel.ExecutionStatusRunning,
		QueuedAt:    queuedAt,
		StartedAt:   &startedAt,
		UpdatedAt:   startedAt,
	}); err != nil {
		t.Fatalf("SaveExecutionState() error = %v", err)
	}
	lockedAt := time.Now().UTC()
	lockExpires := lockedAt.Add(5 * time.Minute)
	if err := store.Runtime().SaveThreadLock(ctx, runtimemodel.ThreadLock{
		ThreadTS:       threadTS,
		LockOwner:      "runtime-1",
		LockedAt:       lockedAt,
		LeaseExpiresAt: &lockExpires,
	}); err != nil {
		t.Fatalf("SaveThreadLock() error = %v", err)
	}

	var out bytes.Buffer
	cancelAdapter := &fakeRuntimeCancelAdapter{}
	handler := &runtimeCommandHandler{
		configStore: cfgStore,
		adapter:     cancelAdapter,
		out:         &out,
	}

	if err := handler.Cancel(ctx, []string{threadTS}); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}

	var report runtimemodel.CancelReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; output=%s", err, out.String())
	}
	if report.NoOp || report.Result != "cancelled" {
		t.Fatalf("cancel report = %#v, want cancelled", report)
	}
	if report.SessionName != state.SessionName {
		t.Fatalf("cancel report session name = %q, want %q", report.SessionName, state.SessionName)
	}
	if len(cancelAdapter.cancelCalls) != 1 || cancelAdapter.cancelCalls[0] != threadTS {
		t.Fatalf("Cancel() calls = %#v, want one call for %s", cancelAdapter.cancelCalls, threadTS)
	}

	loadedState, err := store.Runtime().LoadThreadState(ctx, threadTS)
	if err != nil {
		t.Fatalf("LoadThreadState() error = %v", err)
	}
	if loadedState.LastStatus != "cancelled" {
		t.Fatalf("LoadThreadState() last status = %q, want cancelled", loadedState.LastStatus)
	}
	if loadedState.SessionName != state.SessionName || loadedState.LastRequestID != state.LastRequestID {
		t.Fatalf("LoadThreadState() = %#v, want preserved mapping %#v", loadedState, state)
	}
	loadedExecutionState, err := store.Runtime().LoadExecutionStateByDelivery(ctx, "mention", "Ev123")
	if err != nil {
		t.Fatalf("LoadExecutionStateByDelivery() error = %v", err)
	}
	if loadedExecutionState.Status != runtimemodel.ExecutionStatusCancelled || loadedExecutionState.CancelledAt == nil {
		t.Fatalf("LoadExecutionStateByDelivery() = %#v, want cancelled execution", loadedExecutionState)
	}
	if loadedExecutionState.LastError != "cancelled by operator" {
		t.Fatalf("LoadExecutionStateByDelivery() last error = %q, want cancellation reason", loadedExecutionState.LastError)
	}
	execution, err := store.Runtime().LoadExecution(ctx, "mention:Ev123")
	if err != nil {
		t.Fatalf("LoadExecution() error = %v", err)
	}
	if execution.CompletedAt == nil {
		t.Fatalf("execution completed_at = nil, want terminal timestamp")
	}
	if execution.DiagnosticContext != "cancelled by operator" {
		t.Fatalf("execution diagnostic context = %q, want cancelled by operator", execution.DiagnosticContext)
	}
}

// Test: thread-based cancel still cancels the live running execution even when the thread summary has already advanced to queued.
// Validates: thread cancel resolves the active running execution from persisted executions rather than trusting the thread summary alone
func TestRuntimeCancelCancelsRunningExecutionWhenThreadSummaryIsQueued(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	workspacePath := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspacePath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	cfgStore := config.NewFileStore("")
	if _, err := cfgStore.SetBaseWorkspacePath(ctx, workspacePath); err != nil {
		t.Fatalf("SetBaseWorkspacePath() error = %v", err)
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	if err := store.Projects().Upsert(ctx, registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(workspacePath, "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C12345678",
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	threadTS := "1713686400.000101"
	state := runtimemodel.ThreadState{
		ThreadTS:      threadTS,
		ChannelID:     "C12345678",
		ProjectName:   "alpha",
		SessionName:   "slack-1713686400.000101",
		LastStatus:    "queued",
		LastRequestID: "Ev-queued-after-running",
	}
	if err := store.Runtime().SaveThreadState(ctx, state); err != nil {
		t.Fatalf("SaveThreadState() error = %v", err)
	}
	lockedAt := time.Now().UTC()
	lockExpires := lockedAt.Add(5 * time.Minute)
	if err := store.Runtime().SaveThreadLock(ctx, runtimemodel.ThreadLock{
		ThreadTS:       threadTS,
		LockOwner:      "runtime-1",
		LockedAt:       lockedAt,
		LeaseExpiresAt: &lockExpires,
	}); err != nil {
		t.Fatalf("SaveThreadLock() error = %v", err)
	}
	if err := store.Runtime().CreateExecution(ctx, runtimemodel.ExecutionRequest{
		ExecutionID: "exec-cancel-queued-summary",
		SourceType:  "message",
		DeliveryID:  "Ev-running-before-queued",
		ChannelID:   state.ChannelID,
		ProjectName: state.ProjectName,
		SessionKey:  state.SessionName,
		ThreadTS:    threadTS,
		CommandText: "long running prompt",
		CreatedAt:   lockedAt.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("CreateExecution() error = %v", err)
	}
	startedAt := lockedAt.Add(-30 * time.Second)
	if err := store.Runtime().UpdateExecutionState(ctx, runtimemodel.ExecutionState{
		ExecutionID: "exec-cancel-queued-summary",
		Status:      runtimemodel.ExecutionStateRunning,
		StartedAt:   &startedAt,
		UpdatedAt:   startedAt,
	}); err != nil {
		t.Fatalf("UpdateExecutionState(running) error = %v", err)
	}

	var out bytes.Buffer
	cancelAdapter := &fakeRuntimeCancelAdapter{}
	handler := &runtimeCommandHandler{
		configStore: cfgStore,
		adapter:     cancelAdapter,
		out:         &out,
	}

	if err := handler.Cancel(ctx, []string{threadTS}); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}

	var report runtimemodel.CancelReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; output=%s", err, out.String())
	}
	if report.NoOp || report.Result != "cancelled" {
		t.Fatalf("cancel report = %#v, want cancelled", report)
	}
	if len(cancelAdapter.cancelCalls) != 1 || cancelAdapter.cancelCalls[0] != threadTS {
		t.Fatalf("Cancel() calls = %#v, want one call for %s", cancelAdapter.cancelCalls, threadTS)
	}

	execution, err := store.Runtime().LoadExecution(ctx, "exec-cancel-queued-summary")
	if err != nil {
		t.Fatalf("LoadExecution() error = %v", err)
	}
	if execution.Status != runtimemodel.ExecutionStateCancelled {
		t.Fatalf("execution status = %q, want cancelled", execution.Status)
	}

	loadedState, err := store.Runtime().LoadThreadState(ctx, threadTS)
	if err != nil {
		t.Fatalf("LoadThreadState() error = %v", err)
	}
	if loadedState.LastStatus != "cancelled" {
		t.Fatalf("LoadThreadState() last status = %q, want cancelled", loadedState.LastStatus)
	}
	if _, err := store.Runtime().LoadThreadLock(ctx, threadTS); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("LoadThreadLock() error = %v, want not found", err)
	}
}

// Test: threaded app_mention cancel bypasses the prompt queue, calls local runtime cancel, and posts the cancel result back to Slack.
// Validates: thread mention cancel uses the local runtime cancel surface instead of ACPX prompt routing
func TestForegroundRuntimeStarterCancelsActiveThreadFromMentionCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	home := t.TempDir()
	t.Setenv("HOME", home)

	workspacePath := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspacePath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	cfgStore := config.NewFileStore("")
	if _, err := cfgStore.SetBaseWorkspacePath(ctx, workspacePath); err != nil {
		t.Fatalf("SetBaseWorkspacePath() error = %v", err)
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	project := registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(workspacePath, "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C12345678",
	}
	if err := os.MkdirAll(project.LocalPath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := store.Projects().Upsert(ctx, project); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	threadTS := "1713686400.000100"
	state := runtimemodel.ThreadState{
		ThreadTS:      threadTS,
		ChannelID:     project.SlackChannelID,
		ProjectName:   project.Name,
		SessionName:   "slack-1713686400.000100",
		LastStatus:    "processing",
		LastRequestID: "Ev-running",
	}
	if err := store.Runtime().SaveThreadState(ctx, state); err != nil {
		t.Fatalf("SaveThreadState() error = %v", err)
	}
	lockedAt := time.Now().UTC()
	lockExpires := lockedAt.Add(5 * time.Minute)
	if err := store.Runtime().SaveThreadLock(ctx, runtimemodel.ThreadLock{
		ThreadTS:       threadTS,
		LockOwner:      "runtime-1",
		LockedAt:       lockedAt,
		LeaseExpiresAt: &lockExpires,
	}); err != nil {
		t.Fatalf("SaveThreadLock() error = %v", err)
	}
	if err := store.Runtime().CreateExecution(ctx, runtimemodel.ExecutionRequest{
		ExecutionID: "exec-mention-cancel-1",
		SourceType:  "message",
		DeliveryID:  state.LastRequestID,
		ChannelID:   state.ChannelID,
		ProjectName: state.ProjectName,
		SessionKey:  state.SessionName,
		ThreadTS:    threadTS,
		CommandText: "long running prompt",
		CreatedAt:   lockedAt.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("CreateExecution() error = %v", err)
	}
	startedAt := lockedAt.Add(-30 * time.Second)
	if err := store.Runtime().UpdateExecutionState(ctx, runtimemodel.ExecutionState{
		ExecutionID: "exec-mention-cancel-1",
		Status:      runtimemodel.ExecutionStateRunning,
		StartedAt:   &startedAt,
		UpdatedAt:   startedAt,
	}); err != nil {
		t.Fatalf("UpdateExecutionState(running) error = %v", err)
	}

	client := &recordingSlackClient{}
	cancelAdapter := &fakeRuntimeCancelAdapter{}
	var debug bytes.Buffer
	starter := &foregroundRuntimeStarter{
		source: &fakeSlackInvocationSource{
			invocations: []slack.InboundInvocation{
				{
					SourceType:  slack.InboundSourceMention,
					DeliveryID:  "Ev-mention-cancel",
					ChannelID:   project.SlackChannelID,
					ThreadTS:    threadTS,
					UserID:      "U123",
					CommandText: "<@Ubot> cancel",
				},
			},
		},
		client:      client,
		renderer:    runtimemodel.SlackThreadRenderer{Client: client},
		adapter:     cancelAdapter,
		projectRepo: store.Projects(),
		runtimeRepo: store.Runtime(),
	}
	starter.debugf = func(format string, args ...any) {
		_, _ = debug.WriteString(fmt.Sprintf(format, args...) + "\n")
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- starter.Start(ctx, runtimemodel.Status{})
	}()

	waitForCondition(t, time.Second, func() bool {
		messages := client.snapshotMessages()
		return len(messages) == 1 && messages[0].Text == "active ACPX execution cancelled"
	})
	cancel()

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context canceled", err)
	}
	if len(cancelAdapter.cancelCalls) != 1 || cancelAdapter.cancelCalls[0] != threadTS {
		t.Fatalf("Cancel() calls = %#v, want one cancel call for %s", cancelAdapter.cancelCalls, threadTS)
	}
	if len(client.snapshotMessages()) != 1 {
		t.Fatalf("slack messages = %#v, want one cancel response", client.snapshotMessages())
	}
	verifyCtx := context.Background()
	execution, err := store.Runtime().LoadExecution(verifyCtx, "exec-mention-cancel-1")
	if err != nil {
		t.Fatalf("LoadExecution() error = %v", err)
	}
	if execution.Status != runtimemodel.ExecutionStateCancelled {
		t.Fatalf("execution status = %q, want cancelled", execution.Status)
	}
	if _, err := store.Runtime().LoadThreadLock(verifyCtx, threadTS); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("LoadThreadLock() error = %v, want not found", err)
	}
	if !strings.Contains(debug.String(), "rendered mention cancel thread=1713686400.000100 session=slack-1713686400.000100 result=cancelled") {
		t.Fatalf("debug output missing mention cancel trace: %s", debug.String())
	}
}

// Test: cancelling a running execution by execution id resolves the active owner from persisted state and persists the terminal cancelled outcome.
// Validates: AC-2116 (REQ-1586 - cancel routes to the active execution owner), AC-2116 (REQ-1587 - successful cancellation persists the terminal outcome)
func TestRuntimeCancelRoutesRunningExecutionByExecutionID(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	workspacePath := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspacePath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	cfgStore := config.NewFileStore("")
	if _, err := cfgStore.SetBaseWorkspacePath(ctx, workspacePath); err != nil {
		t.Fatalf("SetBaseWorkspacePath() error = %v", err)
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	if err := store.Projects().Upsert(ctx, registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(workspacePath, "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C12345678",
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	threadTS := "1713686400.000200"
	state := runtimemodel.ThreadState{
		ThreadTS:      threadTS,
		ChannelID:     "C12345678",
		ProjectName:   "alpha",
		SessionName:   "slack-1713686400.000200",
		LastStatus:    "processing",
		LastRequestID: "Ev124",
	}
	if err := store.Runtime().SaveThreadState(ctx, state); err != nil {
		t.Fatalf("SaveThreadState() error = %v", err)
	}
	lockedAt := time.Now().UTC()
	lockExpires := lockedAt.Add(5 * time.Minute)
	if err := store.Runtime().SaveThreadLock(ctx, runtimemodel.ThreadLock{
		ThreadTS:       threadTS,
		LockOwner:      "runtime-2",
		LockedAt:       lockedAt,
		LeaseExpiresAt: &lockExpires,
	}); err != nil {
		t.Fatalf("SaveThreadLock() error = %v", err)
	}
	if err := store.Runtime().CreateExecution(ctx, runtimemodel.ExecutionRequest{
		ExecutionID: "exec-cancel-by-id",
		SourceType:  "message",
		DeliveryID:  state.LastRequestID,
		ChannelID:   state.ChannelID,
		ProjectName: state.ProjectName,
		SessionKey:  state.SessionName,
		ThreadTS:    threadTS,
		CommandText: "cancel via execution id",
		CreatedAt:   lockedAt.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("CreateExecution() error = %v", err)
	}
	startedAt := lockedAt.Add(-30 * time.Second)
	if err := store.Runtime().UpdateExecutionState(ctx, runtimemodel.ExecutionState{
		ExecutionID: "exec-cancel-by-id",
		Status:      runtimemodel.ExecutionStateRunning,
		StartedAt:   &startedAt,
		UpdatedAt:   startedAt,
	}); err != nil {
		t.Fatalf("UpdateExecutionState(running) error = %v", err)
	}

	var out bytes.Buffer
	cancelAdapter := &fakeRuntimeCancelAdapter{}
	handler := &runtimeCommandHandler{
		configStore: cfgStore,
		adapter:     cancelAdapter,
		out:         &out,
	}

	if err := handler.Cancel(ctx, []string{"exec-cancel-by-id"}); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}

	var report runtimemodel.CancelReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; output=%s", err, out.String())
	}
	if report.NoOp || report.Result != "cancelled" {
		t.Fatalf("cancel report = %#v, want cancelled", report)
	}
	if report.ThreadTS != threadTS {
		t.Fatalf("cancel report thread ts = %q, want %q", report.ThreadTS, threadTS)
	}
	if report.SessionName != state.SessionName {
		t.Fatalf("cancel report session name = %q, want %q", report.SessionName, state.SessionName)
	}
	if len(cancelAdapter.cancelCalls) != 1 || cancelAdapter.cancelCalls[0] != threadTS {
		t.Fatalf("Cancel() calls = %#v, want one call for %s", cancelAdapter.cancelCalls, threadTS)
	}

	execution, err := store.Runtime().LoadExecution(ctx, "exec-cancel-by-id")
	if err != nil {
		t.Fatalf("LoadExecution() error = %v", err)
	}
	if execution.Status != runtimemodel.ExecutionStateCancelled {
		t.Fatalf("execution status = %q, want cancelled", execution.Status)
	}
	if execution.CompletedAt == nil {
		t.Fatalf("execution completed_at = nil, want terminal timestamp")
	}
	if execution.DiagnosticContext != "cancelled by operator" {
		t.Fatalf("execution diagnostic context = %q, want cancelled by operator", execution.DiagnosticContext)
	}
	if _, err := store.Runtime().LoadThreadLock(ctx, threadTS); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("LoadThreadLock() error = %v, want not found", err)
	}
}

// Test: cancel failures do not mutate the persisted thread/session mapping or cached runtime status snapshot.
// Validates: AC-1795 (REQ-1156 - cancel requests cancellation without corrupting the thread/session mapping)
func TestRuntimeCancelPreservesStateOnAdapterError(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	workspacePath := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspacePath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	cfgStore := config.NewFileStore("")
	if _, err := cfgStore.SetBaseWorkspacePath(ctx, workspacePath); err != nil {
		t.Fatalf("SetBaseWorkspacePath() error = %v", err)
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	if err := store.Projects().Upsert(ctx, registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(workspacePath, "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C12345678",
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	threadTS := "1713686400.000100"
	state := runtimemodel.ThreadState{
		ThreadTS:      threadTS,
		ChannelID:     "C12345678",
		ProjectName:   "alpha",
		SessionName:   "slack-1713686400.000100",
		LastStatus:    "processing",
		LastRequestID: "Ev123",
	}
	if err := store.Runtime().SaveThreadState(ctx, state); err != nil {
		t.Fatalf("SaveThreadState() error = %v", err)
	}
	lockedAt := time.Now().UTC()
	lockExpires := lockedAt.Add(5 * time.Minute)
	if err := store.Runtime().SaveThreadLock(ctx, runtimemodel.ThreadLock{
		ThreadTS:       threadTS,
		LockOwner:      "runtime-1",
		LockedAt:       lockedAt,
		LeaseExpiresAt: &lockExpires,
	}); err != nil {
		t.Fatalf("SaveThreadLock() error = %v", err)
	}

	var out bytes.Buffer
	cancelAdapter := &fakeRuntimeCancelAdapter{cancelErr: errors.New("boom")}
	handler := &runtimeCommandHandler{
		configStore: cfgStore,
		adapter:     cancelAdapter,
		out:         &out,
	}

	if err := handler.Status(ctx, nil); err != nil {
		t.Fatalf("Status() error = %v", err)
	}

	var statusBefore runtimemodel.Status
	if err := json.Unmarshal(out.Bytes(), &statusBefore); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; output=%s", err, out.String())
	}

	out.Reset()
	if err := handler.Cancel(ctx, []string{threadTS}); err == nil {
		t.Fatalf("Cancel() error = nil, want adapter failure")
	}

	out.Reset()
	if err := handler.Status(ctx, nil); err != nil {
		t.Fatalf("Status() error after cancel failure = %v", err)
	}

	var statusAfter runtimemodel.Status
	if err := json.Unmarshal(out.Bytes(), &statusAfter); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; output=%s", err, out.String())
	}
	if statusAfter.ProjectCount != statusBefore.ProjectCount {
		t.Fatalf("status project count = %d, want %d", statusAfter.ProjectCount, statusBefore.ProjectCount)
	}
	if len(cancelAdapter.cancelCalls) != 1 || cancelAdapter.cancelCalls[0] != threadTS {
		t.Fatalf("Cancel() calls = %#v, want one call for %s", cancelAdapter.cancelCalls, threadTS)
	}

	loadedState, err := store.Runtime().LoadThreadState(ctx, threadTS)
	if err != nil {
		t.Fatalf("LoadThreadState() error = %v", err)
	}
	if loadedState.LastStatus != "processing" {
		t.Fatalf("LoadThreadState() last status = %q, want processing", loadedState.LastStatus)
	}
	if _, err := store.Runtime().LoadThreadLock(ctx, threadTS); err != nil {
		t.Fatalf("LoadThreadLock() error = %v, want lock to remain", err)
	}
}

// Test: invalid registry reload keeps the last known good runtime snapshot instead of adopting a corrupt SQLite file.
// Validates: AC-1785 (REQ-1146 - runtime preserves last valid active state on invalid reload), AC-1784 (REQ-1145 - runtime applies valid registry changes without restart)
func TestRuntimeReloadPreservesStateOnInvalidRegistryChange(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	workspacePath := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspacePath, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	cfgStore := config.NewFileStore("")
	if _, err := cfgStore.SetBaseWorkspacePath(ctx, workspacePath); err != nil {
		t.Fatalf("SetBaseWorkspacePath() error = %v", err)
	}

	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatalf("OpenDefault() error = %v", err)
	}
	if err := store.Projects().Upsert(ctx, registry.Project{
		Name:             "alpha",
		LocalPath:        filepath.Join(workspacePath, "alpha"),
		SlackChannelName: "spexus-alpha",
		SlackChannelID:   "C12345678",
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	var out bytes.Buffer
	handler := &runtimeCommandHandler{
		configStore: cfgStore,
		out:         &out,
	}

	if err := handler.Status(ctx, nil); err != nil {
		t.Fatalf("Status() error = %v", err)
	}

	var statusBefore runtimemodel.Status
	if err := json.Unmarshal(out.Bytes(), &statusBefore); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; output=%s", err, out.String())
	}

	storagePath, err := storage.DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath() error = %v", err)
	}
	if err := os.WriteFile(storagePath, []byte("corrupt sqlite payload"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	out.Reset()
	if err := handler.Reload(ctx, nil); err == nil {
		t.Fatalf("Reload() error = nil, want corrupt registry failure")
	}

	out.Reset()
	if err := handler.Status(ctx, nil); err != nil {
		t.Fatalf("Status() error after invalid registry reload = %v", err)
	}

	var statusAfter runtimemodel.Status
	if err := json.Unmarshal(out.Bytes(), &statusAfter); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; output=%s", err, out.String())
	}

	if statusAfter.ProjectCount != statusBefore.ProjectCount {
		t.Fatalf("status project count = %d, want preserved %d", statusAfter.ProjectCount, statusBefore.ProjectCount)
	}
	if len(statusAfter.Projects) != 1 || statusAfter.Projects[0].Name != "alpha" {
		t.Fatalf("status projects = %#v, want preserved alpha snapshot", statusAfter.Projects)
	}
}

// Test: invalid runtime config is reported by doctor and startup status still fails before storage initialization.
// Validates: AC-1783 (REQ-1144 - runtime initializes storage if absent) and AC-1786 (REQ-1147 - invalid initial runtime configuration fails startup)
func TestRuntimeDoctorReportsInvalidConfigAndStatusFailsBeforeStorageCreation(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfgPath, err := config.DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(cfgPath, []byte(`{"runtime":{"reloadMode":"auto","logLevel":"info"}}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var out bytes.Buffer
	handler := &runtimeCommandHandler{
		configStore: config.NewFileStore(""),
		out:         &out,
	}

	report, err := handler.loadDoctorReport(ctx)
	if err != nil {
		t.Fatalf("loadDoctorReport() error = %v", err)
	}
	if report.Healthy {
		t.Fatalf("Doctor report is healthy, want unhealthy: %#v", report)
	}
	if len(report.Checks) == 0 {
		t.Fatalf("Doctor report checks are empty: %#v", report)
	}

	out.Reset()
	if err := handler.Status(ctx, nil); err == nil {
		t.Fatalf("Status() error = nil, want invalid-config failure")
	}

	storagePath, err := storage.DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath() error = %v", err)
	}
	if _, statErr := os.Stat(storagePath); !os.IsNotExist(statErr) {
		t.Fatalf("storage.sqlite3 exists after invalid startup attempt: %v", statErr)
	}

	if !strings.Contains(report.Checks[0].Name, "config") {
		t.Fatalf("Doctor report config check missing: %#v", report.Checks)
	}
}
