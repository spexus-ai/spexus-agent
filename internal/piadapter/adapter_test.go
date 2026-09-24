package piadapter

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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

func TestResidentPiReusesProcessAndIsolatesSessions(t *testing.T) {
	base := helperAdapter(t)
	a, err := NewResident(base.profile, base.binary)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	pid := func(thread, message, want string) string {
		t.Helper()
		events, err := prompt(t, a, thread, message)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			if e.Kind == harness.EventAssistantMessageFinal {
				if !strings.Contains(e.Text, want) {
					t.Fatalf("wrong session history %q", e.Text)
				}
				_, after, ok := strings.Cut(e.Text, " pid=")
				if !ok || after == "" {
					t.Fatalf("missing process identity %q", e.Text)
				}
				return after
			}
		}
		t.Fatal("missing final event")
		return ""
	}
	first := pid("1", "alpha", "history=alpha")
	second := pid("1", "continue", "history=alpha|continue")
	if first != second {
		t.Fatalf("same session started a new process: %s != %s", first, second)
	}
	other := pid("2", "beta", "history=beta")
	if other == first {
		t.Fatal("different sessions shared the same process")
	}
	returning := pid("1", "again", "history=alpha|continue|again")
	if returning == other {
		t.Fatal("process retained the other thread's history")
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

func TestResidentPiAbortDiscardsProcessAndPreservesConversation(t *testing.T) {
	base := helperAdapter(t)
	a, err := NewResident(base.profile, base.binary)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := a.StartPrompt(ctx, harness.SessionRequest{ProjectPath: a.profile.Workspace, ChannelID: "C1", ThreadTS: "1", Prompt: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	firstPID := s.(*promptStream).cmd.Process.Pid
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
	if !cancelled || a.idle != nil {
		t.Fatal("cancelled process was retained")
	}
	events, err = prompt(t, a, "1", "continue")
	if err != nil {
		t.Fatal(err)
	}
	if a.idle == nil || a.idle.cmd.Process.Pid == firstPID {
		t.Fatal("resume did not start a fresh process")
	}
	found := false
	for _, e := range events {
		found = found || strings.Contains(e.Text, "history=wait|continue")
	}
	if !found {
		t.Fatal("durable session history was lost after cancellation")
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
		if r.Message == "activity" {
			emit(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "thinking_delta", "delta": "private reasoning must not leave Pi"}})
			emit(map[string]any{"type": "tool_execution_start", "toolName": "read"})
			emit(map[string]any{"type": "tool_execution_end", "toolName": "read"})
			emit(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": "public draft"}})
			emit(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "stop", "content": []map[string]any{{"type": "text", "text": "final"}}}})
			emit(map[string]any{"type": "agent_settled"})
			continue
		}
		if r.Message == "two-messages" || r.Message == "no-final" {
			emit(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": "UNTRUSTED DRAFT"}})
			if r.Message == "two-messages" {
				emit(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "stop", "content": []map[string]any{{"type": "text", "text": "draft must not be final"}}}})
				emit(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "stop", "content": []map[string]any{{"type": "text", "text": `{"outcome":"succeeded"}`}}}})
			}
			emit(map[string]any{"type": "agent_settled"})
			continue
		}
		if r.Message == "phased-final" {
			data, err := os.ReadFile(os.Getenv("SPEXUS_PI_PHASED_FIXTURE"))
			if err != nil {
				os.Exit(4)
			}
			var event any
			if json.Unmarshal(data, &event) != nil {
				os.Exit(5)
			}
			emit(event)
			emit(map[string]any{"type": "agent_settled"})
			continue
		}
		if r.Message == "provider-error" {
			emit(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "error", "errorMessage": "test provider rejected request"}})
			emit(map[string]any{"type": "agent_settled"})
			continue
		}
		emit(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": fmt.Sprintf("history=%s model=%s system=%s cwd=%s pid=%d", history, arg("--model"), arg("--system-prompt"), cwd, os.Getpid())}})
		if r.Message != "wait" {
			emit(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "stop", "content": []map[string]any{{"type": "text", "text": fmt.Sprintf("history=%s model=%s system=%s cwd=%s pid=%d", history, arg("--model"), arg("--system-prompt"), cwd, os.Getpid())}}}})
			emit(map[string]any{"type": "agent_end"})
			emit(map[string]any{"type": "agent_settled"})
		}
	}
	os.Exit(0)
}

func TestActivityEventsDoNotLeakThinkingText(t *testing.T) {
	a := helperAdapter(t)
	events, err := prompt(t, a, "activity", "activity")
	if err != nil {
		t.Fatal(err)
	}
	var kinds []harness.EventKind
	for _, event := range events {
		kinds = append(kinds, event.Kind)
		if strings.Contains(event.Text, "private reasoning") {
			t.Fatalf("thinking content escaped Pi: %+v", event)
		}
	}
	want := []harness.EventKind{harness.EventSessionStarted, harness.EventAssistantThinking, harness.EventToolStarted, harness.EventToolFinished, harness.EventAssistantMessageChunk, harness.EventAssistantMessageFinal, harness.EventSessionDone}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("events=%v, want %v", kinds, want)
	}
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

func TestActualPiPhasesExcludeCommentaryActions(t *testing.T) {
	path, err := filepath.Abs("testdata/phased-final.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPEXUS_PI_PHASED_FIXTURE", path)
	a := helperAdapter(t)
	events, err := prompt(t, a, "phases", "phased-final")
	if err != nil {
		t.Fatal(err)
	}
	final := ""
	for _, event := range events {
		if event.Kind == harness.EventAssistantMessageFinal {
			final = event.Text
		}
	}
	var output struct {
		Actions []json.RawMessage `json:"actions"`
		Reply   string            `json:"reply"`
	}
	if err := json.Unmarshal([]byte(final), &output); err != nil {
		t.Fatalf("commentary contaminated final JSON: %v", err)
	}
	if len(output.Actions) != 0 || !strings.Contains(output.Reply, "Ожидаю результаты") {
		t.Fatalf("executed commentary instead of actual final: %s", final)
	}
}
func TestFinalPhaseSelection(t *testing.T) {
	signature := func(phase string) string {
		b, _ := json.Marshal(map[string]any{"v": 1, "id": "message-id", "phase": phase})
		return string(b)
	}
	for _, tc := range []struct {
		name   string
		blocks []textBlock
		want   string
	}{
		{"legacy plain IDs", []textBlock{{Type: "text", Text: "one", TextSignature: "msg_legacy"}, {Type: "text", Text: "two"}}, "onetwo"},
		{"unphased v1", []textBlock{{Type: "text", Text: "plain", TextSignature: `{"v":1,"id":"msg"}`}}, "plain"},
		{"commentary only", []textBlock{{Type: "text", Text: `{"actions":["unsafe"]}`, TextSignature: signature("commentary")}}, ""},
		{"final beats unphased", []textBlock{{Type: "text", Text: "draft"}, {Type: "text", Text: "final", TextSignature: signature("final_answer")}}, "final"},
		{"multiple final blocks", []textBlock{{Type: "text", Text: "A", TextSignature: signature("final_answer")}, {Type: "text", Text: "ignored", TextSignature: signature("commentary")}, {Type: "text", Text: "B", TextSignature: signature("final_answer")}}, "AB"},
		{"empty final is not commentary fallback", []textBlock{{Type: "text", Text: "draft"}, {Type: "text", Text: "", TextSignature: signature("final_answer")}}, ""},
		{"unknown phase", []textBlock{{Type: "text", Text: "not final", TextSignature: signature("future")}}, ""},
		{"broken phase metadata", []textBlock{{Type: "text", Text: "do not promote", TextSignature: `{"v":1,"phase":"commentary"`}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := finalAssistantText(tc.blocks); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}
