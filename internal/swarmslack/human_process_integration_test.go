package swarmslack

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/slack"
	"github.com/spexus-ai/spexus-agent/internal/swarm"
	"github.com/spexus-ai/spexus-agent/internal/swarmrunner"
)

// fixtureHumanPi is selected by the existing subprocess test entrypoint. Its
// JSON follows the same Pi stream and runner validation path as a live model.
func fixtureHumanPi() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	if !scanner.Scan() {
		return
	}
	var prompt struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(scanner.Bytes(), &prompt) != nil {
		panic("invalid Pi prompt")
	}
	// Wire v2 adds owner guidance after the JSON input. Select the actual
	// machine-readable line instead of assuming it is the final prompt line.
	input := ""
	for _, line := range strings.Split(prompt.Message, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal([]byte(line), &fields) == nil && (fields["task"] != nil || fields["event"] != nil) {
			input = line
			break
		}
	}
	if input == "" {
		panic("missing model input")
	}
	var v struct {
		Task       *swarm.DispatchPayload   `json:"task"`
		Resume     *swarm.ResumeTaskPayload `json:"resume"`
		Event      swarm.Delivery           `json:"event"`
		Dependency *swarm.Dependency        `json:"dependency"`
		Workers    []struct {
			AgentID string        `json:"agent_id"`
			Profile swarm.Profile `json:"profile"`
		} `json:"available_workers"`
	}
	if json.Unmarshal([]byte(input), &v) != nil {
		panic("invalid model input")
	}
	var output any
	if v.Task != nil {
		if v.Task.Context.Text == "A" && v.Resume == nil {
			output = map[string]any{"outcome": "blocked", "summary": "A requires a human choice", "evidence": []any{}, "error": nil,
				"blocker": swarm.Blocker{Reason: "Human choice required", Context: "A cannot select the release path", Question: "Which release path should A use?", Options: []swarm.HumanOption{{ID: "safe", Label: "Safe path"}, {ID: "fast", Label: "Fast path"}}, Recommendation: "safe because it limits scope", Kind: "choice"}}
		} else {
			output = map[string]any{"outcome": "succeeded", "summary": v.Task.Context.Text + " completed", "evidence": []swarm.Evidence{{Kind: "text", Label: "fixture", ContentOrRef: v.Task.Context.Text + " completed"}}, "error": nil}
		}
	} else {
		actions := []any{}
		reply := "Work continues."
		switch v.Event.Type {
		case "agent.input":
			var userInput swarm.InputPayload
			if json.Unmarshal(v.Event.Payload, &userInput) != nil {
				panic("invalid user input")
			}
			if userInput.Text == "Ask deny" || userInput.Text == "Ask cancel" {
				step := "deny-step"
				if userInput.Text == "Ask cancel" {
					step = "cancel-step"
				}
				actions = append(actions, map[string]any{"kind": "request_human", "data": swarm.HumanRequestPayload{StepKey: step, BlockedWork: "Step " + step + " waits for a human", Blocker: swarm.Blocker{Reason: "A human decision is required", Context: "The owner cannot authorize this step", Question: "Approve this step?", Options: []swarm.HumanOption{{ID: "yes", Label: "Approve"}}, Recommendation: "Deny unless explicitly authorized", Kind: "permission"}}})
				reply = "Waiting for a human decision."
				break
			}
			for i, w := range v.Workers {
				label := "A"
				if i == 1 {
					label = "B"
				}
				actions = append(actions, map[string]any{"kind": "dispatch", "data": map[string]any{"worker_agent_id": w.AgentID, "goal": "Complete independent task " + label, "scope": "fixture", "expected_result": []string{"Result " + label}, "context": swarm.TaskContext{Text: label, Refs: []swarm.ContextRef{}}, "profile": w.Profile}})
			}
			reply = "A and B delegated."
		case "task.result":
			var result swarm.ResultPayload
			if json.Unmarshal(v.Event.Payload, &result) != nil {
				panic("invalid result")
			}
			if result.Outcome == "blocked" {
				if v.Dependency == nil || result.Blocker == nil {
					panic("missing trusted dependency")
				}
				actions = append(actions, map[string]any{"kind": "request_human", "data": swarm.HumanRequestPayload{DependencyID: v.Dependency.ID, Blocker: *result.Blocker}})
				reply = "A waits for a human choice; B continues."
			} else {
				actions = append(actions, map[string]any{"kind": "review", "data": map[string]any{"job_id": v.Event.JobID, "attempt_id": v.Event.AttemptID, "result_message_id": v.Event.MessageID, "verdict": "accepted", "reason": "Fixture evidence checked", "evidence": []swarm.Evidence{}}})
				reply = result.Summary
			}
		case "human.decision":
			var decision swarm.HumanDecisionPayload
			if json.Unmarshal(v.Event.Payload, &decision) != nil || decision.ApplicationStatus != "applied" || v.Dependency == nil {
				panic("decision not applied")
			}
			if decision.State == "denied" {
				if v.Dependency.State != "denied" {
					panic("denial not canonical")
				}
				reply = "Step denied; dependent work remains stopped."
				break
			}
			if v.Dependency.State != "resolved" {
				panic("decision not canonical")
			}
			w := v.Workers[0]
			actions = append(actions, map[string]any{"kind": "resume_task", "data": swarm.ResumeTaskPayload{DependencyID: v.Dependency.ID, DecisionID: decision.DecisionID, WorkerAgentID: w.AgentID,
				Dispatch: swarm.DispatchPayload{Goal: "Complete A after approved safe choice", Scope: "fixture", ExpectedResult: []string{"A completed"}, Context: swarm.TaskContext{Text: "A", Refs: []swarm.ContextRef{}}, Profile: w.Profile}}})
			reply = "A continuation requested."
		}
		output = map[string]any{"actions": actions, "reply": reply}
	}
	raw, _ := json.Marshal(output)
	enc := json.NewEncoder(os.Stdout)
	_ = enc.Encode(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "content": []any{map[string]string{"type": "text", "text": string(raw)}}, "stopReason": "stop"}})
	_ = enc.Encode(map[string]any{"type": "agent_settled"})
}

type humanSlackFixture struct {
	mu      sync.Mutex
	posts   []swarm.SlackDelivery
	updates []string
}

func (f *humanSlackFixture) VerifyWorkspace(_ context.Context, configured string) error {
	if configured != "W-P3-INTEGRATION" {
		return fmt.Errorf("wrong test workspace")
	}
	return nil
}

func (f *humanSlackFixture) Post(_ context.Context, d swarm.SlackDelivery) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, d)
	return "1790000000.000100", nil
}
func (f *humanSlackFixture) Find(_ context.Context, d swarm.SlackDelivery) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.posts {
		if p.ID == d.ID {
			return "1790000000.000100", true, nil
		}
	}
	return "", false, nil
}
func (f *humanSlackFixture) UpdateHumanQuestion(_ context.Context, d swarm.SlackDelivery, _ []swarm.HumanOption, state, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d.SlackTS != "1790000000.000100" {
		return fmt.Errorf("updated wrong question")
	}
	if state != "open" {
		f.updates = append(f.updates, d.ID)
	}
	return nil
}
func (f *humanSlackFixture) ScanThread(context.Context, string, string, string, func(string, string, string, string, bool) error) (string, error) {
	return "", nil
}

// Test: worker A blocks, the owner creates a real Spexus request, independent B
// finishes, the coordinator restarts, and a trusted Slack answer resumes A once.
// Validates: SP-AC-463/464/465 and SP-STD-022 §§5-9 (full P3-HR handover).
func TestHumanEndToEndWithActualProvider(t *testing.T) {
	fixturePath := os.Getenv("SPEXUS_HR_PROVIDER_FIXTURE")
	if fixturePath == "" {
		t.Skip("isolated Spexus provider fixture not configured")
	}
	var provider struct {
		URL     string `json:"backend_https_url"`
		CA      string `json:"ca_cert_path"`
		Token   string `json:"token_file_path"`
		Tenant  string `json:"tenant_id"`
		Project string `json:"project_id"`
		Epic    string `json:"epic_id"`
		Writer  string `json:"gateway_writer_id"`
	}
	metadata, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(metadata, &provider); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	dir := t.TempDir()
	ids := []string{swarm.NewID(), swarm.NewID(), swarm.NewID()}
	instances := []string{swarm.NewID(), swarm.NewID(), swarm.NewID()}
	tokens := []string{swarm.NewID(), swarm.NewID(), swarm.NewID()}
	cfg := swarm.Config{TenantID: provider.Tenant, ProjectID: provider.Project, WireVersion: 2, Human: &swarm.HumanConfig{BaseURL: provider.URL, CAFile: provider.CA, TokenFile: provider.Token, EpicID: provider.Epic, WriterID: provider.Writer, WorkspaceID: "W-P3-INTEGRATION"}}
	profiles := make([]string, 3)
	for i := range ids {
		role := "worker"
		if i == 0 {
			role = "owner"
		}
		p := swarm.TextProfile{ID: fmt.Sprintf("%s-%d", role, i), Model: "fixture/test", Reasoning: "minimal", Prompt: "Text-only fixture", Tools: []string{}, Extensions: []string{}}
		b, _ := json.Marshal(p)
		profiles[i] = filepath.Join(dir, fmt.Sprintf("profile-%d.json", i))
		if err = os.WriteFile(profiles[i], b, 0600); err != nil {
			t.Fatal(err)
		}
		cfg.Profiles = append(cfg.Profiles, swarm.ProfileSnapshot{Bytes: b})
		cfg.Agents = append(cfg.Agents, swarm.AgentConfig{AgentID: ids[i], Role: role, CredentialSHA256: swarm.Digest([]byte(tokens[i])), ProfileID: p.ID})
	}
	// The provider keeps Slack source identities across test runs. Give every
	// run its own realistic thread/message timestamps and event IDs so retries
	// never conflict with previously committed PostgreSQL decisions.
	baseMicros := time.Now().UTC().UnixMicro()
	ts := func(offset int64) string {
		micros := baseMicros + offset
		return fmt.Sprintf("%d.%06d", micros/1_000_000, micros%1_000_000)
	}
	feature := swarm.Feature{FeatureID: swarm.NewID(), TenantID: cfg.TenantID, ProjectID: cfg.ProjectID, OwnerAgentID: ids[0], ChannelID: "C-P3-INTEGRATION", ThreadTS: ts(1), AllowedActorIDs: []string{"U-P3-HUMAN"}}
	eventID := func(kind string) string { return "Ev-P3-" + kind + "-" + feature.FeatureID }
	cfg.Features = []swarm.Feature{feature}
	statePath := filepath.Join(dir, "coordinator.db")
	store, err := swarm.Open(ctx, statePath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.HumanPreflight(ctx); err != nil {
		t.Fatalf("isolated Spexus/JWT preflight: %v", err)
	}
	var storeMu sync.RWMutex
	t.Cleanup(func() { storeMu.Lock(); defer storeMu.Unlock(); _ = store.Close() })
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storeMu.RLock()
		defer storeMu.RUnlock()
		store.Handler().ServeHTTP(w, r)
	}))
	defer server.Close()
	caPath := filepath.Join(dir, "coordinator-ca.crt")
	if err = os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	states := make([]string, 3)
	commands := make([]*exec.Cmd, 3)
	stopped := make([]chan struct{}, 3)
	for i := range ids {
		private := filepath.Join(dir, fmt.Sprintf("agent-%d", i))
		if err = os.MkdirAll(private, 0700); err != nil {
			t.Fatal(err)
		}
		credential := filepath.Join(private, "token")
		if err = os.WriteFile(credential, []byte(tokens[i]), 0600); err != nil {
			t.Fatal(err)
		}
		states[i] = filepath.Join(private, "state")
		rc := swarmrunner.Config{WireVersion: 2, CoordinatorURL: server.URL, CAFile: caPath, CredentialFile: credential, TenantID: cfg.TenantID, ProjectID: cfg.ProjectID, AgentID: ids[i], InstanceID: instances[i], Role: cfg.Agents[i].Role, ProfileFile: profiles[i], StateDirectory: states[i], Workspace: private, PiBinary: binary, Targets: []swarmrunner.Target{}}
		if i == 0 {
			rc.Targets = []swarmrunner.Target{{AgentID: ids[1], ProfileFile: profiles[1]}, {AgentID: ids[2], ProfileFile: profiles[2]}}
		}
		configPath := filepath.Join(private, "config.json")
		writeJSON(t, configPath, rc)
		logFile, openErr := os.Create(filepath.Join(private, "process.log"))
		if openErr != nil {
			t.Fatal(openErr)
		}
		cmd := exec.Command(binary, "-test.run=^$")
		cmd.Env = append(os.Environ(), "SWARM_PROCESS_FIXTURE="+configPath, "SWARM_HUMAN_MODE=1")
		cmd.Stdout, cmd.Stderr = logFile, logFile
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		commands[i] = cmd
		done := make(chan struct{})
		stopped[i] = done
		go func(c *exec.Cmd, d chan struct{}) { _ = c.Wait(); close(d) }(cmd, done)
		t.Cleanup(func() {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
			_ = logFile.Close()
			if t.Failed() {
				b, _ := os.ReadFile(logFile.Name())
				t.Logf("runner-%d: %s", i, b)
			}
		})
	}
	api := &humanSlackFixture{}
	bridge := &Bridge{Store: store, Features: cfg.Features, API: api}
	source := &durableFixture{}
	bridgeCtx, bridgeCancel := context.WithCancel(ctx)
	bridgeDone := make(chan error, 1)
	go func() { bridgeDone <- bridge.RunHuman(bridgeCtx, source, cfg.Human.WorkspaceID) }()
	stopBridge := func() {
		bridgeCancel()
		select {
		case err := <-bridgeDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Slack bridge did not stop")
		}
	}
	defer func() {
		if bridgeCancel != nil {
			stopBridge()
		}
	}()
	history := func() swarm.History {
		t.Helper()
		storeMu.RLock()
		defer storeMu.RUnlock()
		h, e := store.History(ctx, feature.FeatureID)
		if e != nil {
			t.Fatal(e)
		}
		return h
	}
	wait := func(what string, condition func(swarm.History) bool, syncProvider bool) swarm.History {
		t.Helper()
		deadline := time.Now().Add(50 * time.Second)
		for time.Now().Before(deadline) {
			if syncProvider {
				storeMu.RLock()
				e := store.SyncHuman(ctx)
				storeMu.RUnlock()
				if e != nil {
					t.Fatalf("%s: Spexus sync: %v", what, e)
				}
			}
			h := history()
			if condition(h) {
				return h
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("timeout: %s: %+v", what, history())
		return swarm.History{}
	}
	wait("three runner bindings and Slack catchup", func(h swarm.History) bool {
		if h.RecoveryBarrier != "" || len(h.Agents) != 3 {
			return false
		}
		for _, a := range h.Agents {
			if a.InstanceID == "" {
				return false
			}
		}
		return true
	}, false)
	start := slack.Event{ID: eventID("start"), WorkspaceID: cfg.Human.WorkspaceID, ChannelID: feature.ChannelID, ThreadTS: feature.ThreadTS, Timestamp: ts(2), UserID: "U-P3-HUMAN", Text: "Run A and independent B"}
	if err = source.Send(ctx, start); err != nil {
		t.Fatal(err)
	}
	h := wait("A blocked, B completed, request persisted", func(h swarm.History) bool {
		if len(h.Jobs) != 2 || len(h.Dependencies) != 1 || len(h.HumanRequests) != 1 {
			return false
		}
		if h.Dependencies[0].State != "human_waiting" || h.HumanRequests[0].BackendState != "open" {
			return false
		}
		a, b := false, false
		for _, j := range h.Jobs {
			if len(j.Attempts) != 1 {
				continue
			}
			if j.Attempts[0].State == "blocked" {
				a = true
			}
			if j.Attempts[0].State == "succeeded" && j.Attempts[0].Review == "accepted" {
				b = true
			}
		}
		return a && b
	}, true)
	dep := h.Dependencies[0]
	if dep.RequestID == "" || dep.JobID == "" {
		t.Fatal("missing durable request/job identity")
	}
	questionCount := 0
	for _, p := range h.SlackOutbox {
		if p.ID == dep.RequestID {
			questionCount++
			if !strings.Contains(p.Text, dep.RequestID) || !strings.Contains(p.Text, "safe") || !strings.Contains(p.Text, "Blocked work") {
				t.Fatalf("incomplete human question: %s", p.Text)
			}
		}
	}
	if questionCount != 1 {
		t.Fatalf("question count=%d", questionCount)
	}
	for _, post := range h.SlackOutbox {
		if post.Text == "B completed" {
			t.Fatal("independent B review published a premature final reply while A waited for a human")
		}
	}
	eventually(t, "human question published once", func() bool {
		api.mu.Lock()
		defer api.mu.Unlock()
		count := 0
		for _, post := range api.posts {
			if post.ID == dep.RequestID {
				count++
			}
		}
		return count == 1
	})
	beforeTurns := len(h.Turns)
	// The waiter is durable: closing/reopening only the coordinator keeps all
	// three runner processes alive and does not create another model attempt.
	stopBridge()
	bridgeCancel = nil
	storeMu.Lock()
	if err = store.Close(); err == nil {
		store, err = swarm.Open(ctx, statePath, cfg)
	}
	storeMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	bridge = &Bridge{Store: store, Features: cfg.Features, API: api}
	source = &durableFixture{}
	bridgeCtx, bridgeCancel = context.WithCancel(ctx)
	bridgeDone = make(chan error, 1)
	go func() { bridgeDone <- bridge.RunHuman(bridgeCtx, source, cfg.Human.WorkspaceID) }()
	h = wait("restored Slack catchup", func(h swarm.History) bool {
		return h.RecoveryBarrier == "" && h.Dependencies[0].State == "human_waiting"
	}, false)
	if len(h.Turns) != beforeTurns || len(h.Jobs) != 2 || len(h.Jobs[0].Attempts) != 1 || len(h.Jobs[1].Attempts) != 1 {
		t.Fatal("coordinator restart repeated waiting work")
	}
	foreign := slack.Event{ID: eventID("foreign"), WorkspaceID: cfg.Human.WorkspaceID, ChannelID: feature.ChannelID, ThreadTS: feature.ThreadTS, Timestamp: ts(4), UserID: "U-FOREIGN", Text: "!answer " + dep.RequestID + " safe"}
	if err = source.Send(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	if h = history(); h.HumanRequests[0].BackendState != "open" || h.Dependencies[0].State != "human_waiting" {
		t.Fatal("foreign Slack actor changed human request")
	}
	answer := slack.Event{ID: eventID("answer"), WorkspaceID: cfg.Human.WorkspaceID, ChannelID: feature.ChannelID, Timestamp: ts(3), UserID: "U-P3-HUMAN", HumanAction: &slack.HumanAction{RequestID: dep.RequestID, OptionID: "safe", QuestionTS: "1790000000.000100"}}
	if err = source.Send(ctx, answer); err != nil {
		t.Fatal(err)
	}
	h = wait("canonical answer and one A continuation", func(h swarm.History) bool {
		if len(h.Dependencies) != 1 || h.Dependencies[0].State != "continuation_scheduled" || len(h.HumanRequests) != 1 || h.HumanRequests[0].BackendState != "answered" {
			return false
		}
		for _, j := range h.Jobs {
			if j.JobID == dep.JobID {
				return len(j.Attempts) == 2 && j.Attempts[1].State == "succeeded" && j.Attempts[1].Review == "accepted"
			}
		}
		return false
	}, true)
	finalReplies, extraSummaries := 0, 0
	for _, post := range h.SlackOutbox {
		if post.Text == "A completed" {
			finalReplies++
		}
		if post.Text == "B completed" {
			t.Fatal("premature B completion reply appeared after A continued")
		}
	}
	for _, event := range h.Audit {
		if event.Event == "owner_summary_queued" {
			extraSummaries++
		}
	}
	if finalReplies != 1 || extraSummaries != 0 {
		t.Fatalf("final review/reply handover: replies=%d extra_summary_turns=%d", finalReplies, extraSummaries)
	}
	decisionID := h.Dependencies[0].DecisionID
	if decisionID == "" {
		t.Fatal("canonical decision missing")
	}
	if err = source.Send(ctx, answer); err != nil {
		t.Fatal(err)
	}
	storeMu.RLock()
	err = store.SyncHuman(ctx)
	storeMu.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	after := history()
	if len(after.Turns) != len(h.Turns) || len(after.Jobs) != 2 {
		t.Fatal("duplicate Slack answer repeated owner work")
	}
	eventually(t, "answered question buttons disabled", func() bool {
		api.mu.Lock()
		defer api.mu.Unlock()
		for _, id := range api.updates {
			if id == dep.RequestID {
				return true
			}
		}
		return false
	})
	for _, j := range after.Jobs {
		if j.JobID == dep.JobID && len(j.Attempts) != 2 {
			t.Fatal("duplicate answer added A attempt")
		}
	}
	stopBridge()
	bridgeCancel = nil
	storeMu.Lock()
	if err = store.Close(); err == nil {
		store, err = swarm.Open(ctx, statePath, cfg)
	}
	storeMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	bridge = &Bridge{Store: store, Features: cfg.Features, API: api}
	source = &durableFixture{}
	bridgeCtx, bridgeCancel = context.WithCancel(ctx)
	bridgeDone = make(chan error, 1)
	go func() { bridgeDone <- bridge.RunHuman(bridgeCtx, source, cfg.Human.WorkspaceID) }()
	wait("post-decision restart", func(h swarm.History) bool { return h.RecoveryBarrier == "" }, false)
	if err = source.Send(ctx, answer); err != nil {
		t.Fatal(err)
	}
	storeMu.RLock()
	err = store.SyncHuman(ctx)
	storeMu.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	postRestart := history()
	if len(postRestart.Turns) != len(after.Turns) || len(postRestart.Jobs) != 2 {
		t.Fatal("restart and source replay repeated owner work")
	}
	api.mu.Lock()
	questionPosts := 0
	for _, post := range api.posts {
		if post.ID == dep.RequestID {
			questionPosts++
		}
	}
	api.mu.Unlock()
	if questionPosts != 1 {
		t.Fatal("restart republished human question")
	}
	for _, j := range postRestart.Jobs {
		if j.JobID == dep.JobID && len(j.Attempts) != 2 {
			t.Fatal("restart and source replay added A attempt")
		}
	}
	// Verify the true backend terminal source, rather than only the SQLite copy.
	rootPEM, e := os.ReadFile(provider.CA)
	if e != nil {
		t.Fatal(e)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(rootPEM) {
		t.Fatal("invalid provider CA")
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	tokenRaw, e := os.ReadFile(provider.Token)
	if e != nil {
		t.Fatal(e)
	}
	var credential struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(tokenRaw, &credential) != nil {
		t.Fatal("invalid gateway credential")
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, provider.URL+"/api/v1/human-requests/"+dep.RequestID, nil)
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set("Authorization", "Bearer "+credential.Token)
	req.Header.Set("X-Tenant-ID", provider.Tenant)
	req.Header.Set("X-Project-ID", provider.Project)
	res, e := client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("Spexus readback HTTP %d", res.StatusCode)
	}
	var readback struct {
		Data struct {
			State    string `json:"state"`
			Terminal struct {
				ID     string `json:"id"`
				Source struct {
					ActorID   string `json:"actor_id"`
					MessageTS string `json:"message_ts"`
				} `json:"source"`
			} `json:"terminal"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &readback) != nil || readback.Data.State != "answered" || readback.Data.Terminal.ID != decisionID || readback.Data.Terminal.Source.ActorID != "U-P3-HUMAN" || readback.Data.Terminal.Source.MessageTS != answer.Timestamp {
		t.Fatalf("Spexus decision source mismatch: %+v", readback.Data)
	}
	denyStart := slack.Event{ID: eventID("deny-start"), WorkspaceID: cfg.Human.WorkspaceID, ChannelID: feature.ChannelID, ThreadTS: feature.ThreadTS, Timestamp: ts(5), UserID: "U-P3-HUMAN", Text: "Ask deny"}
	if err = source.Send(ctx, denyStart); err != nil {
		t.Fatal(err)
	}
	h = wait("owner step waiting for denial", func(h swarm.History) bool {
		return len(h.Dependencies) == 2 && h.Dependencies[1].State == "human_waiting"
	}, true)
	denyID := h.Dependencies[1].RequestID
	deny := slack.Event{ID: eventID("deny"), WorkspaceID: cfg.Human.WorkspaceID, ChannelID: feature.ChannelID, ThreadTS: feature.ThreadTS, Timestamp: ts(6), UserID: "U-P3-HUMAN", Text: "!answer " + denyID + " deny No authorization"}
	if err = source.Send(ctx, deny); err != nil {
		t.Fatal(err)
	}
	h = wait("canonical denial", func(h swarm.History) bool {
		return len(h.Dependencies) == 2 && h.Dependencies[1].State == "denied" && len(h.HumanRequests) == 2 && h.HumanRequests[1].BackendState == "denied"
	}, true)
	deniedTurns := len(h.Turns)
	late := slack.Event{ID: eventID("late-answer"), WorkspaceID: cfg.Human.WorkspaceID, ChannelID: feature.ChannelID, ThreadTS: feature.ThreadTS, Timestamp: ts(7), UserID: "U-P3-HUMAN", Text: "!answer " + denyID + " yes Too late"}
	if err = source.Send(ctx, late); err != nil {
		t.Fatal(err)
	}
	storeMu.RLock()
	err = store.SyncHuman(ctx)
	storeMu.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	h = history()
	if h.Dependencies[1].State != "denied" || len(h.Turns) != deniedTurns {
		t.Fatal("late answer reopened denied step")
	}
	cancelStart := slack.Event{ID: eventID("cancel-start"), WorkspaceID: cfg.Human.WorkspaceID, ChannelID: feature.ChannelID, ThreadTS: feature.ThreadTS, Timestamp: ts(8), UserID: "U-P3-HUMAN", Text: "Ask cancel"}
	if err = source.Send(ctx, cancelStart); err != nil {
		t.Fatal(err)
	}
	h = wait("owner step waiting for stop", func(h swarm.History) bool {
		return len(h.Dependencies) == 3 && h.Dependencies[2].State == "human_waiting"
	}, true)
	cancelID := h.Dependencies[2].RequestID
	stop := slack.Event{ID: eventID("stop"), WorkspaceID: cfg.Human.WorkspaceID, ChannelID: feature.ChannelID, ThreadTS: feature.ThreadTS, Timestamp: ts(9), UserID: "U-P3-HUMAN", Text: "!stop"}
	if err = source.Send(ctx, stop); err != nil {
		t.Fatal(err)
	}
	h = wait("stop cancellation in Spexus", func(h swarm.History) bool {
		return h.Feature.Stopped && len(h.Dependencies) == 3 && h.Dependencies[2].State == "cancelled" && len(h.HumanRequests) == 3 && h.HumanRequests[2].BackendState == "cancelled"
	}, true)
	lateAfterStop := slack.Event{ID: eventID("late-stop-answer"), WorkspaceID: cfg.Human.WorkspaceID, ChannelID: feature.ChannelID, ThreadTS: feature.ThreadTS, Timestamp: ts(10), UserID: "U-P3-HUMAN", Text: "!answer " + cancelID + " yes Too late"}
	if err = source.Send(ctx, lateAfterStop); err != nil {
		t.Fatal(err)
	}
	continued := slack.Event{ID: eventID("continue"), WorkspaceID: cfg.Human.WorkspaceID, ChannelID: feature.ChannelID, ThreadTS: feature.ThreadTS, Timestamp: ts(11), UserID: "U-P3-HUMAN", Text: "!continue"}
	if err = source.Send(ctx, continued); err != nil {
		t.Fatal(err)
	}
	h = wait("continue leaves cancellation terminal", func(h swarm.History) bool { return !h.Feature.Stopped && h.Dependencies[2].State == "cancelled" }, false)
	for _, j := range h.Jobs {
		if j.JobID == dep.JobID && len(j.Attempts) != 2 {
			t.Fatal("denial/stop retried A")
		}
	}
	for _, cmd := range commands {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	for _, done := range stopped {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("runner failed to stop")
		}
	}
	ownerLaunches, workerLaunches := 0, 0
	runnerHistories := make([]swarmrunner.JournalHistory, 0, len(states))
	for i, state := range states {
		jh, e := swarmrunner.ReadHistory(state)
		if e != nil {
			t.Fatal(e)
		}
		runnerHistories = append(runnerHistories, jh)
		launches := 0
		for _, input := range jh.Inputs {
			launches += input.Launches
		}
		if i == 0 {
			ownerLaunches = launches
		} else {
			workerLaunches += launches
		}
	}
	if ownerLaunches != 8 || workerLaunches != 3 {
		t.Fatalf("unexpected model launch counts owner=%d workers=%d", ownerLaunches, workerLaunches)
	}
	if evidenceDir := os.Getenv("SPEXUS_HR_EVIDENCE_DIR"); evidenceDir != "" {
		if err = os.MkdirAll(evidenceDir, 0700); err != nil {
			t.Fatal(err)
		}
		artifact := struct {
			FeatureID             string                       `json:"feature_id"`
			InitialRequestID      string                       `json:"initial_request_id"`
			DecisionID            string                       `json:"decision_id"`
			DenialRequestID       string                       `json:"denial_request_id"`
			CancellationRequestID string                       `json:"cancellation_request_id"`
			OwnerLaunches         int                          `json:"owner_launches"`
			WorkerLaunches        int                          `json:"worker_launches"`
			History               swarm.History                `json:"history"`
			RunnerJournals        []swarmrunner.JournalHistory `json:"runner_journals"`
			BackendTerminal       json.RawMessage              `json:"backend_answered_readback"`
		}{feature.FeatureID, dep.RequestID, decisionID, denyID, cancelID, ownerLaunches, workerLaunches, h, runnerHistories, body}
		data, marshalErr := json.MarshalIndent(artifact, "", "  ")
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		path := filepath.Join(evidenceDir, "hr05-local-history-"+feature.FeatureID+".json")
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		t.Logf("durable evidence=%s", path)
	}
	t.Logf("feature=%s request=%s decision=%s dependency=%s owner_turns=%d owner_launches=%d worker_launches=%d", feature.FeatureID, dep.RequestID, decisionID, dep.ID, len(after.Turns), ownerLaunches, workerLaunches)
}
