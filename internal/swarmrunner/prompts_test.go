package swarmrunner

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

// Test: the owner-facing resume example decodes as the actual strict wire DTO.
// Validates: SP-EP-026 W06 human.decision continuation uses outer worker_agent_id only.
func TestOwnerResumeInstructionShapeMatchesStrictDispatchPayload(t *testing.T) {
	const marker = "resume_task.data JSON shape (replace every placeholder with the trusted value): "
	instructions := modelInstructions(Config{Role: "owner", WireVersion: 2, AgentID: "orchestrator"})
	start := strings.Index(instructions, marker)
	if start < 0 {
		t.Fatal("owner instructions have no resume data shape")
	}
	after := instructions[start+len(marker):]
	line, _, found := strings.Cut(after, "\n")
	if !found {
		t.Fatal("resume data shape must be one JSON line")
	}
	var resume swarm.ResumeTaskPayload
	if err := decode([]byte(line), &resume); err != nil {
		t.Fatalf("prompt example violates strict resume schema: %v", err)
	}
	if resume.WorkerAgentID == "" || resume.Dispatch.Goal == "" || resume.Dispatch.Profile.ID == "" || resume.Dispatch.RunTimeoutSeconds == 0 {
		t.Fatal("prompt example omits required continuation fields")
	}
	var outer map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &outer); err != nil {
		t.Fatal(err)
	}
	var dispatch map[string]json.RawMessage
	if err := json.Unmarshal(outer["dispatch"], &dispatch); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"goal", "scope", "expected_result", "context", "profile", "run_timeout_seconds"} {
		if _, ok := dispatch[key]; !ok {
			t.Fatalf("nested dispatch omits %s", key)
		}
	}
	if len(dispatch) != 6 {
		t.Fatalf("nested dispatch has extra fields: %v", dispatch)
	}
	dispatch["worker_agent_id"] = json.RawMessage(`"worker-a"`)
	outer["dispatch"], _ = json.Marshal(dispatch)
	malformed, _ := json.Marshal(outer)
	if err := decode(malformed, &resume); err == nil {
		t.Fatal("strict decoder accepted worker_agent_id inside nested dispatch")
	}
}
