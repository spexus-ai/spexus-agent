package swarmslack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSetThreadStatusSetClearAndReject(t *testing.T) {
	var calls []map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/assistant.threads.setStatus" {
			t.Errorf("unexpected Slack method %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing bot authorization")
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		calls = append(calls, payload)
		if len(calls) == 3 {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "secret-value-must-not-leak"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer server.Close()

	api := NewAPI("test-token")
	api.BaseURL, api.Client = server.URL+"/", server.Client()
	ctx := context.Background()
	for _, status := range []string{"проверяю решение…", ""} {
		if err := api.SetThreadStatus(ctx, "C123ABC", "123.000456", status); err != nil {
			t.Fatal(err)
		}
	}
	if err := api.SetThreadStatus(ctx, "C123ABC", "123.000456", "проверяю решение…"); err == nil || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("Slack rejection was accepted or leaked response: %v", err)
	}
	if len(calls) != 3 || calls[0]["channel_id"] != "C123ABC" || calls[0]["thread_ts"] != "123.000456" || calls[0]["status"] != "проверяю решение…" || calls[1]["status"] != "" {
		t.Fatalf("unexpected status calls: %+v", calls)
	}
	for _, input := range []struct{ channel, thread, status string }{
		{"", "123.000456", "processing"},
		{"C123ABC\n", "123.000456", "processing"},
		{"C123ABC", "invalid", "processing"},
		{"C123ABC", "123.000456", " leading"},
		{"C123ABC", "123.000456", "line\nbreak"},
		{"C123ABC", "123.000456", strings.Repeat("x", 201)},
	} {
		if err := api.SetThreadStatus(ctx, input.channel, input.thread, input.status); err == nil {
			t.Fatalf("invalid status input accepted: %+v", input)
		}
	}
	if len(calls) != 3 {
		t.Fatalf("invalid inputs reached Slack: %d calls", len(calls))
	}
}
