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

// Test: answer syntax keeps a full request ID and never infers a decision
// from an ordinary message or an empty deny.
// Validates: AC-464 (REQ-391 - explicit human decision with source).
func TestParseHumanAnswer(t *testing.T) {
	id := "123e4567-e89b-42d3-a456-426614174000"
	for _, tc := range []struct {
		text, kind, option, body string
		valid                    bool
	}{
		{"!answer " + id + " a because it is smaller", "answer", "a", "because it is smaller", true},
		{"!answer " + id + " text use the existing API", "answer", "", "use the existing API", true},
		{"!answer " + id + " deny unsafe change", "deny", "", "unsafe change", true},
		{"!answer " + id[:8] + " a", "", "", "", false},
		{"!answer " + id + " deny", "", "", "", false},
		{"approved", "", "", "", false},
	} {
		got, err := parseAnswer(tc.text)
		if (err == nil) != tc.valid {
			t.Errorf("%q: err=%v", tc.text, err)
			continue
		}
		if tc.valid && (got.RequestID != id || got.Kind != tc.kind || got.OptionID != tc.option || got.Text != tc.body) {
			t.Errorf("%q: %+v", tc.text, got)
		}
	}
}

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
			upper, err := a.ScanThread(context.Background(), "C", "1.000001", "1.000000", func(ts, user, text, thread string) error {
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
