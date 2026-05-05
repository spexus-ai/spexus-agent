package runtime

import (
	"strings"
	"testing"
)

// Test: the shared Slack command parser accepts the minimal command set for mention and slash routing.
// Validates: AC-1820 (REQ-1189 - supported commands include help, status, ask, and close)
func TestParseSlackCommandRecognizesSupportedCommands(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		input  string
		kind   SlackCommandKind
		prompt string
	}{
		{name: "help", input: "help", kind: SlackCommandHelp},
		{name: "status", input: "status", kind: SlackCommandStatus},
		{name: "ask", input: "ask summarize current project state", kind: SlackCommandAsk, prompt: "summarize current project state"},
		{name: "close", input: "close", kind: SlackCommandClose},
		{name: "case-insensitive", input: "ASK summarize current project state", kind: SlackCommandAsk, prompt: "summarize current project state"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			command := ParseSlackCommand(tc.input)
			if command.Kind != tc.kind {
				t.Fatalf("ParseSlackCommand(%q) kind = %q, want %q", tc.input, command.Kind, tc.kind)
			}
			if command.Prompt != tc.prompt {
				t.Fatalf("ParseSlackCommand(%q) prompt = %q, want %q", tc.input, command.Prompt, tc.prompt)
			}
		})
	}
}

// Test: the shared Slack command parser routes only valid executable commands into ACPX-ready prompt text.
// Validates: AC-1817 (REQ-1184 - empty mention commands return usage guidance), AC-1820 (REQ-1189 - ask forwards prompt text)
func TestParseSlackCommandBuildsACPXPromptOnlyForExecutableCommands(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		input      string
		shouldRun  bool
		acpxPrompt string
		expectKind SlackCommandKind
	}{
		{name: "empty", input: "", shouldRun: false, expectKind: SlackCommandInvalid},
		{name: "help", input: "help", shouldRun: false, expectKind: SlackCommandHelp},
		{name: "unknown", input: "review", shouldRun: false, expectKind: SlackCommandInvalid},
		{name: "ask missing prompt", input: "ask", shouldRun: false, expectKind: SlackCommandInvalid},
		{name: "status", input: "status", shouldRun: true, acpxPrompt: "status", expectKind: SlackCommandStatus},
		{name: "ask", input: "ask summarize open work", shouldRun: true, acpxPrompt: "summarize open work", expectKind: SlackCommandAsk},
		{name: "close", input: "close", shouldRun: true, acpxPrompt: "cancel", expectKind: SlackCommandClose},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			command := ParseSlackCommand(tc.input)
			if command.Kind != tc.expectKind {
				t.Fatalf("ParseSlackCommand(%q) kind = %q, want %q", tc.input, command.Kind, tc.expectKind)
			}
			if command.ShouldExecute() != tc.shouldRun {
				t.Fatalf("ParseSlackCommand(%q) ShouldExecute() = %t, want %t", tc.input, command.ShouldExecute(), tc.shouldRun)
			}
			if got := command.ACPXPrompt(); got != tc.acpxPrompt {
				t.Fatalf("ParseSlackCommand(%q) ACPXPrompt() = %q, want %q", tc.input, got, tc.acpxPrompt)
			}
		})
	}
}

// Test: mention and slash help text expose the same supported command set with transport-specific usage examples.
// Validates: AC-1817 (REQ-1184 - mention usage guidance is human-readable), AC-1820 (REQ-1189 - slash help includes status, ask, and close)
func TestSlackCommandHelpTextIncludesSupportedCommands(t *testing.T) {
	t.Parallel()

	mention := SlackCommandHelpText(SlackCommandSurfaceMention)
	slash := SlackCommandHelpText(SlackCommandSurfaceSlash)

	for _, text := range []string{mention, slash} {
		for _, fragment := range []string{"help", "status", "ask <prompt>", "close"} {
			if !strings.Contains(text, fragment) {
				t.Fatalf("help text %q missing %q", text, fragment)
			}
		}
	}

	if !strings.Contains(mention, "mention the agent") {
		t.Fatalf("mention help text = %q, want mention-specific usage", mention)
	}
	if !strings.Contains(slash, "/spexus status") {
		t.Fatalf("slash help text = %q, want slash-specific usage", slash)
	}
}
