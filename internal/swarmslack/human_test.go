package swarmslack

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Test: paged replies retain their author and timestamp while rejecting an
// incomplete cursor chain and respecting Slack's Retry-After signal.
// Validates: AC-432 (REQ-350 - complete history before recovery opens).
func TestScanThreadPaginationAndFailures(t *testing.T) {
	for _, mode := range []string{"complete", "incomplete", "limited"} {
		t.Run(mode, func(t *testing.T) {
			pages := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				pages++
				if r.Header.Get("Authorization") != "Bearer test" || r.URL.Query().Get("channel") != "C" || r.URL.Query().Get("ts") != "1.000001" || r.URL.Query().Get("oldest") != "1.000000" || r.URL.Query().Get("inclusive") != "true" {
					t.Error("wrong scoped history request")
				}
				if mode == "limited" {
					w.Header().Set("Retry-After", "7")
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				if pages == 1 {
					cursor := "next"
					if mode == "incomplete" {
						cursor = ""
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "has_more": true, "messages": []any{map[string]string{"ts": "1.000001", "user": "U", "text": "root"}}, "response_metadata": map[string]string{"next_cursor": cursor}})
					return
				}
				if r.URL.Query().Get("cursor") != "next" {
					t.Error("cursor missing")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": []any{map[string]string{"ts": "1.000002", "thread_ts": "1.000001", "user": "U", "text": "!stop"}}})
			}))
			defer server.Close()
			a := NewAPI("test")
			a.BaseURL, a.Client = server.URL+"/", server.Client()
			var got []string
			upper, err := a.ScanThread(context.Background(), "C", "1.000001", "1.000000", func(ts, user, text, thread string, edited bool) error {
				if edited {
					t.Error("unedited message marked edited")
				}
				got = append(got, strings.Join([]string{ts, user, text, thread}, "/"))
				return nil
			})
			switch mode {
			case "complete":
				if err != nil || upper != "1.000002" || len(got) != 1 || got[0] != "1.000002/U/!stop/1.000001" {
					t.Fatalf("upper=%q got=%v err=%v", upper, got, err)
				}
			case "incomplete":
				if err == nil {
					t.Fatal("incomplete pagination accepted")
				}
			case "limited":
				var rate *RateLimitError
				if !errors.As(err, &rate) || rate.After != 7*time.Second {
					t.Fatalf("rate error=%v", err)
				}
			}
		})
	}
}

// Test: bot-token workspace identity must match the configured human scope.
// Validates: AC-464 (REQ-391 - trusted Slack source provenance).
func TestVerifySlackBotWorkspace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/auth.test" || r.Header.Get("Authorization") != "Bearer test" {
			t.Error("wrong auth.test request")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "team_id": "T-real"})
	}))
	defer server.Close()
	a := NewAPI("test")
	a.BaseURL, a.Client = server.URL+"/", server.Client()
	if err := a.VerifyWorkspace(context.Background(), "T-real"); err != nil {
		t.Fatal(err)
	}
	if err := a.VerifyWorkspace(context.Background(), "T-other"); err == nil {
		t.Fatal("bot token accepted for a different workspace")
	}
}
