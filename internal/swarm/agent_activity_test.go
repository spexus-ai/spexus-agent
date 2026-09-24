package swarm

import (
	"testing"
)

func TestOwnerActivityIsScopedToRunningTurn(t *testing.T) {
	f := newFixture(t)
	turnID := f.ownerTurn()
	active := HeartbeatRequest{InstanceID: f.instances["orchestrator"], ActiveOwnerTurnID: &turnID, ActivityPhase: "thinking"}
	f.call("orchestrator", "POST", "/agents/self/heartbeat", active, 200)
	for _, agent := range f.history().Agents {
		if agent.AgentID == "orchestrator" && (agent.OwnerTurnID != turnID || agent.ActivityPhase != "thinking") {
			t.Fatalf("owner activity=%+v", agent)
		}
	}
	active.ActivityPhase = "tool"
	f.call("orchestrator", "POST", "/agents/self/heartbeat", active, 200)
	active.ActivityPhase = "responding"
	f.call("orchestrator", "POST", "/agents/self/heartbeat", active, 200)
	active.ActivityPhase = "private model text"
	f.call("orchestrator", "POST", "/agents/self/heartbeat", active, 400)
	active.ActivityPhase = "thinking"
	active.ActiveOwnerTurnID = nil
	f.call("orchestrator", "POST", "/agents/self/heartbeat", active, 400)
	f.call("orchestrator", "POST", "/agents/self/heartbeat", HeartbeatRequest{InstanceID: f.instances["orchestrator"]}, 200)
	for _, agent := range f.history().Agents {
		if agent.AgentID == "orchestrator" && (agent.OwnerTurnID != "" || agent.ActivityPhase != "") {
			t.Fatalf("owner activity survived idle heartbeat: %+v", agent)
		}
	}
}

func TestWorkerActivityIsScopedToRunningAttempt(t *testing.T) {
	f := newFixture(t)
	turnID := f.ownerTurn()
	dispatch := f.dispatch(turnID, "worker-a")
	f.post("orchestrator", dispatch, 201)
	f.started(dispatch)
	attemptID := dispatch.AttemptID
	active := HeartbeatRequest{InstanceID: f.instances["worker-a"], ActiveAttemptID: &attemptID, ActivityPhase: "tool"}
	f.call("worker-a", "POST", "/agents/self/heartbeat", active, 200)
	for _, agent := range f.history().Agents {
		if agent.AgentID == "worker-a" && (agent.ActiveAttemptID != attemptID || agent.ActivityPhase != "tool") {
			t.Fatalf("worker activity=%+v", agent)
		}
	}
	active.InstanceID = f.instances["worker-b"]
	f.call("worker-b", "POST", "/agents/self/heartbeat", active, 403)
	f.call("worker-a", "POST", "/agents/self/heartbeat", HeartbeatRequest{InstanceID: f.instances["worker-a"]}, 200)
	for _, agent := range f.history().Agents {
		if agent.AgentID == "worker-a" && (agent.ActiveAttemptID != "" || agent.ActivityPhase != "") {
			t.Fatalf("worker activity survived idle heartbeat: %+v", agent)
		}
	}
}
