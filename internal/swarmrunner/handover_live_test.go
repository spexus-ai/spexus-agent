//go:build handoverlive

package swarmrunner

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

// TestRealPiWorkspaceHandover is an opt-in, paid-model experiment. It runs the
// real coordinator protocol and three real Pi sessions against one disposable
// prepared workspace. The reviewer is started only after the coder exits.
func TestRealPiWorkspaceHandover(t *testing.T) {
	if os.Getenv("SPEXUS_TEST_HANDOVER") != "1" {
		t.Skip("set SPEXUS_TEST_HANDOVER=1 to run the live Pi handover experiment")
	}
	dockerImage := os.Getenv("SPEXUS_TEST_HANDOVER_CONTAINER_IMAGE")
	if dockerImage == "" {
		t.Skip("set SPEXUS_TEST_HANDOVER_CONTAINER_IMAGE to an image built from this checkout")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatal(err)
	}
	marker := "checkpoint-" + swarm.NewID()
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(workspace, path), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("package.json", `{"type":"module","scripts":{"test":"node --test"}}`+"\n")
	write("calc.js", fmt.Sprintf("export const Checkpoint = %q;\n\nexport function add(a, b) { return a - b; }\n", marker))
	write("calc.test.js", "import { test } from 'node:test';\nimport { strict as assert } from 'node:assert';\nimport { add } from './calc.js';\n\ntest('add sums two numbers', () => assert.equal(add(2, 3), 5));\n")
	profiles := map[string]swarm.TextProfile{
		"orchestrator": {
			ID: "orchestrator", Model: "openai-codex/gpt-6-luna", Reasoning: "low", Tools: []string{}, Extensions: []string{},
			Prompt: `This is a two-stage workspace handover experiment. On the initial agent.input, dispatch exactly one job to coder: fix add in calc.js so node --test passes, preserving Checkpoint. On coder's task.result, review that exact result; if it succeeded, also dispatch exactly one job to reviewer in the same actions array, after the review action. The reviewer must inspect the actual calc.js and calc.test.js files in its mounted workspace, verify the fix, and report the exact Checkpoint value found there. Do not include or guess the Checkpoint value in the dispatch. On reviewer's task.result, review it and give a concise final reply. Never dispatch reviewer before the coder result or dispatch coder twice. Both workers have their own supplied profiles. Use only the structured runtime protocol.`,
		},
		"coder": {
			ID: "coder", Model: "openai-codex/gpt-6-luna", Reasoning: "low", Tools: []string{"read", "edit", "write", "bash"}, Extensions: []string{},
			Prompt: `You are the coding worker. In your current workspace, inspect calc.js, fix add so node --test passes, and run that test with bash. Preserve the Checkpoint constant. Report the actual file change and test result in structured evidence. Do not ask another agent to edit for you.`,
		},
		"reviewer": {
			ID: "reviewer", Model: "openai-codex/gpt-6-luna", Reasoning: "low", Tools: []string{"read"}, Extensions: []string{},
			Prompt: `You are an independent read-only reviewer. Use the read tool to inspect calc.js and calc.test.js in your current workspace. Verify that add satisfies the test. Quote the exact Checkpoint constant value in your evidence so the experiment can prove you saw the handed-over filesystem. Do not edit files or claim to have run a test.`,
		},
	}
	modelConfig := swarm.Config{WireVersion: 2, TenantID: swarm.NewID(), ProjectID: swarm.NewID(), Human: &swarm.HumanConfig{BaseURL: "https://unused.invalid", CAFile: "/unused", TokenFile: "/unused", EpicID: swarm.NewID(), WriterID: swarm.NewID(), WorkspaceID: "handover-test"}}
	if err := os.MkdirAll(filepath.Join(root, "profiles"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"orchestrator", "coder", "reviewer"} {
		b, err := json.Marshal(profiles[id])
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, "profiles", id+".json")
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
		modelConfig.Profiles = append(modelConfig.Profiles, swarm.ProfileSnapshot{Bytes: b})
		role := "worker"
		if id == "orchestrator" {
			role = "owner"
		}
		modelConfig.Agents = append(modelConfig.Agents, swarm.AgentConfig{AgentID: id, Role: role, ProfileID: id, CredentialSHA256: swarm.Digest([]byte("handover-token-" + id))})
	}
	featureID := swarm.NewID()
	modelConfig.Features = []swarm.Feature{{FeatureID: featureID, TenantID: modelConfig.TenantID, ProjectID: modelConfig.ProjectID, OwnerAgentID: "orchestrator", ChannelID: "C1234567890", ThreadTS: "1234567890.000001", AllowedActorIDs: []string{"U1234567890"}}}
	store, err := swarm.Open(ctx, filepath.Join(root, "coordinator.db"), modelConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// This fixture has no Slack transport. Establish an empty source cutover
	// before releasing wire-v2's startup barrier and admitting the test input.
	if _, err := store.SlackWatermark(ctx, featureID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRecoveryBarrier(ctx, featureID, ""); err != nil {
		t.Fatal(err)
	}
	server, coordinatorURL := newHandoverServer(t, store.Handler())
	defer server.Close()
	caPath := filepath.Join(root, "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	runContainerHandover(t, ctx, root, workspace, marker, featureID, modelConfig, store, caPath, coordinatorURL, dockerImage)
}
