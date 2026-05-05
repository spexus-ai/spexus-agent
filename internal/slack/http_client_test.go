package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPClientPostThreadMessageSplitsLongTextAtNearestNewline(t *testing.T) {
	t.Parallel()

	requests := make([]map[string]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat.postMessage" {
			t.Fatalf("request path = %q, want /chat.postMessage", r.URL.Path)
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		requests = append(requests, payload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1713686400.000100"}`))
	}))
	defer server.Close()

	client := &HTTPClient{
		token:   "xoxb-test",
		baseURL: server.URL + "/",
		client:  server.Client(),
	}
	text := strings.Repeat("a", slackMessageTextSoftLimit-10) + "\n" + strings.Repeat("b", 20)
	err := client.PostThreadMessage(context.Background(), Message{
		ChannelID: "C12345678",
		ThreadTS:  "1713686400.000100",
		Text:      text,
	})
	if err != nil {
		t.Fatalf("PostThreadMessage() error = %v", err)
	}

	if got, want := len(requests), 2; got != want {
		t.Fatalf("chat.postMessage calls = %d, want %d", got, want)
	}
	if requests[0]["text"] != strings.Repeat("a", slackMessageTextSoftLimit-10) {
		t.Fatalf("first text length = %d, want %d", len([]rune(requests[0]["text"])), slackMessageTextSoftLimit-10)
	}
	if requests[1]["text"] != strings.Repeat("b", 20) {
		t.Fatalf("second text = %q, want b chunk", requests[1]["text"])
	}
	if requests[0]["thread_ts"] != "1713686400.000100" || requests[1]["thread_ts"] != "1713686400.000100" {
		t.Fatalf("thread_ts values = %#v %#v, want same thread", requests[0], requests[1])
	}
}

func TestHTTPClientPostMessageSplitsLongRootMessageIntoThread(t *testing.T) {
	t.Parallel()

	requests := make([]map[string]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		requests = append(requests, payload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1713686400.000100"}`))
	}))
	defer server.Close()

	client := &HTTPClient{
		token:   "xoxb-test",
		baseURL: server.URL + "/",
		client:  server.Client(),
	}
	text := strings.Repeat("x", slackMessageTextSoftLimit-1) + "\n" + strings.Repeat("y", 25)
	posted, err := client.PostMessage(context.Background(), Message{
		ChannelID: "C12345678",
		Text:      text,
	})
	if err != nil {
		t.Fatalf("PostMessage() error = %v", err)
	}

	if posted.Timestamp != "1713686400.000100" {
		t.Fatalf("PostMessage() timestamp = %q, want first message timestamp", posted.Timestamp)
	}
	if got, want := len(requests), 2; got != want {
		t.Fatalf("chat.postMessage calls = %d, want %d", got, want)
	}
	if _, ok := requests[0]["thread_ts"]; ok {
		t.Fatalf("first request thread_ts = %q, want root message without thread_ts", requests[0]["thread_ts"])
	}
	if requests[1]["thread_ts"] != "1713686400.000100" {
		t.Fatalf("second request thread_ts = %q, want first message timestamp", requests[1]["thread_ts"])
	}
	if len([]rune(requests[0]["text"])) > slackMessageTextSoftLimit || len([]rune(requests[1]["text"])) > slackMessageTextSoftLimit {
		t.Fatalf("split chunk lengths = %d and %d, want <= %d", len([]rune(requests[0]["text"])), len([]rune(requests[1]["text"])), slackMessageTextSoftLimit)
	}
}

func TestHTTPClientUpdateMessageUsesChatUpdate(t *testing.T) {
	t.Parallel()

	requests := make([]map[string]string, 0, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat.update" {
			t.Fatalf("request path = %q, want /chat.update", r.URL.Path)
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		requests = append(requests, payload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	client := &HTTPClient{
		token:   "xoxb-test",
		baseURL: server.URL + "/",
		client:  server.Client(),
	}
	err := client.UpdateMessage(context.Background(), MessageUpdate{
		ChannelID: "C12345678",
		Timestamp: "1713686400.000100",
		Text:      "updated partial",
	})
	if err != nil {
		t.Fatalf("UpdateMessage() error = %v", err)
	}

	if got, want := len(requests), 1; got != want {
		t.Fatalf("chat.update calls = %d, want %d", got, want)
	}
	if requests[0]["channel"] != "C12345678" || requests[0]["ts"] != "1713686400.000100" || requests[0]["text"] != "updated partial" {
		t.Fatalf("chat.update payload = %#v", requests[0])
	}
}
