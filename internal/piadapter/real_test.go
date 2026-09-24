package piadapter

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

	"github.com/spexus-ai/spexus-agent/internal/config"
	"github.com/spexus-ai/spexus-agent/internal/harness"
)

// Opt-in exercises the installed Pi process, RPC, provider client and real
// session files against a deterministic local model endpoint (no paid calls).
func TestRealPiConversation(t *testing.T) {
	binary := os.Getenv("SPEXUS_TEST_PI_BIN")
	if binary == "" {
		t.Skip("set SPEXUS_TEST_PI_BIN to exercise installed Pi")
	}
	dir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(dir, "pi-home"))
	if err := os.MkdirAll(filepath.Join(dir, "pi-home"), 0700); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		requests = append(requests, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []any{
			map[string]any{"id": "test", "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": "ACK"}, "finish_reason": nil}}},
			map[string]any{"id": "test", "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 12, "completion_tokens": 1, "total_tokens": 13}},
		} {
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	models := map[string]any{"providers": map[string]any{"prototype-test": map[string]any{"baseUrl": server.URL + "/v1", "api": "openai-completions", "apiKey": "local-test-only", "models": []any{map[string]any{"id": "test-model", "contextWindow": 32000, "maxTokens": 1000}}}}}
	data, _ := json.Marshal(models)
	if err := os.WriteFile(filepath.Join(dir, "pi-home", "models.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := NewResident(config.AgentProfile{ID: "prototype", Provider: "prototype-test", Model: "test-model", Thinking: "off", SystemPrompt: "SPECIFIC_AGENT_INSTRUCTION", Workspace: dir, SessionDirectory: filepath.Join(dir, "sessions"), Tools: []string{}}, binary)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	firstPID := 0
	for i, turn := range []struct{ thread, prompt string }{{"1", "FIRST_THREAD_SECRET"}, {"1", "continue"}, {"2", "SECOND_THREAD_SECRET"}, {"1", "again"}} {
		s, err := a.StartPrompt(ctx, harness.SessionRequest{ChannelID: "C1", ProjectPath: dir, ThreadTS: turn.thread, Prompt: turn.prompt})
		if err != nil {
			t.Fatal(err)
		}
		events, err := harness.CollectPromptStream(s)
		if err != nil {
			t.Fatalf("real Pi: %v; events=%+v", err, events)
		}
		var final string
		for _, e := range events {
			if e.Kind == harness.EventAssistantMessageFinal {
				final = e.Text
			}
		}
		if final != "ACK" {
			t.Fatalf("unexpected model text %q", final)
		}
		if a.idle == nil {
			t.Fatal("resident process was not retained")
		}
		if i == 0 {
			firstPID = a.idle.cmd.Process.Pid
		} else if i == 1 && a.idle.cmd.Process.Pid != firstPID {
			t.Fatal("consecutive turns restarted Pi")
		} else if i == 2 && a.idle.cmd.Process.Pid == firstPID {
			t.Fatal("another thread reused the first thread's process")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 4 {
		t.Fatalf("model requests=%d", len(requests))
	}
	for i, r := range requests {
		b, _ := json.Marshal(r)
		body := string(b)
		if !strings.Contains(body, "SPECIFIC_AGENT_INSTRUCTION") || r["model"] != "test-model" {
			t.Fatalf("profile not applied on turn %d", i)
		}
		if i == 2 && strings.Contains(body, "FIRST_THREAD_SECRET") {
			t.Fatal("thread 1 leaked into thread 2")
		}
		if i == 3 && (!strings.Contains(body, "FIRST_THREAD_SECRET") || strings.Contains(body, "SECOND_THREAD_SECRET")) {
			t.Fatal("thread history lost or mixed")
		}
	}
}
