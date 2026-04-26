package runtime

import (
	"fmt"
	"strings"
)

type SlackCommandSurface string

const (
	SlackCommandSurfaceMention SlackCommandSurface = "mention"
	SlackCommandSurfaceSlash   SlackCommandSurface = "slash"
)

type SlackCommandKind string

const (
	SlackCommandInvalid SlackCommandKind = "invalid"
	SlackCommandHelp    SlackCommandKind = "help"
	SlackCommandStatus  SlackCommandKind = "status"
	SlackCommandAsk     SlackCommandKind = "ask"
	SlackCommandCancel  SlackCommandKind = "cancel"
)

type SlackCommand struct {
	Kind   SlackCommandKind
	Prompt string
}

func ParseSlackCommand(text string) SlackCommand {
	text = strings.TrimSpace(text)
	if text == "" {
		return SlackCommand{Kind: SlackCommandInvalid}
	}

	fields := strings.Fields(text)
	if len(fields) == 0 {
		return SlackCommand{Kind: SlackCommandInvalid}
	}

	keyword := strings.ToLower(fields[0])
	remainder := strings.TrimSpace(text[len(fields[0]):])

	switch keyword {
	case string(SlackCommandHelp):
		if remainder != "" {
			return SlackCommand{Kind: SlackCommandInvalid}
		}
		return SlackCommand{Kind: SlackCommandHelp}
	case string(SlackCommandStatus):
		if remainder != "" {
			return SlackCommand{Kind: SlackCommandInvalid}
		}
		return SlackCommand{Kind: SlackCommandStatus}
	case string(SlackCommandAsk):
		if remainder == "" {
			return SlackCommand{Kind: SlackCommandInvalid}
		}
		return SlackCommand{
			Kind:   SlackCommandAsk,
			Prompt: remainder,
		}
	case string(SlackCommandCancel):
		if remainder != "" {
			return SlackCommand{Kind: SlackCommandInvalid}
		}
		return SlackCommand{Kind: SlackCommandCancel}
	default:
		return SlackCommand{Kind: SlackCommandInvalid}
	}
}

func (c SlackCommand) ShouldExecute() bool {
	switch c.Kind {
	case SlackCommandStatus, SlackCommandAsk, SlackCommandCancel:
		return true
	default:
		return false
	}
}

func (c SlackCommand) ACPXPrompt() string {
	switch c.Kind {
	case SlackCommandStatus:
		return "status"
	case SlackCommandAsk:
		return strings.TrimSpace(c.Prompt)
	case SlackCommandCancel:
		return "cancel"
	default:
		return ""
	}
}

func SlackCommandHelpText(surface SlackCommandSurface) string {
	switch surface {
	case SlackCommandSurfaceSlash:
		return "Supported commands: `help`, `status`, `ask <prompt>`, and `cancel`. Usage: `/spexus help`, `/spexus status`, `/spexus ask <prompt>`, or `/spexus cancel`."
	default:
		return fmt.Sprintf(
			"Supported commands: `help`, `status`, `ask <prompt>`, and `cancel`. Usage: mention the agent with one of those commands, for example `%s`.",
			"<@agent> ask summarize current project state",
		)
	}
}
