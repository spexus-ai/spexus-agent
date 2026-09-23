// Package testsupport supplies a local deterministic model for opt-in tests
// using the real Pi executable. It is not imported by the runtime binary.
package testsupport

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spexus-ai/spexus-agent/internal/config"
)

type PiModel struct {
	Profile  config.AgentProfile
	Binary   string
	mu       sync.Mutex
	requests []string
}

func NewPiModel(t *testing.T) *PiModel {
	t.Helper()
	binary := os.Getenv("SPEXUS_TEST_PI_BIN")
	if binary == "" {
		t.Skip("set SPEXUS_TEST_PI_BIN for a real Pi + local model integration test")
	}
	dir := t.TempDir()
	home := filepath.Join(dir, "pi-home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PI_CODING_AGENT_DIR", home)
	m := &PiModel{Binary: binary, Profile: config.AgentProfile{ID: "prototype", Provider: "prototype-test", Model: "test-model", Thinking: "off", SystemPrompt: "SPECIFIC_AGENT_INSTRUCTION", Workspace: dir, SessionDirectory: filepath.Join(dir, "sessions"), Tools: []string{}}}
	server := httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(server.Close)
	models := map[string]any{"providers": map[string]any{"prototype-test": map[string]any{"baseUrl": server.URL + "/v1", "api": "openai-completions", "apiKey": "local-test-only", "models": []any{map[string]any{"id": "test-model", "contextWindow": 32000, "maxTokens": 1000}}}}}
	data, _ := json.Marshal(models)
	if err := os.WriteFile(filepath.Join(home, "models.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	// No automatic retries for deliberate failure fixtures.
	if err := os.WriteFile(filepath.Join(home, "settings.json"), []byte(`{"retry":{"enabled":false}}`), 0600); err != nil {
		t.Fatal(err)
	}
	return m
}

func (m *PiModel) Requests() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.requests...)
}
func (m *PiModel) serve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	data, _ := json.Marshal(body)
	m.mu.Lock()
	m.requests = append(m.requests, string(data))
	m.mu.Unlock()
	last := ""
	for _, message := range body.Messages {
		if message.Role == "user" {
			last = string(message.Content)
		}
	}
	if strings.Contains(last, "PROVIDER_ERROR") {
		http.Error(w, `{"error":{"message":"deliberate local model failure","type":"invalid_request_error"}}`, 400)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	text := "ACK"
	if strings.Contains(last, "BLOCK_UNTIL_STOP") {
		text = "WAITING"
	}
	chunk := func(delta map[string]any, finish any) {
		data, _ := json.Marshal(map[string]any{"id": "test", "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		fmt.Fprintf(w, "data: %s\n\n", data)
		w.(http.Flusher).Flush()
	}
	chunk(map[string]any{"role": "assistant", "content": text}, nil)
	if text == "WAITING" {
		<-r.Context().Done()
		return
	}
	chunk(map[string]any{}, "stop")
	fmt.Fprint(w, "data: [DONE]\n\n")
}
