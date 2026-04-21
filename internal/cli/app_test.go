package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

type fakeConfigHandler struct {
	setBaseWorkspaceArgs []string
	loginArgs            []string
	statusArgs           []string
	logoutArgs           []string
}

func (f *fakeConfigHandler) Init(context.Context, []string) error     { return nil }
func (f *fakeConfigHandler) Show(context.Context, []string) error     { return nil }
func (f *fakeConfigHandler) Validate(context.Context, []string) error { return nil }
func (f *fakeConfigHandler) SetBaseWorkspace(_ context.Context, args []string) error {
	f.setBaseWorkspaceArgs = append([]string(nil), args...)
	return nil
}
func (f *fakeConfigHandler) Login(_ context.Context, args []string) error {
	f.loginArgs = append([]string(nil), args...)
	return nil
}
func (f *fakeConfigHandler) Status(_ context.Context, args []string) error {
	f.statusArgs = append([]string(nil), args...)
	return nil
}
func (f *fakeConfigHandler) Logout(_ context.Context, args []string) error {
	f.logoutArgs = append([]string(nil), args...)
	return nil
}

type fakeRuntimeHandler struct {
	startArgs     []string
	startCtx      context.Context
	startWaitDone chan struct{}
	statusArgs    []string
}

func (f *fakeRuntimeHandler) Start(ctx context.Context, args []string) error {
	f.startArgs = append([]string(nil), args...)
	f.startCtx = ctx
	if f.startWaitDone != nil {
		close(f.startWaitDone)
	}
	if f.startWaitDone != nil {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}
func (f *fakeRuntimeHandler) Status(_ context.Context, args []string) error {
	f.statusArgs = append([]string(nil), args...)
	return nil
}
func (f *fakeRuntimeHandler) Reload(context.Context, []string) error { return nil }
func (f *fakeRuntimeHandler) Doctor(context.Context, []string) error { return nil }
func (f *fakeRuntimeHandler) Cancel(context.Context, []string) error { return nil }

// Test the config CLI routes set-base-workspace to the handler.
// Validates: AC-1761 (REQ-1122 - config CLI manages base workspace path)
func TestAppRunConfigSetBaseWorkspaceRoutesHandler(t *testing.T) {
	t.Parallel()

	handler := &fakeConfigHandler{}
	app := &App{
		Config: handler,
		Out:    &bytes.Buffer{},
		Err:    &bytes.Buffer{},
	}

	if code := app.Run([]string{"config", "set-base-workspace", "/workspace"}); code != 0 {
		t.Fatalf("Run() exit code = %d, want 0", code)
	}

	if got, want := strings.Join(handler.setBaseWorkspaceArgs, ","), "/workspace"; got != want {
		t.Fatalf("handler args = %q, want %q", got, want)
	}
}

// Test the config CLI routes Slack auth login to the nested handler.
// Validates: AC-1762 (REQ-1123 - Slack auth is collected through interactive config commands)
func TestAppRunConfigSlackAuthLoginRoutesHandler(t *testing.T) {
	t.Parallel()

	handler := &fakeConfigHandler{}
	app := &App{
		Config: handler,
		Out:    &bytes.Buffer{},
		Err:    &bytes.Buffer{},
	}

	if code := app.Run([]string{"config", "slack-auth", "login"}); code != 0 {
		t.Fatalf("Run() exit code = %d, want 0", code)
	}

	if got, want := strings.Join(handler.loginArgs, ","), ""; got != want {
		t.Fatalf("handler args = %q, want %q", got, want)
	}
}

// Test the config help includes the new subcommand.
// Validates: AC-1762 (REQ-1123 - config init/show/validate/set-base-workspace/slack-auth command surface exists)
func TestConfigHelpIncludesSetBaseWorkspace(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	printConfigHelp(&out)

	if !strings.Contains(out.String(), "set-base-workspace") {
		t.Fatalf("config help missing set-base-workspace: %s", out.String())
	}
	if !strings.Contains(out.String(), "slack-auth") {
		t.Fatalf("config help missing slack-auth: %s", out.String())
	}
}

// Test the bare runtime command routes to the status handler so startup health is checked immediately.
// Validates: AC-1780 (REQ-1141 - runtime operates as a Linux user-space process) and AC-1781 (REQ-1142 - runtime loads config at startup)
func TestAppRunRuntimeRoutesBareCommandToStatus(t *testing.T) {
	t.Parallel()

	handler := &fakeRuntimeHandler{}
	app := &App{
		Runtime: handler,
		Out:     &bytes.Buffer{},
		Err:     &bytes.Buffer{},
	}

	if code := app.Run([]string{"runtime"}); code != 0 {
		t.Fatalf("Run() exit code = %d, want 0", code)
	}

	if got, want := strings.Join(handler.statusArgs, ","), ""; got != want {
		t.Fatalf("handler args = %q, want %q", got, want)
	}
}

// Test the runtime CLI routes the explicit start subcommand to the handler.
// Validates: AC-1796 (REQ-1158 - runtime start runs as a regular user-level process), AC-1797 (REQ-1157 - runtime provides an explicit start command)
func TestAppRunRuntimeStartRoutesHandler(t *testing.T) {
	t.Parallel()

	handler := &fakeRuntimeHandler{}
	app := &App{
		Runtime: handler,
		Out:     &bytes.Buffer{},
		Err:     &bytes.Buffer{},
	}

	if code := app.Run([]string{"runtime", "start"}); code != 0 {
		t.Fatalf("Run() exit code = %d, want 0", code)
	}

	if got, want := strings.Join(handler.startArgs, ","), ""; got != want {
		t.Fatalf("handler args = %q, want %q", got, want)
	}
}

// Test the runtime CLI forwards the debug flag to the runtime start handler.
// Validates: AC-1797 (REQ-1157 - runtime provides an explicit start command)
func TestAppRunRuntimeStartRoutesDebugFlag(t *testing.T) {
	t.Parallel()

	handler := &fakeRuntimeHandler{}
	app := &App{
		Runtime: handler,
		Out:     &bytes.Buffer{},
		Err:     &bytes.Buffer{},
	}

	if code := app.Run([]string{"runtime", "start", "--debug"}); code != 0 {
		t.Fatalf("Run() exit code = %d, want 0", code)
	}

	if got, want := strings.Join(handler.startArgs, ","), "--debug"; got != want {
		t.Fatalf("handler args = %q, want %q", got, want)
	}
}

// Test the runtime start path treats SIGTERM-style cancellation as a clean service shutdown.
// Validates: AC-1800 (REQ-1161 - runtime start is suitable as an ExecStart target for user-level service managers), AC-1801 (REQ-1162 - runtime lifecycle surface includes a stop-compatible termination path)
func TestAppRunRuntimeStartExitsCleanlyOnShutdownSignal(t *testing.T) {
	t.Parallel()

	originalSignalNotifyContext := signalNotifyContext
	defer func() {
		signalNotifyContext = originalSignalNotifyContext
	}()

	ctx, cancel := context.WithCancel(context.Background())
	signalNotifyContext = func(context.Context, ...os.Signal) (context.Context, context.CancelFunc) {
		return ctx, cancel
	}

	handler := &fakeRuntimeHandler{
		startWaitDone: make(chan struct{}),
	}
	app := &App{
		Runtime: handler,
		Out:     &bytes.Buffer{},
		Err:     &bytes.Buffer{},
	}

	done := make(chan int, 1)
	go func() {
		done <- app.Run([]string{"runtime", "start"})
	}()

	<-handler.startWaitDone
	cancel()

	if code := <-done; code != 0 {
		t.Fatalf("Run() exit code = %d, want 0", code)
	}
	if handler.startCtx == nil {
		t.Fatalf("Start() did not receive a context")
	}
	if handler.startCtx.Err() == nil {
		t.Fatalf("Start() context was not cancelled after shutdown")
	}
}

// Test the runtime help includes the daemon lifecycle command surface.
// Validates: AC-1801 (REQ-1162 - runtime lifecycle surface includes start, status, reload, and stop-compatible termination)
func TestRuntimeHelpIncludesLifecycleCommands(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	printRuntimeHelp(&out)

	for _, command := range []string{"start [--debug]", "status", "reload", "cancel <thread-ts>"} {
		if !strings.Contains(out.String(), command) {
			t.Fatalf("runtime help missing %q: %s", command, out.String())
		}
	}
}
