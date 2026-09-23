package swarmrunner

import (
	"testing"

	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

func TestOwnerPublicReplyKeepsAnswersButHidesActionChatter(t *testing.T) {
	tests := []struct {
		name    string
		actions []string
		want    string
	}{
		{name: "clarification without action", want: "Готов пояснить выбор."},
		{name: "final reviewed result", actions: []string{"task.review"}, want: "Готов пояснить выбор."},
		{name: "dispatch", actions: []string{"task.dispatch"}},
		{name: "human question", actions: []string{"task.review", "human.request"}},
		{name: "decision write", actions: []string{"human.respond"}},
		{name: "dependent resume", actions: []string{"task.resume"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			messages := make([]swarm.Envelope, len(test.actions))
			for i, kind := range test.actions {
				messages[i].Type = kind
			}
			if got := ownerPublicReply("Готов пояснить выбор.", messages); got != test.want {
				t.Fatalf("reply = %q, want %q", got, test.want)
			}
		})
	}
}
