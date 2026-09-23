package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOfflineReconcileRejectsWrongContainerEvidence(t *testing.T) {
	id := strings.Repeat("a", 64)
	required := map[string]string{"io.spexus.swarm.tenant-id": "tenant", "io.spexus.swarm.project-id": "project", "io.spexus.swarm.role": "worker", "io.spexus.swarm.agent-id": "worker-a", "io.spexus.swarm.instance-id": "old-instance"}
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		valid  bool
	}{
		{"stopped matching process", func(map[string]any) {}, true},
		{"another container ID", func(d map[string]any) { d["Id"] = strings.Repeat("b", 64) }, false},
		{"old process still running", func(d map[string]any) { d["State"].(map[string]any)["Running"] = true }, false},
		{"process still has PID", func(d map[string]any) { d["State"].(map[string]any)["Pid"] = 123 }, false},
		{"restarting process", func(d map[string]any) { d["State"].(map[string]any)["Restarting"] = true }, false},
		{"unrelated stopped runner", func(d map[string]any) {
			d["Config"].(map[string]any)["Labels"].(map[string]string)["io.spexus.swarm.agent-id"] = "other"
		}, false},
		{"wrong old instance", func(d map[string]any) {
			d["Config"].(map[string]any)["Labels"].(map[string]string)["io.spexus.swarm.instance-id"] = "another"
		}, false},
		{"wrong scope", func(d map[string]any) {
			d["Config"].(map[string]any)["Labels"].(map[string]string)["io.spexus.swarm.project-id"] = "another"
		}, false},
		{"missing labels", func(d map[string]any) { delete(d, "Config") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			labels := map[string]string{}
			for k, v := range required {
				labels[k] = v
			}
			d := map[string]any{"Id": id, "Config": map[string]any{"Labels": labels}, "State": map[string]any{"Running": false, "Pid": 0, "Status": "exited"}}
			tc.mutate(d)
			raw, _ := json.Marshal(d)
			err := validateStopped(raw, id, required)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t err=%v", tc.valid, err)
			}
		})
	}
}
