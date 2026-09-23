package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/config"
	"github.com/spexus-ai/spexus-agent/internal/piadapter"
	"github.com/spexus-ai/spexus-agent/internal/registry"
	runtimemodel "github.com/spexus-ai/spexus-agent/internal/runtime"
	"github.com/spexus-ai/spexus-agent/internal/slack"
	"github.com/spexus-ai/spexus-agent/internal/storage"
	"github.com/spexus-ai/spexus-agent/internal/testsupport"
)

type liveInvocationSource struct{ events chan slack.InboundInvocation }

func (s *liveInvocationSource) InboundInvocations(context.Context) (<-chan slack.InboundInvocation, error) {
	return s.events, nil
}
func (s *liveInvocationSource) Close() error { return nil }

func TestSlackRuntimeWithRealPi(t *testing.T) {
	m := testsupport.NewPiModel(t)
	t.Setenv("SPEXUS_AGENT_HOME", filepath.Join(m.Profile.Workspace, "runtime"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cfg := config.DefaultGlobalConfig()
	cfg.BaseWorkspacePath = m.Profile.Workspace
	cfg.Agent = m.Profile
	cfg.Slack = config.SlackAuth{BotToken: "test", AppToken: "test", WorkspaceID: "T1"}
	if err := config.NewFileStore("").Save(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := storage.OpenDefault(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Projects().Upsert(ctx, registry.Project{Name: "test", LocalPath: m.Profile.Workspace, SlackChannelID: "C1", SlackChannelName: "test"}); err != nil {
		t.Fatal(err)
	}
	a, err := piadapter.New(m.Profile, m.Binary)
	if err != nil {
		t.Fatal(err)
	}
	source := &liveInvocationSource{events: make(chan slack.InboundInvocation, 16)}
	client := &recordingSlackClient{}
	s := &foregroundRuntimeStarter{source: source, client: client, renderer: runtimemodel.SlackThreadRenderer{Client: client}, adapter: a, projectRepo: store.Projects(), runtimeRepo: store.Runtime()}
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx, runtimemodel.Status{}) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("runtime did not stop")
		}
	}()
	send := func(id, thread, text string) {
		source.events <- slack.InboundInvocation{SourceType: slack.InboundSourceMessage, DeliveryID: id, ChannelID: "C1", ThreadTS: thread, UserID: "U1", CommandText: text}
	}
	waitState := func(id, status string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			state, err := store.Runtime().LoadExecutionStateByDelivery(ctx, slack.InboundSourceMessage, id)
			if err == nil && state.Status == status {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(20 * time.Millisecond):
			}
		}
		t.Fatalf("execution %s did not reach %s; messages=%+v", id, status, client.snapshotMessages())
	}
	send("first", "1", "FIRST_THREAD_SECRET")
	waitState("first", runtimemodel.ExecutionStatusProcessed)
	send("second", "2", "SECOND_THREAD_SECRET")
	waitState("second", runtimemodel.ExecutionStatusProcessed)
	send("first", "1", "FIRST_THREAD_SECRET") // identical Slack event must not invoke Pi again
	send("followup", "1", "continue")
	waitState("followup", runtimemodel.ExecutionStatusProcessed)
	requests := m.Requests()
	if len(requests) != 3 {
		t.Fatalf("duplicate reached model, requests=%d", len(requests))
	}
	if strings.Contains(requests[1], "FIRST_THREAD_SECRET") || strings.Contains(requests[2], "SECOND_THREAD_SECRET") || !strings.Contains(requests[2], "FIRST_THREAD_SECRET") {
		t.Fatal("thread context lost or leaked")
	}
	for _, message := range client.snapshotMessages() {
		if message.ChannelID != "C1" || (message.ThreadTS != "1" && message.ThreadTS != "2") {
			t.Fatalf("misrouted answer: %+v", message)
		}
	}
	send("block", "1", "BLOCK_UNTIL_STOP")
	deadline := time.Now().Add(15 * time.Second)
	for len(m.Requests()) < 4 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(m.Requests()) != 4 {
		t.Fatal("blocking model call did not start")
	}
	send("parallel", "2", "continue while other thread is blocked")
	waitState("parallel", runtimemodel.ExecutionStatusProcessed)
	send("stop", "1", "!stop")
	waitState("block", runtimemodel.ExecutionStatusCancelled)
	send("resumed", "1", "continue after stop")
	waitState("resumed", runtimemodel.ExecutionStatusProcessed)
	send("failure", "2", "PROVIDER_ERROR")
	waitState("failure", runtimemodel.ExecutionStatusFailed)
	foundError := false
	for _, message := range client.snapshotMessages() {
		if message.ThreadTS == "2" && strings.Contains(message.Text, "error") {
			foundError = true
		}
	}
	if !foundError {
		t.Fatal("provider error not visible in originating Slack thread")
	}
	if _, err := os.Stat(m.Profile.SessionDirectory); err != nil {
		t.Fatal("missing persistent Pi session directory", err)
	}
}
