package swarmrunner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

func waitFixtureReady(t *testing.T, dir string) (string, swarm.LaunchClaim) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".ready") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var marker swarm.LaunchClaim
			if err := json.Unmarshal(raw, &marker); err != nil {
				t.Fatal(err)
			}
			return strings.TrimSuffix(path, ".ready"), marker
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("claim barrier did not become ready")
	return "", swarm.LaunchClaim{}
}

func TestLocalFixtureClaimBarrierPausesBeforePiAndSurvivesRestart(t *testing.T) {
	for _, mode := range []string{"release", "crash"} {
		t.Run(mode, func(t *testing.T) {
			var acceptedID string
			var accepted, started, results, launches atomic.Int32
			handler := http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
				if q.URL.Path == swarm.APIPrefix+"/messages" {
					var message swarm.Envelope
					if err := json.NewDecoder(q.Body).Decode(&message); err != nil {
						t.Error(err)
						return
					}
					switch message.Type {
					case "task.accepted":
						acceptedID = message.MessageID
						accepted.Add(1)
					case "task.started":
						started.Add(1)
					case "task.result":
						results.Add(1)
					default:
						t.Errorf("unexpected message %s", message.Type)
					}
					writeJSON(w, swarm.Receipt{MessageID: message.MessageID, Receipt: "stored", MailboxSeq: 2})
					return
				}
				if strings.Contains(q.URL.Path, "/jobs/") {
					writeJSON(w, swarm.JobView{JobID: job, FeatureID: feature, CurrentAttemptID: attempt, Attempts: []swarm.Attempt{{AttemptID: attempt, AssignedAgentID: "worker-a", State: "running", AcceptedMessageID: acceptedID}}})
					return
				}
				t.Errorf("unexpected request %s", q.URL.Path)
				http.NotFound(w, q)
			})
			r, _ := runnerFixture(t, handler)
			if err := r.EnableLocalFixtureClaimBarrier(); err != nil {
				t.Fatal(err)
			}
			r.model = modelFunc(func(context.Context, string, string) (string, bool, error) {
				launches.Add(1)
				return `{"outcome":"succeeded","summary":"done","evidence":[],"error":null}`, false, nil
			})
			d := dispatchFixture(r)
			storeInput(t, r, d)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- r.process(ctx, d) }()
			base, marker := waitFixtureReady(t, r.fixtureClaimBarrierDir)
			var claimID, state, source string
			var snapshot []byte
			if err := r.journal.db.QueryRow(`SELECT claim_id,state,source,snapshot FROM profile_launches WHERE seq=?`, d.MailboxSeq).Scan(&claimID, &state, &source, &snapshot); err != nil {
				t.Fatal(err)
			}
			var launchCount int
			if err := r.journal.db.QueryRow(`SELECT launches FROM inbox WHERE seq=?`, d.MailboxSeq).Scan(&launchCount); err != nil {
				t.Fatal(err)
			}
			if claimID == "" || marker.ClaimID != claimID || marker.ProfileID != r.profile.ID || state != "claimed" || source != "web" || string(snapshot) != string(r.profile.Bytes) || launchCount != 0 || accepted.Load() != 1 || started.Load() != 0 || launches.Load() != 0 {
				t.Fatalf("barrier not between pin and launch: claim=%s marker=%+v state=%s source=%s launches=%d accepted=%d started=%d pi=%d", claimID, marker, state, source, launchCount, accepted.Load(), started.Load(), launches.Load())
			}
			if observations, err := r.journal.pendingProfileObservations(); err != nil || len(observations) != 0 {
				t.Fatalf("premature launch observation: %+v %v", observations, err)
			}
			if mode == "release" {
				if err := os.WriteFile(base+".release", []byte("release\n"), 0600); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("release did not resume runner")
				}
				if started.Load() != 1 || launches.Load() != 1 || results.Load() != 1 {
					t.Fatalf("release did not launch once: started=%d pi=%d result=%d", started.Load(), launches.Load(), results.Load())
				}
				if err := r.journal.db.QueryRow(`SELECT state FROM profile_launches WHERE seq=?`, d.MailboxSeq).Scan(&state); err != nil || state != "observed" {
					t.Fatalf("launch was not observed: %s %v", state, err)
				}
				return
			}
			// Apply the same startup recovery transition to the durable state
			// captured at the pause point, before allowing the blocked goroutine
			// to clean up. This models a killed process with no graceful write.
			if err := r.journal.recover(); err != nil {
				t.Fatal(err)
			}
			var inboxState string
			if err := r.journal.db.QueryRow(`SELECT state FROM inbox WHERE seq=?`, d.MailboxSeq).Scan(&inboxState); err != nil || inboxState != "interrupted" {
				t.Fatalf("startup recovery did not quarantine unknown claim: %s %v", inboxState, err)
			}
			cancel()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("cancel did not stop barrier")
			}
			if err := r.journal.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenJournal(r.cfg.StateDirectory)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err := reopened.recover(); err != nil {
				t.Fatal(err)
			}
			if _, pending, err := reopened.next(); err != nil || pending {
				t.Fatalf("restart replayed unknown claim: pending=%v err=%v", pending, err)
			}
			if observations, err := reopened.pendingProfileObservations(); err != nil || len(observations) != 0 {
				t.Fatalf("restart invented launched observation: %+v %v", observations, err)
			}
			if err := reopened.db.QueryRow(`SELECT state FROM profile_launches WHERE seq=?`, d.MailboxSeq).Scan(&state); err != nil || state != "claimed" {
				t.Fatalf("unknown claim was changed: %s %v", state, err)
			}
			if started.Load() != 0 || launches.Load() != 0 || results.Load() != 0 {
				t.Fatal("Pi or completion proceeded after crash point")
			}
		})
	}
}

func TestLocalFixtureClaimBarrierIsNotConfiguredByRuntimeJSON(t *testing.T) {
	r, _ := runnerFixture(t, http.NotFoundHandler())
	if r.fixtureClaimBarrierDir != "" {
		t.Fatal("normal runner enabled fixture barrier")
	}
	if _, err := os.Stat(filepath.Join(r.cfg.StateDirectory, "claim-barrier")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("normal runner created barrier: %v", err)
	}
	config, err := json.Marshal(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(config, &fields); err != nil {
		t.Fatal(err)
	}
	fields["pause_after_claim"] = json.RawMessage(`true`)
	config, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Config
	if err := decode(config, &decoded); err == nil {
		t.Fatal("runtime JSON enabled fixture barrier")
	}
	d := dispatchFixture(r)
	storeInput(t, r, d)
	if err := r.journal.starting(d.MailboxSeq, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.claimProfile(context.Background(), d, r.profile); err != nil {
		t.Fatalf("normal claim blocked: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.cfg.StateDirectory, "claim-barrier")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("normal claim created barrier: %v", err)
	}
	private := r.cfg.StateDirectory
	if err := os.Chmod(private, 0777); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(private, 0700)
	if err := r.EnableLocalFixtureClaimBarrier(); err == nil {
		t.Fatal("nonprivate state directory admitted fixture barrier")
	}
}
