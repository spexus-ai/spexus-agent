package swarmrunner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

// Exercises the installed Pi process against a local model endpoint. A normal
// owner session must retain event history without retaining copies of its role
// contract as user messages.
func TestRealPiOwnerContractDoesNotAccumulateInTurnHistory(t *testing.T) {
	binary := os.Getenv("SPEXUS_TEST_PI_BIN")
	if binary == "" {
		t.Skip("set SPEXUS_TEST_PI_BIN to exercise installed Pi")
	}
	dir := t.TempDir()
	piHome := filepath.Join(dir, "pi-home")
	t.Setenv("PI_CODING_AGENT_DIR", piHome)
	if err := os.MkdirAll(piHome, 0700); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []any{
			map[string]any{"id": "test", "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": `{"actions":[],"reply":"ok"}`}, "finish_reason": nil}}},
			map[string]any{"id": "test", "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 12, "completion_tokens": 1, "total_tokens": 13}},
		} {
			data, _ := json.Marshal(chunk)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	models := map[string]any{"providers": map[string]any{"prototype-test": map[string]any{"baseUrl": server.URL + "/v1", "api": "openai-completions", "apiKey": "local-test-only", "models": []any{map[string]any{"id": "test-model", "contextWindow": 32000, "maxTokens": 1000}}}}}
	data, _ := json.Marshal(models)
	if err := os.WriteFile(filepath.Join(piHome, "models.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	c := Config{
		TenantID: tenant, ProjectID: project, AgentID: "owner",
		Role: "owner", WireVersion: 2, PiBinary: binary,
		Workspace: dir, StateDirectory: filepath.Join(dir, "state"),
	}
	p := profile{TextProfile: swarm.TextProfile{ID: "owner", Model: "prototype-test/test-model", Reasoning: "off", Prompt: "PROFILE_RULE", Tools: []string{}, Extensions: []string{}}}
	m, err := newModel(c, p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	for _, marker := range []string{"FIRST_EVENT", "SECOND_EVENT"} {
		payload, _ := json.Marshal(map[string]string{"text": marker})
		d := swarm.Delivery{Envelope: swarm.Envelope{ProtocolVersion: 2, Type: "agent.input", FeatureID: feature, Payload: payload}}
		r := &Runner{cfg: c, targets: []targetProfile{}}
		input, err := r.input(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(input, "Dispatch data:") || strings.Contains(input, "Wire v2 human requests:") {
			t.Fatal("static instructions leaked into turn input")
		}
		if _, _, err := m.Run(ctx, c.session(d), input); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("model requests=%d, want 2", len(requests))
	}
	for i, req := range requests {
		messages, ok := req["messages"].([]any)
		if !ok {
			t.Fatalf("request %d omitted messages", i)
		}
		userCount, systemCount := 0, 0
		for _, raw := range messages {
			message, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			body, _ := json.Marshal(message["content"])
			text := string(body)
			switch message["role"] {
			case "system":
				systemCount++
				if !strings.Contains(text, "PROFILE_RULE") || !strings.Contains(text, "Wire v2 human requests:") || !strings.Contains(text, "Dispatch data:") {
					t.Fatalf("request %d lacks the owner contract in system context", i)
				}
			case "user":
				userCount++
				if strings.Contains(text, "Wire v2 human requests:") || strings.Contains(text, "Dispatch data:") {
					t.Fatalf("request %d repeated the owner contract in user history", i)
				}
			}
		}
		if systemCount != 1 || userCount != i+1 {
			t.Fatalf("request %d has %d system and %d user messages", i, systemCount, userCount)
		}
	}
}
