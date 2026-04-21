package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

type fakeSlackEventSource struct {
	events []slack.Event
}

func (s *fakeSlackEventSource) Events(ctx context.Context) (<-chan slack.Event, error) {
	out := make(chan slack.Event, len(s.events))
	for _, event := range s.events {
		out <- event
	}
	go func() {
		<-ctx.Done()
		close(out)
	}()
	return out, nil
}

func (s *fakeSlackEventSource) Close() error { return nil }

type recordingSlackClient struct {
	messages []slack.Message
}

func (c *recordingSlackClient) PostThreadMessage(_ context.Context, message slack.Message) error {
	c.messages = append(c.messages, message)
	return nil
}

func (c *recordingSlackClient) CreateChannel(context.Context, slack.CreateChannelRequest) (slack.Channel, error) {
	return slack.Channel{}, nil
}

func (c *recordingSlackClient) FindChannelByName(context.Context, string) (slack.Channel, error) {
	return slack.Channel{}, nil
}

func (c *recordingSlackClient) Close() error { return nil }

type fakePromptAdapter struct {
	results []acpxadapter.SessionResult
	calls   []acpxadapter.SessionRequest
}

func (a *fakePromptAdapter) EnsureSession(context.Context, acpxadapter.SessionRequest) (acpxadapter.SessionResult, error) {
	return acpxadapter.SessionResult{}, nil
}

func (a *fakePromptAdapter) SendPrompt(_ context.Context, req acpxadapter.SessionRequest) (acpxadapter.SessionResult, error) {
	a.calls = append(a.calls, req)
	if len(a.results) == 0 {
		return acpxadapter.SessionResult{SessionName: acpxadapter.SessionName(req.ThreadTS)}, nil
	}
	result := a.results[0]
	a.results = a.results[1:]
	return result, nil
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

type recordingRuntimeStarterFunc func(context.Context, runtimemodel.Status) error

func (fn recordingRuntimeStarterFunc) Start(ctx context.Context, status runtimemodel.Status) error {
	return fn(ctx, status)
}

// Test: the foreground runtime loop consumes Slack events, dispatches them through ACPX, renders the reply, and emits debug trace lines.
// Validates: AC-1797 (REQ-1157 - runtime start executes a foreground loop), AC-1798 (REQ-1159 - startup state is loaded before event processing), AC-1801 (REQ-1162 - operator-visible foreground lifecycle and tracing)
func TestForegroundRuntimeStarterProcessesSlackEventAndLogsDebugTrace(t *testing.T) {
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
		source: &fakeSlackEventSource{
			events: []slack.Event{
				{
					ID:        "Ev123",
					ChannelID: project.SlackChannelID,
					Timestamp: "1713686400.000100",
					UserID:    "U123",
					Text:      "hello from slack",
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
		if strings.Contains(line, "event processed id=Ev123") {
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
	if adapter.calls[0].Prompt != "hello from slack" || adapter.calls[0].ProjectPath != project.LocalPath {
		t.Fatalf("adapter call = %#v, want prompt and local path propagated", adapter.calls[0])
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
		"runtime.loop: received slack event id=Ev123",
		"runtime.loop: dispatching event id=Ev123 project=alpha session=slack-1713686400.000100",
		"runtime.loop: acpx output session=slack-1713686400.000100 payload=",
		"runtime.loop: rendered slack reply thread=1713686400.000100 session=slack-1713686400.000100",
		"runtime.loop: event processed id=Ev123 session=slack-1713686400.000100",
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
