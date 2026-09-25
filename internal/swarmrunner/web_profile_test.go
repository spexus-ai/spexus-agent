package swarmrunner

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

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
