//go:build p3experiment

package swarmrunner

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

// This is a fault experiment against P2, not a production P3 recovery API.
// The test driver controls ingress ordering; no real Slack catchup is implied.
func TestP3CrashChild(t *testing.T) {
	path := os.Getenv("SPEXUS_P3_CHILD_CONFIG")
	if path == "" {
		t.Skip("subprocess only")
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	r := p3OpenRunner(t, c)
	defer r.Close()
	r.model = modelFunc(func(context.Context, string, string) (string, bool, error) {
		f, err := os.OpenFile(filepath.Join(c.StateDirectory, "model-calls"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return "", false, err
		}
		_, err = f.WriteString("launch\n")
		if err == nil {
			err = f.Sync()
		}
		f.Close()
		if err != nil {
			return "", false, err
		}
		actions := []any{}
		for _, target := range r.targets {
			actions = append(actions, map[string]any{"kind": "dispatch", "data": map[string]any{
				"worker_agent_id": target.AgentID, "goal": "Compute 6 * 7", "scope": "text only",
				"expected_result": []string{"42"}, "context": swarm.TaskContext{Text: "P3 crash experiment", Refs: []swarm.ContextRef{}}, "profile": target.Profile, "run_timeout_seconds": 60,
			}})
		}
		b, err := json.Marshal(map[string]any{"actions": actions, "reply": "Delegation requested."})
		return string(b), false, err
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
}

func p3OpenRunner(t *testing.T, c Config) *Runner {
	t.Helper()
	p, e := loadProfile(c.ProfileFile)
	if e != nil {
		t.Fatal(e)
	}
	client, e := NewClient(c)
	if e != nil {
		t.Fatal(e)
	}
	j, e := OpenJournal(c.StateDirectory)
	if e != nil {
		t.Fatal(e)
	}
	if e = j.identity(c); e != nil {
		j.Close()
		t.Fatal(e)
	}
	r := &Runner{cfg: c, profile: p, client: client, journal: j}
	for _, target := range c.Targets {
		p, e := loadProfile(target.ProfileFile)
		if e != nil {
			t.Fatal(e)
		}
		r.targets = append(r.targets, targetProfile{target.AgentID, p.wire()})
	}
	return r
}
func p3Write(t *testing.T, path string, v any) {
	t.Helper()
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, b, 0600); e != nil {
		t.Fatal(e)
	}
}
func p3Must(t *testing.T, e error) {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
}

func TestP3CommittedOwnerOutputRecoveryExperiment(t *testing.T) {
	for _, scenario := range []string{"rotate_first_control", "replay_before_rotate", "offline_stop", "partial_actions", "lost_receipt", "temporary_barrier"} {
		t.Run(scenario, func(t *testing.T) { p3Scenario(t, scenario) })
	}
}

func p3Scenario(t *testing.T, scenario string) {
	ctx := context.Background()
	dir := t.TempDir()
	if root := os.Getenv("SPEXUS_P3_EVIDENCE_DIR"); root != "" {
		p3Must(t, os.MkdirAll(root, 0700))
		var e error
		dir, e = os.MkdirTemp(root, scenario+"-")
		p3Must(t, e)
	}
	cfg := swarm.Config{TenantID: swarm.NewID(), ProjectID: swarm.NewID()}
	profiles := []string{}
	ids := []string{"owner", "worker-a", "worker-b"}
	for i, id := range ids {
		role := "worker"
		if i == 0 {
			role = "owner"
		}
		p := swarm.TextProfile{ID: id, Model: "fixture/deterministic", Reasoning: "minimal", Prompt: "P3 controlled crash fixture", Tools: []string{}, Extensions: []string{}}
		b, e := json.Marshal(p)
		p3Must(t, e)
		path := filepath.Join(dir, id+".json")
		p3Must(t, os.WriteFile(path, b, 0600))
		profiles = append(profiles, path)
		cfg.Profiles = append(cfg.Profiles, swarm.ProfileSnapshot{Bytes: b})
		cfg.Agents = append(cfg.Agents, swarm.AgentConfig{AgentID: id, Role: role, ProfileID: id, CredentialSHA256: swarm.Digest([]byte("test-token-" + id))})
	}
	featureID := swarm.NewID()
	cfg.Features = []swarm.Feature{{FeatureID: featureID, TenantID: cfg.TenantID, ProjectID: cfg.ProjectID, OwnerAgentID: "owner", ChannelID: "C-TEST", ThreadTS: "123.456", AllowedActorIDs: []string{"U-TEST"}}}
	dbpath := filepath.Join(dir, "coordinator.db")
	store, e := swarm.Open(ctx, dbpath, cfg)
	p3Must(t, e)
	defer func() { store.Close() }()
	var handler atomic.Value
	handler.Store(store.Handler())
	hit := make(chan struct{})
	release := make(chan struct{})
	var intercepted atomic.Int32
	var barrier atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Method == "POST" && q.URL.Path == swarm.APIPrefix+"/messages" {
			if barrier.Load() {
				httpError(w, 503, "recovery_pending")
				return
			}
			n := intercepted.Add(1)
			target := int32(1)
			if scenario == "partial_actions" {
				target = 2
			}
			if n == target {
				if scenario == "lost_receipt" {
					recorder := httptest.NewRecorder()
					handler.Load().(http.Handler).ServeHTTP(recorder, q)
					if recorder.Code != http.StatusCreated && recorder.Code != http.StatusOK {
						t.Errorf("commit before lost receipt: %d %s", recorder.Code, recorder.Body.String())
					}
				}
				close(hit)
				<-release
				httpError(w, 503, "fixture_crashed")
				return
			}
		}
		handler.Load().(http.Handler).ServeHTTP(w, q)
	}))
	defer server.Close()
	ca := filepath.Join(dir, "ca.pem")
	p3Must(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600))
	token := filepath.Join(dir, "fixture-token")
	p3Must(t, os.WriteFile(token, []byte("test-token-owner"), 0600))
	c := Config{CoordinatorURL: server.URL, CAFile: ca, CredentialFile: token, TenantID: cfg.TenantID, ProjectID: cfg.ProjectID, AgentID: "owner", InstanceID: swarm.NewID(), Role: "owner", ProfileFile: profiles[0], StateDirectory: filepath.Join(dir, "runner"), Workspace: filepath.Join(dir, "workspace"), Targets: []Target{{AgentID: "worker-a", ProfileFile: profiles[1]}, {AgentID: "worker-b", ProfileFile: profiles[2]}}}
	p3Must(t, os.MkdirAll(c.Workspace, 0700))
	configPath := filepath.Join(dir, "runner-config.json")
	p3Write(t, configPath, c)
	_, _, e = store.Ingest(ctx, featureID, swarm.InputPayload{Text: "Delegate two text calculations", Source: swarm.Source{Kind: "slack", EventID: "event-1", ChannelID: "C-TEST", ThreadTS: "123.456", ActorID: "U-TEST"}})
	p3Must(t, e)
	binary, e := os.Executable()
	p3Must(t, e)
	child := exec.Command(binary, "-test.run=^TestP3CrashChild$", "-test.v")
	child.Env = append(os.Environ(), "SPEXUS_P3_CHILD_CONFIG="+configPath)
	log, e := os.Create(filepath.Join(dir, "child.log"))
	p3Must(t, e)
	defer log.Close()
	child.Stdout = log
	child.Stderr = log
	p3Must(t, child.Start())
	reaped := false
	defer func() {
		if !reaped {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	}()
	// Always release an intercepted handler, even when the crash precondition fails.
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	select {
	case <-hit:
	case <-time.After(15 * time.Second):
		b, _ := os.ReadFile(filepath.Join(dir, "child.log"))
		t.Fatalf("did not reach durable-output crash point: %s", b)
	}
	p3Must(t, child.Process.Kill())
	if e = child.Wait(); e == nil {
		t.Fatal("child unexpectedly exited cleanly")
	}
	reaped = true
	close(release)
	released = true
	before, e := ReadHistory(c.StateDirectory)
	p3Must(t, e)
	p3Write(t, filepath.Join(dir, "runner-before.json"), before)
	if len(before.Inputs) != 1 || before.Inputs[0].State != "output" || before.Inputs[0].Launches != 1 {
		t.Fatalf("wrong crash boundary: %+v", before)
	}
	r := p3OpenRunner(t, c)
	closed := false
	defer func() {
		if !closed {
			r.Close()
		}
	}()
	p3Must(t, r.journal.recover())
	var outputBefore []byte
	p3Must(t, r.journal.db.QueryRow("SELECT output FROM inbox WHERE seq=?", before.Inputs[0].Seq).Scan(&outputBefore))
	beforeCoord, e := store.History(ctx, featureID)
	p3Must(t, e)
	p3Write(t, filepath.Join(dir, "coordinator-before.json"), beforeCoord)
	expectedPreJobs := 0
	if scenario == "partial_actions" || scenario == "lost_receipt" {
		expectedPreJobs = 1
	}
	if len(beforeCoord.Jobs) != expectedPreJobs {
		t.Fatalf("pre-crash jobs=%d want %d", len(beforeCoord.Jobs), expectedPreJobs)
	}
	// Control: P2 reconciliation rotates authority without consulting the journal.
	rotate := func() {
		p3Must(t, store.Close())
		p3Must(t, swarm.ReconcileOffline(ctx, dbpath, cfg, swarm.ReconcileRequest{AgentID: "owner", OldInstanceID: c.InstanceID, NewInstanceID: swarm.NewID(), Reason: "Local process killed and reaped by test; simulated container attestation", Actor: "test-operator", ContainerID: "fixture-not-a-real-container", ContainerStopped: true, CheckedAt: time.Now().UTC()}))
		var e error
		store, e = swarm.Open(ctx, dbpath, cfg)
		p3Must(t, e)
		handler.Store(store.Handler())
	}
	if scenario == "rotate_first_control" {
		rotate()
	}
	// The driver supplies the missed stop. This is not a Slack history implementation.
	if scenario == "offline_stop" {
		p3Must(t, store.StopFeature(ctx, featureID, "U-TEST", "Missed stop delivered before replay"))
	}
	if scenario == "temporary_barrier" {
		barrier.Store(true)
		waitCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
		e = r.flush(waitCtx)
		cancel()
		if e == nil {
			t.Fatal("temporary barrier did not block")
		}
		held, e := r.journal.History()
		p3Must(t, e)
		if held.Pending != before.Pending {
			t.Fatal("temporary barrier consumed pending action")
		}
		for _, x := range held.Outbox {
			if x.Status == "rejected" {
				t.Fatal("temporary barrier became permanent rejection")
			}
		}
		p3Write(t, filepath.Join(dir, "runner-deferred.json"), held)
		p3Must(t, r.Close())
		closed = true
		r = p3OpenRunner(t, c)
		closed = false
		barrier.Store(false)
	}
	flushCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	e = r.flush(flushCtx)
	if scenario == "rotate_first_control" {
		var rejected *HTTPError
		if !errors.As(e, &rejected) || rejected.Code != "instance_conflict" {
			t.Fatalf("expected instance_conflict, got %v", e)
		}
	} else {
		p3Must(t, e)
		p3Must(t, r.flush(flushCtx))
	}
	after, e := r.journal.History()
	p3Must(t, e)
	var outputAfter []byte
	p3Must(t, r.journal.db.QueryRow("SELECT output FROM inbox WHERE seq=?", before.Inputs[0].Seq).Scan(&outputAfter))
	if string(outputBefore) != string(outputAfter) {
		t.Fatal("typed output changed")
	}
	if len(before.Outbox) != len(after.Outbox) {
		t.Fatal("recovery changed outbox identities")
	}
	for i, old := range before.Outbox {
		current := after.Outbox[i]
		if old.MessageID != current.MessageID || old.Kind != current.Kind || old.Seq != current.Seq {
			t.Fatal("outbox identity changed")
		}
		if old.Status == "stored" && current.Status != "stored" {
			t.Fatal("stored receipt was overwritten")
		}
	}
	if after.Inputs[0].Launches != 1 {
		t.Fatal("model replay")
	}
	calls, e := os.ReadFile(filepath.Join(c.StateDirectory, "model-calls"))
	p3Must(t, e)
	if strings.Count(string(calls), "launch\n") != 1 {
		t.Fatal("model call counter changed")
	}
	if _, ok, e := r.journal.next(); e != nil || ok {
		t.Fatalf("output input scheduled again: %v %v", ok, e)
	}
	if scenario != "rotate_first_control" {
		rotate()
	}
	final, e := store.History(ctx, featureID)
	p3Must(t, e)
	expectedJobs := 2
	expectedState := "succeeded"
	if scenario == "offline_stop" || scenario == "rotate_first_control" {
		expectedJobs = 0
		expectedState = "interrupted"
	}
	if len(final.Jobs) != expectedJobs || len(final.Turns) != 1 || final.Turns[0].State != expectedState {
		t.Fatalf("wrong final state: jobs=%d turns=%+v", len(final.Jobs), final.Turns)
	}
	if scenario != "rotate_first_control" && after.Pending != 0 {
		t.Fatal("replay left pending operations")
	}
	var heartbeat swarm.HeartbeatResponse
	staleErr := r.client.once(ctx, "POST", "/agents/self/heartbeat", swarm.HeartbeatRequest{InstanceID: c.InstanceID}, &heartbeat)
	var stale *HTTPError
	if !errors.As(staleErr, &stale) || stale.Code != "instance_conflict" {
		t.Fatalf("expected stale instance conflict, got %v", staleErr)
	}
	p3Write(t, filepath.Join(dir, "runner-after.json"), after)
	p3Write(t, filepath.Join(dir, "coordinator-after.json"), final)
	result := map[string]any{"scenario": scenario, "owner_model_calls": 1, "worker_model_calls": 0, "jobs_before": len(beforeCoord.Jobs), "jobs_after": len(final.Jobs), "turn_state": final.Turns[0].State, "pending_before": before.Pending, "pending_after": after.Pending, "typed_output_preserved": true, "stale_instance_rejected": true, "container_proof": "fixture only; process kill/reap real", "slack_catchup": "test-controlled ordering, not Slack implementation"}
	p3Write(t, filepath.Join(dir, "summary.json"), result)
	t.Logf("P3_EVIDENCE %s %s", scenario, dir)
	t.Logf("P3_RESULT %s", func() string { b, _ := json.Marshal(result); return string(b) }())
}
