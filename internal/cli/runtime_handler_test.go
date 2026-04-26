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

type recordingSlackClient struct {
	mu                  sync.Mutex
	messages            []slack.Message
	responseURLMessages []slack.ResponseURLMessage
}

func (c *recordingSlackClient) PostMessage(_ context.Context, message slack.Message) (slack.PostedMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.messages = append(c.messages, message)
	return slack.PostedMessage{
		ChannelID: message.ChannelID,
		Timestamp: "1713686400.000100",
	}, nil
}

func (c *recordingSlackClient) PostThreadMessage(_ context.Context, message slack.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.messages = append(c.messages, message)
	return nil
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
	results    []acpxadapter.SessionResult
	calls      []acpxadapter.SessionRequest
	sendPrompt func(context.Context, acpxadapter.SessionRequest) (acpxadapter.SessionResult, error)
}

func (a *fakePromptAdapter) EnsureSession(context.Context, acpxadapter.SessionRequest) (acpxadapter.SessionResult, error) {
	return acpxadapter.SessionResult{}, nil
}

func (a *fakePromptAdapter) SendPrompt(ctx context.Context, req acpxadapter.SessionRequest) (acpxadapter.SessionResult, error) {
	a.calls = append(a.calls, req)
	if a.sendPrompt != nil {
		return a.sendPrompt(ctx, req)
	}
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
					CommandText: "<@Ubot> status",
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
		"runtime.loop: acpx output session=slack-1713686400.000100 payload=",
		"runtime.loop: rendered slack reply thread=1713686400.000100 session=slack-1713686400.000100",
		"runtime.loop: mention processed delivery=Ev123 session=slack-1713686400.000100",
	} {
		if !strings.Contains(output, fragment) {
			t.Fatalf("debug output missing %q: %s", fragment, output)
		}
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
					CommandText: "<@Ubot> status",
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

// Test: slash invocations are acknowledged before long-running ACPX work and publish the final result asynchronously into a new execution thread.
// Validates: AC-1818 (REQ-1186 - slash invocations acknowledge within the Slack window), AC-1819 (REQ-1188 - long-running slash commands publish their final result asynchronously), AC-1819 (REQ-1195 - slash commands use channel-level execution rules)
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
	adapter := &fakePromptAdapter{
		results: []acpxadapter.SessionResult{
			{
				SessionName: acpxadapter.SessionName("1713686400.000100"),
				Output: `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"019db13d-f733-7ce0-8186-5aced7cdb2a7","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"deduped mention result"}}}}
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
					DeliveryID:  "Ev126",
					ChannelID:   project.SlackChannelID,
					ThreadTS:    "1713686400.000100",
					UserID:      "U123",
					CommandText: "<@Ubot> status",
				},
				{
					SourceType:  slack.InboundSourceMention,
					DeliveryID:  "Ev126",
					ChannelID:   project.SlackChannelID,
					ThreadTS:    "1713686400.000100",
					UserID:      "U123",
					CommandText: "<@Ubot> status",
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
		if strings.Contains(line, "runtime.loop: duplicate mention skipped delivery=Ev126") {
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
	if got, want := len(client.messages), 1; got != want {
		t.Fatalf("slack message count = %d, want %d", got, want)
	}

	dedupe, err := store.Runtime().LoadEventDedupe(context.Background(), "mention", "Ev126")
	if err != nil {
		t.Fatalf("LoadEventDedupe() error = %v", err)
	}
	if dedupe.Status != "processed" || dedupe.ProcessedAt == nil {
		t.Fatalf("LoadEventDedupe() = %#v, want processed mention delivery", dedupe)
	}

	output := debug.String()
	for _, fragment := range []string{
		"runtime.loop: lifecycle event=execution_started source=mention delivery_id=Ev126 channel_id=C123 project=alpha status=processing",
		"runtime.loop: lifecycle event=execution_completed source=mention delivery_id=Ev126 channel_id=C123 project=alpha status=processed",
		"runtime.loop: lifecycle event=duplicate_skipped source=mention delivery_id=Ev126 channel_id=C123 project=alpha status=duplicate",
	} {
		if !strings.Contains(output, fragment) {
			t.Fatalf("debug output missing %q: %s", fragment, output)
		}
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
				},
				{
					SourceType:    slack.InboundSourceSlash,
					DeliveryID:    "3-fwdc4",
					ChannelID:     project.SlackChannelID,
					UserID:        "U123",
					CommandText:   "status",
					ResponseURL:   "https://hooks.slack.test/response",
					AckEnvelopeID: "3-fwdc4",
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
