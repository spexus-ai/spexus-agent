package swarmslack

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"github.com/spexus-ai/spexus-agent/internal/slack"
	"github.com/spexus-ai/spexus-agent/internal/swarm"
	"github.com/spexus-ai/spexus-agent/internal/swarmrunner"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The ordinary Go test executable is also a permanent subprocess fixture. It
// runs the real Runner and Pi adapter in separate OS processes without paid API.
func TestMain(m *testing.M) {
	if os.Getenv("SWARM_PROCESS_FIXTURE") != "" {
		if len(os.Args) > 1 && os.Args[1] == "--mode" {
			fixturePi()
			os.Exit(0)
		}
		c, err := swarmrunner.LoadConfig(os.Getenv("SWARM_PROCESS_FIXTURE"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		r, err := swarmrunner.New(c)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
		err = r.Run(ctx)
		ctxErr := ctx.Err()
		stop()
		r.Close()
		if err != nil && ctxErr == nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}
func fixturePi() {
	if os.Getenv("SWARM_HUMAN_MODE") == "1" {
		fixtureHumanPi()
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	if !scanner.Scan() {
		return
	}
	var req struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(scanner.Bytes(), &req) != nil {
		panic("prompt JSON")
	}
	input := req.Message[strings.LastIndex(req.Message, "\n")+1:]
	var v struct {
		Task    *swarm.DispatchPayload `json:"task"`
		Event   swarm.Delivery         `json:"event"`
		Workers []struct {
			AgentID string        `json:"agent_id"`
			Profile swarm.Profile `json:"profile"`
		} `json:"available_workers"`
	}
	if json.Unmarshal([]byte(input), &v) != nil {
		panic("input JSON")
	}
	var output any
	if v.Task != nil {
		output = map[string]any{"outcome": "succeeded", "summary": v.Task.Context.Text + " = 42", "evidence": []swarm.Evidence{{Kind: "text", Label: "calculation", ContentOrRef: v.Task.Context.Text + ": 6 * 7 = 42"}}, "error": nil}
	} else if v.Event.Type == "agent.input" {
		actions := []any{}
		for i, w := range v.Workers {
			actions = append(actions, map[string]any{"kind": "dispatch", "data": map[string]any{"worker_agent_id": w.AgentID, "goal": "Calculate 6 * 7", "scope": "text only", "expected_result": []string{"42 with own context marker"}, "context": swarm.TaskContext{Text: fmt.Sprintf("PRIVATE-%d", i), Refs: []swarm.ContextRef{}}, "profile": w.Profile}})
		}
		output = map[string]any{"actions": actions, "reply": "Two tasks dispatched; waiting."}
	} else {
		session := ""
		for i, arg := range os.Args {
			if arg == "--session" && i+1 < len(os.Args) {
				session = os.Args[i+1]
			}
		}
		var prior []string
		b, _ := os.ReadFile(session + ".fixture-results")
		_ = json.Unmarshal(b, &prior)
		var result swarm.ResultPayload
		_ = json.Unmarshal(v.Event.Payload, &result)
		prior = append(prior, result.Summary)
		b, _ = json.Marshal(prior)
		_ = os.WriteFile(session+".fixture-results", b, 0600)
		reply := "One result received; waiting."
		if len(prior) == 2 {
			reply = "Combined: " + strings.Join(prior, "; ")
		}
		output = map[string]any{"actions": []any{map[string]any{"kind": "review", "data": map[string]any{"job_id": v.Event.JobID, "attempt_id": v.Event.AttemptID, "result_message_id": v.Event.MessageID, "verdict": "accepted", "reason": "Checked 6 * 7 = 42", "evidence": []swarm.Evidence{}}}}, "reply": reply}
	}
	raw, _ := json.Marshal(output)
	enc := json.NewEncoder(os.Stdout)
	_ = enc.Encode(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "content": []any{map[string]string{"type": "text", "text": string(raw)}}, "stopReason": "stop"}})
	_ = enc.Encode(map[string]any{"type": "agent_settled"})
}

type recordedSlack struct {
	mu      sync.Mutex
	posts   []swarm.SlackDelivery
	unknown bool
}

func (s *recordedSlack) Post(_ context.Context, d swarm.SlackDelivery) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.posts = append(s.posts, d)
	if s.unknown {
		return "", fmt.Errorf("lost response")
	}
	return "posted-" + d.ID, nil
}
func (s *recordedSlack) Find(_ context.Context, d swarm.SlackDelivery) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.posts {
		if p.ID == d.ID {
			return "posted-" + d.ID, true, nil
		}
	}
	return "", false, nil
}
func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}
func eventually(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", what)
}
func TestRealRunnerProcessesTwoWorkersAndDurableSlack(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ids := []string{swarm.NewID(), swarm.NewID(), swarm.NewID()}
	instances := []string{swarm.NewID(), swarm.NewID(), swarm.NewID()}
	tokens := []string{"test-owner-token", "test-worker-a-token", "test-worker-b-token"}
	cfg := swarm.Config{TenantID: swarm.NewID(), ProjectID: swarm.NewID()}
	profiles := make([]string, 3)
	for i := range ids {
		role := "worker"
		if i == 0 {
			role = "owner"
		}
		p := swarm.TextProfile{ID: role + fmt.Sprint(i), Model: "fixture/test", Reasoning: "minimal", Prompt: "Text-only integration fixture", Tools: []string{}, Extensions: []string{}}
		b, _ := json.Marshal(p)
		profiles[i] = filepath.Join(dir, fmt.Sprintf("profile%d.json", i))
		if err := os.WriteFile(profiles[i], b, 0600); err != nil {
			t.Fatal(err)
		}
		cfg.Profiles = append(cfg.Profiles, swarm.ProfileSnapshot{Bytes: b})
		cfg.Agents = append(cfg.Agents, swarm.AgentConfig{AgentID: ids[i], Role: role, CredentialSHA256: swarm.Digest([]byte(tokens[i])), ProfileID: p.ID})
	}
	f := swarm.Feature{FeatureID: swarm.NewID(), TenantID: cfg.TenantID, ProjectID: cfg.ProjectID, OwnerAgentID: ids[0], ChannelID: "C-TEST", ThreadTS: "123.456", AllowedActorIDs: []string{"U-TEST"}}
	cfg.Features = []swarm.Feature{f}
	store, err := swarm.Open(ctx, filepath.Join(dir, "coordinator.db"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	server := httptest.NewTLSServer(store.Handler())
	t.Cleanup(server.Close)
	ca := filepath.Join(dir, "ca.crt")
	if err = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	commands := make([]*exec.Cmd, 3)
	exited := make([]chan struct{}, 3)
	states := make([]string, 3)
	for i := range ids {
		private := filepath.Join(dir, fmt.Sprintf("agent%d", i))
		if err = os.MkdirAll(private, 0700); err != nil {
			t.Fatal(err)
		}
		tokenFile := filepath.Join(private, "token")
		_ = os.WriteFile(tokenFile, []byte(tokens[i]), 0600)
		states[i] = filepath.Join(private, "state")
		c := swarmrunner.Config{CoordinatorURL: server.URL, CAFile: ca, CredentialFile: tokenFile, TenantID: cfg.TenantID, ProjectID: cfg.ProjectID, AgentID: ids[i], InstanceID: instances[i], Role: cfg.Agents[i].Role, ProfileFile: profiles[i], StateDirectory: states[i], Workspace: private, PiBinary: binary, Targets: []swarmrunner.Target{}}
		if i == 0 {
			c.Targets = []swarmrunner.Target{{AgentID: ids[1], ProfileFile: profiles[1]}, {AgentID: ids[2], ProfileFile: profiles[2]}}
		}
		configPath := filepath.Join(private, "config.json")
		writeJSON(t, configPath, c)
		logFile, err := os.Create(filepath.Join(private, "process.log"))
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binary, "-test.run=^$")
		cmd.Env = append(os.Environ(), "SWARM_PROCESS_FIXTURE="+configPath)
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		commands[i] = cmd
		done := make(chan struct{})
		exited[i] = done
		go func() { _ = cmd.Wait(); close(done) }()
		t.Cleanup(func() {
			_ = cmd.Process.Signal(syscall.SIGTERM)

			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
			logFile.Close()
			if t.Failed() {
				b, _ := os.ReadFile(logFile.Name())
				t.Logf("runner %s: %s", private, b)
			}
		})
	}
	eventually(t, "three bound agents", func() bool {
		h, e := store.History(ctx, f.FeatureID)
		if e != nil {
			t.Fatalf("initial history: %v", e)
		}
		return e == nil && len(h.Agents) == 3 && h.Agents[0].InstanceID != "" && h.Agents[1].InstanceID != "" && h.Agents[2].InstanceID != ""
	})
	api := &recordedSlack{}
	bridge := &Bridge{Store: store, Features: cfg.Features, API: api}
	event := slack.Event{ID: "Ev-original", ChannelID: f.ChannelID, ThreadTS: f.ThreadTS, Timestamp: "123.457", UserID: "U-TEST", Text: "Delegate two private calculations and combine their results"}
	if err = bridge.Handle(ctx, event); err != nil {
		t.Fatal(err)
	}
	if err = bridge.Handle(ctx, event); err != nil {
		t.Fatal(err)
	}
	var h swarm.History
	eventually(t, "two reviewed results", func() bool {
		h, err = store.History(ctx, f.FeatureID)
		if err != nil || len(h.Jobs) != 2 {
			return false
		}
		for _, j := range h.Jobs {
			if len(j.Attempts) != 1 || j.Attempts[0].State != "succeeded" || j.Attempts[0].Review != "accepted" {
				return false
			}
		}
		return len(h.SlackOutbox) == 3
	})
	for _, j := range h.Jobs {
		a := j.Attempts[0]
		if a.Result == nil || !strings.Contains(a.Result.Summary, "42") {
			t.Fatalf("missing result %+v", a)
		}
		if strings.Contains(a.Result.Summary, "PRIVATE-0") && strings.Contains(a.Result.Summary, "PRIVATE-1") {
			t.Fatal("worker context mixed")
		}
	}
	final := h.SlackOutbox[len(h.SlackOutbox)-1]
	if !strings.Contains(final.Text, "PRIVATE-0 = 42") || !strings.Contains(final.Text, "PRIVATE-1 = 42") {
		t.Fatalf("bad combined reply: %s", final.Text)
	}
	if final.ChannelID != f.ChannelID || final.ThreadTS != f.ThreadTS {
		t.Fatal("reply left source thread")
	}
	// Repost exact dispatch/result IDs across real HTTPS. Receipt duplication must
	// not create any new Pi launch or owner result turn.
	post := func(agent int, method, path string, body any) (int, []byte) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, server.URL+swarm.APIPrefix+path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+tokens[agent])
		req.Header.Set("X-Agent-Instance-ID", instances[agent])
		req.Header.Set("Content-Type", "application/json")
		res, e := server.Client().Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, b
	}
	var savedResult swarm.Envelope
	for _, m := range h.Messages {
		if m.Type != "task.dispatch" && m.Type != "task.result" {
			continue
		}
		sender := 0
		for i, id := range ids {
			if m.FromAgentID == id {
				sender = i
			}
		}
		status, body := post(sender, http.MethodPost, "/messages", m.Envelope)
		if status != 200 {
			t.Fatalf("duplicate %s: %d %s", m.Type, status, body)
		}
		if m.Type == "task.result" {
			savedResult = m.Envelope
		}
	}
	foreign := savedResult
	foreign.MessageID = swarm.NewID()
	foreign.FromAgentID = ids[0]
	status, _ := post(0, http.MethodPost, "/messages", foreign)
	if status < 400 {
		t.Fatal("owner impersonated worker result")
	}
	stale := savedResult
	stale.MessageID = swarm.NewID()
	stale.AttemptID = swarm.NewID()
	sender := 1
	if stale.FromAgentID == ids[2] {
		sender = 2
	}
	status, _ = post(sender, http.MethodPost, "/messages", stale)
	if status < 400 {
		t.Fatal("foreign attempt accepted")
	}
	after, err := store.History(ctx, f.FeatureID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Jobs) != 2 || len(after.Turns) != 3 || len(after.SlackOutbox) != 3 {
		t.Fatal("duplicate/foreign input mutated business work")
	}
	// Slack accepted publication but its response was lost. Correlation lookup
	// settles the existing record without posting a second message.
	api.unknown = true
	if bridge.DeliverOne(ctx) == nil {
		t.Fatal("expected unknown delivery")
	}
	if err = bridge.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	api.unknown = false
	for i := 0; i < 3; i++ {
		if err = bridge.DeliverOne(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(api.posts) != 3 {
		t.Fatalf("Slack duplicate: %d posts", len(api.posts))
	}
	// Stop processes before inspecting their persistent journals.
	for _, cmd := range commands {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	eventually(t, "runner processes stopped", func() bool {
		for _, done := range exited {
			select {
			case <-done:
			default:
				return false
			}
		}
		return true
	})
	eventually(t, "all launches durably recorded", func() bool {
		for i, state := range states {
			jh, e := swarmrunner.ReadHistory(state)
			if e != nil {
				return false
			}
			n := 0
			for _, input := range jh.Inputs {
				n += input.Launches
			}
			want := 1
			if i == 0 {
				want = 3
			}
			if n != want {
				return false
			}
		}
		return true
	})

	// A new attempt of a completed job is now current. An old worker result with
	// a fresh message ID is rejected, and a stopped worker expires explicitly.
	receipt, _, err := store.Ingest(ctx, f.FeatureID, swarm.InputPayload{Text: "retry after operator inspection", Source: swarm.Source{Kind: "slack", EventID: "Ev-retry", ChannelID: f.ChannelID, ThreadTS: f.ThreadTS, ActorID: "U-TEST"}})
	if err != nil {
		t.Fatal(err)
	}
	turnID := swarm.NewID()
	status, body := post(0, http.MethodPost, "/owner-turns/start", swarm.OwnerStartRequest{TurnID: turnID, FeatureID: f.FeatureID, InputMailboxSeq: receipt.MailboxSeq})
	if status != 201 {
		t.Fatalf("manual owner start: %d %s", status, body)
	}
	var next swarm.Envelope
	for _, m := range h.Messages {
		if m.Type == "task.dispatch" && m.JobID == savedResult.JobID {
			next = m.Envelope
		}
	}
	var payload swarm.DispatchPayload
	_ = json.Unmarshal(next.Payload, &payload)
	payload.AcceptBy = time.Now().UTC().Add(time.Second).Format(time.RFC3339Nano)
	next.Payload, _ = json.Marshal(payload)
	next.MessageID = swarm.NewID()
	next.AttemptID = swarm.NewID()
	next.OwnerTurnID = turnID
	next.SentAt = time.Now().UTC().Format(time.RFC3339Nano)
	status, body = post(0, http.MethodPost, "/messages", next)
	if status != 201 {
		t.Fatalf("new attempt: %d %s", status, body)
	}
	stale = savedResult
	stale.MessageID = swarm.NewID()
	status, body = post(sender, http.MethodPost, "/messages", stale)
	if status != 409 || !bytes.Contains(body, []byte("stale_attempt")) {
		t.Fatalf("stale result: %d %s", status, body)
	}
	eventually(t, "unavailable worker explicit result", func() bool {
		if e := store.Sweep(ctx); e != nil {
			t.Fatal(e)
		}
		current, e := store.History(ctx, f.FeatureID)
		if e != nil {
			t.Fatal(e)
		}
		for _, job := range current.Jobs {
			if job.JobID == next.JobID {
				if job.CurrentAttemptID != next.AttemptID {
					t.Fatal("old result changed current attempt")
				}
				for _, a := range job.Attempts {
					if a.AttemptID == next.AttemptID {
						return a.State == "failed" && a.Error != nil && a.Error.Code == "recipient_unavailable"
					}
				}
			}
		}
		return false
	})
	// Foreign actors and different thread anchors create neither work nor notices.
	before, _ := store.History(ctx, f.FeatureID)
	invalid := event
	invalid.ID = "Ev-foreign"
	invalid.UserID = "other"
	invalid.Text = "!stop"
	if err = bridge.Handle(ctx, invalid); err != nil {
		t.Fatal(err)
	}
	invalid.UserID = "U-TEST"
	invalid.ThreadTS = "other"
	if err = bridge.Handle(ctx, invalid); err != nil {
		t.Fatal(err)
	}
	untouched, _ := store.History(ctx, f.FeatureID)
	if untouched.Feature.Stopped || len(untouched.SlackOutbox) != len(before.SlackOutbox) {
		t.Fatal("foreign Slack event affected feature")
	}
	command := event
	command.ID = "Ev-status"
	command.Text = "!status"
	if err = bridge.Handle(ctx, command); err != nil {
		t.Fatal(err)
	}
	if err = bridge.Handle(ctx, command); err != nil {
		t.Fatal(err)
	}
	command.ID = "Ev-stop"
	command.Text = "!stop"
	if err = bridge.Handle(ctx, command); err != nil {
		t.Fatal(err)
	}
	command.ID = "Ev-continue"
	command.Text = "!continue"
	if err = bridge.Handle(ctx, command); err == nil {
		t.Fatal("continue silently resumed unresolved owner turn")
	}
	notices, _ := store.History(ctx, f.FeatureID)
	if !notices.Feature.Stopped {
		t.Fatal("feature stop not retained")
	}
	foundError := false
	for _, d := range notices.SlackOutbox {
		if strings.HasPrefix(d.Text, "Request rejected:") {
			foundError = true
			if d.ThreadTS != f.ThreadTS {
				t.Fatal("rejection left source thread")
			}
		}
	}
	if !foundError {
		t.Fatal("authorized rejection had no durable Slack notice")
	}

}
