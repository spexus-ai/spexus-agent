package swarmrunner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

func TestWebQueuedWorkerProfileInvalidationIsVisibleBeforeClaim(t *testing.T) {
	for _, phase := range []string{"initial", "prelaunch"} {
		for _, change := range []string{"edited", "disabled"} {
			t.Run(phase+"_"+change, func(t *testing.T) {
				var override *swarm.ActiveProfile
				reads, claims, accepted, started, launches := 0, 0, 0, 0, 0
				var result swarm.ResultPayload
				handler := http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
					if q.URL.Path != swarm.APIPrefix+"/messages" {
						t.Errorf("unexpected request %s", q.URL.Path)
						http.NotFound(w, q)
						return
					}
					var message swarm.Envelope
					if err := json.NewDecoder(q.Body).Decode(&message); err != nil {
						t.Fatal(err)
					}
					switch message.Type {
					case "task.accepted":
						accepted++
					case "task.started":
						started++
					case "task.result":
						if err := json.Unmarshal(message.Payload, &result); err != nil {
							t.Fatal(err)
						}
					default:
						t.Errorf("unexpected message %s", message.Type)
					}
					writeJSON(w, swarm.Receipt{MessageID: message.MessageID, Receipt: "stored", MailboxSeq: 2})
				})
				intercept := func(w http.ResponseWriter, q *http.Request) bool {
					if q.URL.Path == swarm.APIPrefix+"/agent-profiles/worker-a/active" {
						reads++
						if override != nil && (phase == "initial" || reads > 1) {
							writeJSON(w, *override)
							return true
						}
					}
					if strings.HasSuffix(q.URL.Path, "/launch-claims") {
						claims++
					}
					return false
				}
				r, _ := runnerFixtureWithIntercept(t, handler, intercept)
				active := swarm.ActiveProfile{Slot: "worker-a", Role: "worker", ProfileID: "worker-a", Enabled: change != "disabled", Generation: 2, AllowedModels: []string{r.profile.Model}}
				p := r.profile.TextProfile
				if change == "edited" {
					p.Prompt = "Changed while queued"
				}
				raw, err := json.Marshal(p)
				if err != nil {
					t.Fatal(err)
				}
				active.ActiveRevision, active.SnapshotJSON, active.SnapshotBytesBase64 = swarm.Digest(raw), raw, base64.StdEncoding.EncodeToString(raw)
				override = &active
				d := dispatchFixture(r)
				storeInput(t, r, d)
				r.model = modelFunc(func(context.Context, string, string) (string, bool, error) { launches++; return "", false, nil })
				if err := r.process(context.Background(), d); err != nil {
					t.Fatal(err)
				}
				want := "profile_changed_before_launch"
				if change == "disabled" {
					want = "profile_disabled_before_launch"
				}
				if result.Error == nil || result.Error.Code != want || result.Outcome != "failed" {
					t.Fatalf("result=%+v, want %s", result, want)
				}
				wantAccepted := 0
				if phase == "prelaunch" {
					wantAccepted = 1
				}
				if accepted != wantAccepted || started != 0 || claims != 0 || launches != 0 || reads != wantAccepted+1 {
					t.Fatalf("accepted=%d started=%d claims=%d launches=%d reads=%d", accepted, started, claims, launches, reads)
				}
				var body []byte
				var launchCount int
				if err := r.journal.db.QueryRow(`SELECT body,launches FROM inbox WHERE seq=?`, d.MailboxSeq).Scan(&body, &launchCount); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(body), r.profile.Revision) || launchCount != 0 {
					t.Fatal("queued attempt lost original dispatch or recorded a launch")
				}
			})
		}
	}
}

func TestWebQueuedWorkerBackendOutageIsVisible(t *testing.T) {
	var result swarm.ResultPayload
	claims, launches := 0, 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		var message swarm.Envelope
		if err := json.NewDecoder(q.Body).Decode(&message); err != nil {
			t.Fatal(err)
		}
		if message.Type != "task.result" {
			t.Errorf("unexpected %s", message.Type)
		}
		if err := json.Unmarshal(message.Payload, &result); err != nil {
			t.Fatal(err)
		}
		writeJSON(w, swarm.Receipt{MessageID: message.MessageID, Receipt: "stored", MailboxSeq: 2})
	})
	intercept := func(w http.ResponseWriter, q *http.Request) bool {
		if strings.HasSuffix(q.URL.Path, "/active") {
			httpError(w, http.StatusServiceUnavailable, "storage_unavailable")
			return true
		}
		if strings.HasSuffix(q.URL.Path, "/launch-claims") {
			claims++
		}
		return false
	}
	r, _ := runnerFixtureWithIntercept(t, handler, intercept)
	d := dispatchFixture(r)
	storeInput(t, r, d)
	r.model = modelFunc(func(context.Context, string, string) (string, bool, error) { launches++; return "", false, nil })
	if err := r.process(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if result.Error == nil || result.Error.Code != "profile_backend_unavailable" || claims != 0 || launches != 0 {
		t.Fatalf("outage result=%+v claims=%d launches=%d", result, claims, launches)
	}
}

func TestWebWorkerClaimRaceReportsEditOrDisable(t *testing.T) {
	for _, scenario := range []struct {
		name, backendCode, want string
		status                  int
	}{
		{"edit_wins", "GENERATION_CONFLICT", "profile_changed_before_launch", http.StatusPreconditionFailed},
		{"disable_wins", "DISABLED", "profile_disabled_before_launch", http.StatusConflict},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var result swarm.ResultPayload
			accepted, started, claims, launches := 0, 0, 0, 0
			handler := http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
				if q.URL.Path != swarm.APIPrefix+"/messages" {
					t.Errorf("unexpected %s", q.URL.Path)
					http.NotFound(w, q)
					return
				}
				var message swarm.Envelope
				if err := json.NewDecoder(q.Body).Decode(&message); err != nil {
					t.Fatal(err)
				}
				switch message.Type {
				case "task.accepted":
					accepted++
				case "task.started":
					started++
				case "task.result":
					if err := json.Unmarshal(message.Payload, &result); err != nil {
						t.Fatal(err)
					}
				default:
					t.Errorf("unexpected %s", message.Type)
				}
				writeJSON(w, swarm.Receipt{MessageID: message.MessageID, Receipt: "stored", MailboxSeq: 2})
			})
			intercept := func(w http.ResponseWriter, q *http.Request) bool {
				if strings.HasSuffix(q.URL.Path, "/launch-claims") {
					claims++
					httpError(w, scenario.status, scenario.backendCode)
					return true
				}
				return false
			}
			r, _ := runnerFixtureWithIntercept(t, handler, intercept)
			d := dispatchFixture(r)
			storeInput(t, r, d)
			r.model = modelFunc(func(context.Context, string, string) (string, bool, error) { launches++; return "", false, nil })
			if err := r.process(context.Background(), d); err != nil {
				t.Fatal(err)
			}
			if result.Error == nil || result.Error.Code != scenario.want || accepted != 1 || started != 0 || claims != 1 || launches != 0 {
				t.Fatalf("result=%+v accepted=%d started=%d claims=%d launches=%d", result, accepted, started, claims, launches)
			}
		})
	}
}

func TestWebActiveProfilePinsExactBytesAndAvailableModel(t *testing.T) {
	p := swarm.TextProfile{ID: "orchestrator", Model: "openai-codex/gpt-6-luna", Reasoning: "minimal", Prompt: "Own the feature", Tools: []string{}, Extensions: []string{}}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	active := swarm.ActiveProfile{Slot: "owner", Role: "owner", ProfileID: p.ID, Enabled: true, Generation: 2, ActiveRevision: swarm.Digest(raw), SnapshotJSON: raw, SnapshotBytesBase64: base64.StdEncoding.EncodeToString(raw)}
	got, err := profileFromActive(active, p.ID, []string{p.Model})
	if err != nil || string(got.Bytes) != string(raw) || got.Generation != 2 {
		t.Fatalf("valid profile lost exact bytes: %+v, %v", got, err)
	}
	active.SnapshotBytesBase64 = base64.StdEncoding.EncodeToString(append(raw, ' '))
	if _, err := profileFromActive(active, p.ID, []string{p.Model}); err == nil {
		t.Fatal("digest mismatch accepted")
	}
	active.SnapshotBytesBase64 = base64.StdEncoding.EncodeToString(raw)
	if _, err := profileFromActive(active, p.ID, []string{"other/model"}); err == nil {
		t.Fatal("unavailable model accepted")
	}
}

func TestWebProfileJournalNeverDuplicatesUnknownLaunch(t *testing.T) {
	j, err := OpenJournal(t.TempDir(), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	seq := int64(1)
	if _, err := j.db.Exec(`INSERT INTO inbox(seq,body,type,state) VALUES(1,'{}','task.dispatch','starting')`); err != nil {
		t.Fatal(err)
	}
	p := profile{TextProfile: swarm.TextProfile{ID: "worker-a", Model: "openai-codex/gpt-6-luna", Reasoning: "minimal"}, Revision: "revision", Generation: 1, Bytes: []byte("snapshot")}
	ref := swarm.ExecutionRef{WorkerAttemptID: swarm.NewID()}
	claim := swarm.LaunchClaim{ClaimID: swarm.NewID()}
	if err := j.pin(seq, p, claim, ref); err != nil {
		t.Fatal(err)
	}
	if err := j.launch(seq); err != nil {
		t.Fatal(err)
	}
	if err := j.launch(seq); err == nil {
		t.Fatal("duplicate launch accepted")
	}
	if obs, err := j.pendingProfileObservations(); err != nil || len(obs) != 0 {
		t.Fatalf("unknown launch generated observation: %+v %v", obs, err)
	}
	if err := j.physicalLaunch(seq, claim.ClaimID); err != nil {
		t.Fatal(err)
	}
	if obs, err := j.pendingProfileObservations(); err != nil || len(obs) != 1 || obs[0].ClaimID != claim.ClaimID {
		t.Fatalf("physical launch evidence missing: %+v %v", obs, err)
	}
	if err := j.observed(seq, claim.ClaimID); err != nil {
		t.Fatal(err)
	}
	if err := j.correctionLaunch(seq); err != nil {
		t.Fatal(err)
	}
	if err := j.correctionLaunch(seq); err == nil {
		t.Fatal("duplicate correction launch accepted")
	}
	if err := j.recover(); err != nil {
		t.Fatal(err)
	}
	if obs, err := j.pendingProfileObservations(); err != nil || len(obs) != 0 {
		t.Fatalf("unknown correction launch generated observation: %+v %v", obs, err)
	}
	var source, revision, model, reasoning string
	var snapshot []byte
	if err := j.db.QueryRow(`SELECT source,revision,model,reasoning,snapshot FROM profile_launches WHERE seq=?`, seq).Scan(&source, &revision, &model, &reasoning, &snapshot); err != nil {
		t.Fatal(err)
	}
	if source != "web" || revision != p.Revision || model != p.Model || reasoning != p.Reasoning || string(snapshot) != string(p.Bytes) {
		t.Fatal("correction changed pinned web profile")
	}
	if obs, err := j.pendingProfileObservations(); err != nil || len(obs) != 0 {
		t.Fatalf("observation remained pending: %+v %v", obs, err)
	}
}
