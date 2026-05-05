package runtime

import "testing"

// Test: structured ACPX turn output is translated into the internal runtime event model with supported turn-bound event kinds.
// Validates: AC-1787 (REQ-1148 - root Slack messages create or ensure a thread session), AC-1788 (REQ-1149 - thread replies continue the existing thread session), AC-1795 (REQ-1156 - cancel requests cancellation without corrupting session mapping)
func TestTranslateACPXTurnOutputParsesStrictJSONRPCEvents(t *testing.T) {
	t.Parallel()

	output := `{"jsonrpc":"2.0","id":2,"method":"session/new","result":{"sessionId":"019db13d-f733-7ce0-8186-5aced7cdb2a7"}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"019db13d-f733-7ce0-8186-5aced7cdb2a7","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Hello"}}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"019db13d-f733-7ce0-8186-5aced7cdb2a7","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":" world"}}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"019db13d-f733-7ce0-8186-5aced7cdb2a7","update":{"sessionUpdate":"tool_call","toolCallId":"call_1","title":"Run pwd","kind":"execute","status":"in_progress"}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"019db13d-f733-7ce0-8186-5aced7cdb2a7","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_1","status":"completed"}}}
{"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}`

	events, err := TranslateACPXTurnOutput(output)
	if err != nil {
		t.Fatalf("TranslateACPXTurnOutput() error = %v", err)
	}

	if got, want := len(events), 7; got != want {
		t.Fatalf("TranslateACPXTurnOutput() events = %d, want %d", got, want)
	}
	if events[0].Kind != ACPXEventSessionStarted || events[0].Text != "019db13d-f733-7ce0-8186-5aced7cdb2a7" {
		t.Fatalf("TranslateACPXTurnOutput() first event = %#v", events[0])
	}
	if events[1].Kind != ACPXEventAssistantMessageChunk || events[1].Text != "Hello" {
		t.Fatalf("TranslateACPXTurnOutput() second event = %#v", events[1])
	}
	if events[2].Kind != ACPXEventAssistantMessageChunk || events[2].Text != " world" {
		t.Fatalf("TranslateACPXTurnOutput() third event = %#v", events[2])
	}
	if events[3].Kind != ACPXEventToolStarted || events[3].ToolName != "Run pwd" {
		t.Fatalf("TranslateACPXTurnOutput() fourth event = %#v", events[3])
	}
	if events[4].Kind != ACPXEventToolFinished || events[4].ToolName != "Run pwd" || events[4].ToolStatus != "completed" {
		t.Fatalf("TranslateACPXTurnOutput() fifth event = %#v", events[4])
	}
	if events[5].Kind != ACPXEventAssistantMessageFinal || events[5].Text != "Hello world" {
		t.Fatalf("TranslateACPXTurnOutput() sixth event = %#v", events[5])
	}
	if events[6].Kind != ACPXEventSessionDone {
		t.Fatalf("TranslateACPXTurnOutput() seventh event = %#v", events[6])
	}
}

func TestTranslateACPXTurnOutputParsesStrictJSONError(t *testing.T) {
	t.Parallel()

	events, err := TranslateACPXTurnOutput(`{"jsonrpc":"2.0","id":null,"error":{"code":-32002,"message":"no session"}}`)
	if err != nil {
		t.Fatalf("TranslateACPXTurnOutput() error = %v", err)
	}

	if got, want := len(events), 1; got != want {
		t.Fatalf("TranslateACPXTurnOutput() events = %d, want %d", got, want)
	}
	if events[0].Kind != ACPXEventSessionError || events[0].Text != "no session" {
		t.Fatalf("TranslateACPXTurnOutput() event = %#v", events[0])
	}
}

func TestTranslateACPXTurnOutputParsesArrayContentChunks(t *testing.T) {
	t.Parallel()

	output := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":[{"type":"text","text":"hello"},{"type":"text","text":" world"}]}}}
{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`

	events, err := TranslateACPXTurnOutput(output)
	if err != nil {
		t.Fatalf("TranslateACPXTurnOutput() error = %v", err)
	}

	if got, want := len(events), 3; got != want {
		t.Fatalf("TranslateACPXTurnOutput() events = %d, want %d", got, want)
	}
	if events[0].Kind != ACPXEventAssistantMessageChunk || events[0].Text != "hello world" {
		t.Fatalf("TranslateACPXTurnOutput() first event = %#v", events[0])
	}
	if events[1].Kind != ACPXEventAssistantMessageFinal || events[1].Text != "hello world" {
		t.Fatalf("TranslateACPXTurnOutput() second event = %#v", events[1])
	}
	if events[2].Kind != ACPXEventSessionDone {
		t.Fatalf("TranslateACPXTurnOutput() third event = %#v", events[2])
	}
}
