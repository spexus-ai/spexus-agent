package piadapter

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/config"
	"github.com/spexus-ai/spexus-agent/internal/harness"
)

func helperAdapter(t *testing.T) *Adapter {
	t.Helper()
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPEXUS_PI_HELPER", "1")
	binary := filepath.Join(dir, "pi")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexec '"+strings.ReplaceAll(exe, "'", "'\\''")+"' -test.run=^TestPiHelperProcess$ -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a, err := New(config.AgentProfile{ID: "alpha", Provider: "test", Model: "test-model", Thinking: "off", SystemPrompt: "agent instruction", Workspace: dir, SessionDirectory: filepath.Join(dir, "sessions")}, binary)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func prompt(t *testing.T, a *Adapter, thread, message string) ([]harness.Event, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := a.StartPrompt(ctx, harness.SessionRequest{ProjectPath: a.profile.Workspace, ChannelID: "C1", ThreadTS: thread, Prompt: message})
	if err != nil {
		return nil, err
	}
	defer s.Close()
	return harness.CollectPromptStream(s)
}

func TestPiSessionsPersistAndIsolateThreads(t *testing.T) {
	a := helperAdapter(t)
	for _, tc := range []struct{ thread, prompt, want string }{{"1", "alpha", "history=alpha"}, {"2", "beta", "history=beta"}, {"1", "continue", "history=alpha|continue"}} {
		events, err := prompt(t, a, tc.thread, tc.prompt)
		if err != nil {
			t.Fatal(err)
		}
		var final string
		for _, e := range events {
			if e.Kind == harness.EventAssistantMessageFinal {
				final = e.Text
			}
		}
		if !strings.Contains(final, tc.want) || !strings.Contains(final, "test-model") || !strings.Contains(final, "agent instruction") || !strings.Contains(final, a.profile.Workspace) {
			t.Fatalf("wrong context %q", final)
		}
	}
}

func TestPiAbortPreservesConversation(t *testing.T) {
	a := helperAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := a.StartPrompt(ctx, harness.SessionRequest{ProjectPath: a.profile.Workspace, ChannelID: "C1", ThreadTS: "1", Prompt: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	for e := range s.Events() {
		if e.Kind == harness.EventAssistantMessageChunk {
			break
		}
	}
	if err := a.Cancel(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	events, err := harness.CollectPromptStream(s)
	if err != nil {
		t.Fatal(err)
	}
	cancelled := false
	for _, e := range events {
		cancelled = cancelled || e.Kind == harness.EventSessionCancelled
	}
	if !cancelled {
		t.Fatal("missing cancelled event")
	}
	events, err = prompt(t, a, "1", "continue")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		found = found || strings.Contains(e.Text, "history=wait|continue")
	}
	if !found {
		t.Fatal("conversation lost after abort")
	}
}

func TestPiFailuresAreNotSuccessfulTurns(t *testing.T) {
	a := helperAdapter(t)
	for _, message := range []string{"provider-error", "reject", "malformed", "exit"} {
		t.Run(message, func(t *testing.T) {
			events, err := prompt(t, a, message, message)
			if err == nil {
				t.Fatal("expected failure")
			}
			for _, e := range events {
				if e.Kind == harness.EventSessionDone {
					t.Fatal("failure reported as success")
				}
			}
		})
	}
	if _, err := a.StartPrompt(context.Background(), harness.SessionRequest{ProjectPath: t.TempDir(), ThreadTS: "x", Prompt: "hi"}); err == nil {
		t.Fatal("mismatched workspace accepted")
	}
}

// This process speaks actual JSONL framing and survives until stdin closes,
// allowing cancellation and child-process lifetime to be tested without an LLM.
func TestPiHelperProcess(t *testing.T) {
	if os.Getenv("SPEXUS_PI_HELPER") != "1" {
		return
	}
	args := os.Args
	arg := func(name string) string {
		for i, s := range args {
			if s == name && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	file := arg("--session")
	cwd, _ := os.Getwd()
	enc := json.NewEncoder(os.Stdout)
	emit := func(v any) { _ = enc.Encode(v) }
	scan := bufio.NewScanner(os.Stdin)
	for scan.Scan() {
		var r struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if json.Unmarshal(scan.Bytes(), &r) != nil {
			os.Exit(2)
		}
		if r.Type == "abort" {
			emit(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "aborted"}})
			emit(map[string]any{"type": "agent_settled"})
			continue
		}
		if r.Type != "prompt" {
			continue
		}
		if r.Message == "reject" {
			emit(map[string]any{"type": "response", "command": "prompt", "success": false, "error": "model unavailable"})
			continue
		}
		if r.Message == "malformed" {
			fmt.Println("broken json")
			continue
		}
		if r.Message == "exit" {
			os.Exit(3)
		}
		old, _ := os.ReadFile(file)
		history := string(old)
		if history != "" {
			history += "|"
		}
		history += r.Message
		_ = os.WriteFile(file, []byte(history), 0600)
		emit(map[string]any{"type": "response", "command": "prompt", "success": true})
		if r.Message == "two-messages" || r.Message == "no-final" {
			emit(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": "UNTRUSTED DRAFT"}})
			if r.Message == "two-messages" {
				emit(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "stop", "content": []map[string]any{{"type": "text", "text": "draft must not be final"}}}})
				emit(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "stop", "content": []map[string]any{{"type": "text", "text": `{"outcome":"succeeded"}`}}}})
			}
			emit(map[string]any{"type": "agent_settled"})
			continue
		}
		if r.Message == "provider-error" {
			emit(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "test provider rejected request"}})
			emit(map[string]any{"type": "agent_settled"})
			continue
		}
		emit(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": fmt.Sprintf("history=%s model=%s system=%s cwd=%s", history, arg("--model"), arg("--system-prompt"), cwd)}})
		if r.Message != "wait" {
			emit(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "stop", "content": []map[string]any{{"type": "text", "text": fmt.Sprintf("history=%s model=%s system=%s cwd=%s", history, arg("--model"), arg("--system-prompt"), cwd)}}}})
			emit(map[string]any{"type": "agent_end"})
			emit(map[string]any{"type": "agent_settled"})
		}
	}
	os.Exit(0)
}

func TestFinalMessageIsNotDraftDeltaConcatenation(t *testing.T) {
	a := helperAdapter(t)
	for _, tc := range []struct{ prompt, want string }{{"two-messages", `{"outcome":"succeeded"}`}, {"no-final", ""}} {
		events, err := prompt(t, a, tc.prompt, tc.prompt)
		if err != nil {
			t.Fatal(err)
		}
		final := ""
		for _, event := range events {
			if event.Kind == harness.EventAssistantMessageFinal {
				final = event.Text
			}
		}
		if final != tc.want {
			t.Fatalf("final=%q want=%q", final, tc.want)
		}
	}
}
