package swarmslack

import (
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

// profileServiceFixture supplies the same scoped wire contract used by the
// coordinator and real runner process tests; no local profile file is read.
func profileServiceFixture(t *testing.T, cfg *swarm.Config, model string) {
	t.Helper()
	profiles := map[string][]byte{}
	for _, id := range []string{"orchestrator", "worker-a", "worker-b"} {
		raw, err := json.Marshal(swarm.TextProfile{ID: id, Model: model, Reasoning: "minimal", Prompt: "Return JSON", Tools: []string{}, Extensions: []string{}})
		if err != nil {
			t.Fatal(err)
		}
		profiles[id] = raw
	}
	claims := map[string]swarm.LaunchClaim{}
	var mu sync.Mutex
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Tenant-ID") != cfg.TenantID || r.Header.Get("X-Project-ID") != cfg.ProjectID {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/agent-profiles/"), "/")
		if len(parts) < 2 || profiles[parts[0]] == nil {
			http.NotFound(w, r)
			return
		}
		id, raw := parts[0], profiles[parts[0]]
		respond := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 1, "data": v}) }
		switch parts[1] {
		case "active":
			role, slot := "worker", id
			if id == "orchestrator" {
				role, slot = "owner", "owner"
			}
			respond(swarm.ActiveProfile{Slot: slot, Role: role, ProfileID: id, Enabled: true, Generation: 1, ActiveRevision: swarm.Digest(raw), SnapshotJSON: raw, SnapshotBytesBase64: base64.StdEncoding.EncodeToString(raw), AllowedModels: []string{model}})
		case "launch-claims":
			if r.Method == http.MethodGet && len(parts) == 4 {
				mu.Lock()
				claim, ok := claims[parts[2]+":"+parts[3]]
				mu.Unlock()
				if !ok {
					http.NotFound(w, r)
					return
				}
				respond(claim)
				return
			}
			var req swarm.LaunchClaimRequest
			if json.NewDecoder(r.Body).Decode(&req) != nil || req.ExpectedGeneration != 1 || req.ExpectedRevision != swarm.Digest(raw) {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			kind, key := "worker_attempt", req.ExecutionRef.WorkerAttemptID
			if req.ExecutionRef.OwnerTurnID != "" {
				kind, key = "owner_turn", req.ExecutionRef.OwnerTurnID
			}
			mu.Lock()
			claim, ok := claims[kind+":"+key]
			if !ok {
				claim = swarm.LaunchClaim{ClaimID: swarm.NewID(), ExecutionRef: req.ExecutionRef, ProfileID: id, Generation: 1, Revision: swarm.Digest(raw), SnapshotJSON: raw, SnapshotBytesBase64: base64.StdEncoding.EncodeToString(raw), State: "starting"}
				claims[kind+":"+key] = claim
			}
			mu.Unlock()
			respond(claim)
		case "observations":
			var req swarm.LaunchObservationRequest
			if json.NewDecoder(r.Body).Decode(&req) != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			respond(map[string]any{"profile_id": id, "claim_id": req.ClaimID, "revision": req.Revision, "outcome": req.Outcome})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	ca := filepath.Join(dir, "profile-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(dir, "profile-token")
	if err := os.WriteFile(token, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.AgentProfiles = &swarm.AgentProfileBackend{BaseURL: server.URL, CAFile: ca, TokenFile: token, AllowedModels: []string{model}}
}
