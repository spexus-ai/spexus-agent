package swarmrunner

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

func TestOwnerHumanResponseReferencesTrustedSlackSource(t *testing.T) {
	r, _ := runnerFixture(t, http.NotFoundHandler())
	r.cfg.WireVersion = 2
	r.cfg.Role = "owner"
	r.cfg.AgentID = "orchestrator"
	d := dispatchFixture(r)
	d.ProtocolVersion = 2
	d.Type = "agent.input"
	d.ToAgentID = "orchestrator"
	d.FromAgentID = "human"
	d.JobID, d.AttemptID = "", ""
	d.OwnerTurnID = ""
	requestID := swarm.NewID()
	sourceTS := "1790193461.461559"
	data, err := json.Marshal(swarm.HumanRespondPayload{RequestID: requestID, SourceMessageTS: sourceTS, Kind: "answer", Text: "Я не знаю, что такое P3"})
	if err != nil {
		t.Fatal(err)
	}
	output, err := json.Marshal(ownerOutput{Actions: []action{{Kind: "decide_human", Data: data}}, Reply: "Сохраняю ваш ответ."})
	if err != nil {
		t.Fatal(err)
	}
	_, messages, err := r.ownerActions(d, swarm.NewID(), string(output))
	if err != nil || len(messages) != 1 || messages[0].Type != "human.respond" {
		t.Fatalf("human response action: %v %+v", err, messages)
	}
	if err := swarm.ValidateEnvelope(messages[0]); err != nil {
		t.Fatal(err)
	}
	var got swarm.HumanRespondPayload
	if err := json.Unmarshal(messages[0].Payload, &got); err != nil {
		t.Fatal(err)
	}
	if got.RequestID != requestID || got.SourceMessageTS != sourceTS || got.Kind != "answer" || got.Text != "Я не знаю, что такое P3" {
		t.Fatalf("response changed source or text: %+v", got)
	}
	for _, raw := range []string{
		`{"actions":[{"kind":"decide_human","data":{"request_id":"` + requestID + `","kind":"answer","text":"да"}}],"reply":""}`,
		`{"actions":[{"kind":"decide_human","data":{"request_id":"` + requestID + `","source_message_ts":"bad","kind":"answer","text":"да"}}],"reply":""}`,
	} {
		if _, commands, err := r.ownerActions(d, swarm.NewID(), raw); err == nil || commands != nil {
			t.Fatalf("accepted response without a valid source: %s", raw)
		}
	}
}
