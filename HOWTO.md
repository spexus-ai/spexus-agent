# Spexus Agent How-To

This document explains how to build, configure, and run Spexus Agent locally.

## 1. Build

Prerequisites:

- Go 1.22 or newer
- Pi 0.84.2 or newer with JSONL RPC and `agent_settled` support on `PATH`

The verified version can be installed with `npm install -g @earendil-works/pi-coding-agent@0.84.2`. Configure provider authentication in Pi before starting the agent.

Build the binary:

```bash
make cli
```

The resulting binary is:

```text
./bin/spexus-agent
```

Run tests:

```bash
make tests
```

## 2. Local Configuration

Initialize the default config file:

```bash
./bin/spexus-agent config init
```

Set the base workspace path where imported repositories will live:

```bash
./bin/spexus-agent config set-base-workspace /absolute/path/to/workspace
```

Inspect the current config:

```bash
./bin/spexus-agent config show
./bin/spexus-agent config validate
```

By default, Spexus Agent stores configuration under:

```text
~/.config/spexus-agent/config.json
```

and runtime state under:

```text
~/.config/spexus-agent/storage.sqlite3
```

## 2.1 Agent profile

A minimal profile and prompt are available in `examples/pi-prototype/`. The
example disables tools for conversation smoke checks; enable the tools you need.

Edit `config.json` to include the `agent` object. Paths inside the profile are
relative to the directory containing `config.json`, unless absolute.

```json
{
  "baseWorkspacePath": "/absolute/path/to/workspaces",
  "agent": {
    "id": "prototype",
    "provider": "openai-codex",
    "model": "gpt-5.6-sol",
    "thinking": "medium",
    "promptFile": "prototype.md",
    "workspace": "/absolute/path/to/workspaces/project",
    "tools": ["read", "bash", "edit", "write", "grep", "find", "ls"]
  },
  "slack": {
    "botToken": "<configured by slack-auth login>",
    "appToken": "<configured by slack-auth login>",
    "workspaceId": "<your Slack workspace ID>"
  }
}
```

Choose a provider/model available in your Pi configuration. Create `prototype.md`
with the agent's system instructions, or use `systemPrompt` directly in JSON.
Set exactly one of those prompt fields. The workspace must exist and match the
registered Slack project's local path; a mismatch is reported in the thread.

Optional fields: `sessionDirectory` for durable Pi sessions and `extensions` for
explicit extension paths. Automatic extension, skill and prompt-template discovery
is disabled for reproducibility. Project context files remain available to Pi.
An omitted/null `tools` uses Pi's defaults; `[]` disables tools.

Set `SPEXUS_AGENT_HOME=/absolute/path/to/prototype-state` for an isolated runtime
configuration and database. Set `SPEXUS_AGENT_PI_BIN` if Pi is not on `PATH`.
`PI_CODING_AGENT_DIR` selects Pi's auth/settings directory independently.
Profile changes take effect after a runtime restart. Pi session files preserve
completed conversation history; use a new Slack thread for a fresh conversation.

## 3. Slack Setup

Spexus Agent requires a Slack app with both bot and app-level credentials.

### 3.1 Create a Slack App

Create the app from:

```text
https://api.slack.com/apps
```

### 3.2 Enable Socket Mode

In your Slack app settings:

- enable `Socket Mode`
- create an app-level token
- grant the app-level token the `connections:write` permission

The app-level token should start with:

```text
xapp-
```

### 3.3 Add Bot Token Scopes

Under `OAuth & Permissions`, add at least these bot scopes:

- `channels:manage`
- `channels:history`
- `app_mentions:read`
- `chat:write`

The bot token should start with:

```text
xoxb-
```

### 3.4 Add Event Subscriptions

Under `Event Subscriptions`:

- enable events
- add `message.channels`
- add `app_mention`

After updating scopes or events, reinstall the app to the workspace.

### 3.5 Add the Bot to the Project Channel

For any project channel you expect the runtime to process, ensure the bot is present in the channel.

### 3.6 Slack Thread Usage

In an existing Slack thread, any normal human reply without an agent mention is treated as the next Pi prompt for that thread.

Mention the agent when you want a local runtime command instead:

```text
<@Spexus.Agent> status
<@Spexus.Agent> ask summarize current project state
<@Spexus.Agent> stop
```

Plain root channel messages are ignored. Start or address a thread with an agent mention or `/spexus`, then continue the thread with normal replies.

Use `!stop` in the active thread to interrupt the current turn, `!status` for its status and `!help` for commands. A normal reply or `@agent ask ...` continues the same conversation, including after a stop.

Agent replies stream into the thread. The runtime updates the latest partial answer until that Slack message reaches the configured text limit, then creates the next thread reply and continues streaming there.

## 4. Store Slack Credentials

Run:

```bash
./bin/spexus-agent config slack-auth login
```

Provide:

- the bot token
- the app token
- the Slack workspace ID

Verify the saved credentials:

```bash
./bin/spexus-agent config slack-auth status
```

## 5. Import Projects

Import a local repository:

```bash
./bin/spexus-agent project import-local /absolute/path/to/repo
```

Import a remote repository:

```bash
./bin/spexus-agent project import-remote git@github.com:org/repo.git
```

List registered projects:

```bash
./bin/spexus-agent project list
```

Show a single project:

```bash
./bin/spexus-agent project show <project-name>
```

Delete a project:

```bash
./bin/spexus-agent project delete <project-name>
```

## 6. Run the Foreground Runtime

Start the runtime:

```bash
./bin/spexus-agent runtime start
```

Start the runtime with operational debug output:

```bash
./bin/spexus-agent runtime start --debug
```

This debug mode prints:

- startup validation
- project and storage loading
- Slack event reception
- Pi dispatch lifecycle
- rendered thread replies

To enable raw Slack websocket frame logging as well:

```bash
SPEXUS_AGENT_DEBUG_RAW_SOCKET=1 ./bin/spexus-agent runtime start --debug
```

If you use the user-level systemd service, rebuild its environment after changing `SPEXUS_AGENT_HOME`, `SPEXUS_AGENT_PI_BIN`, `PI_CODING_AGENT_DIR` or model API environment variables. The service needs access to the same Pi authentication store as your terminal.

Check current runtime status:

```bash
./bin/spexus-agent runtime status
```

Reload runtime state:

```bash
./bin/spexus-agent runtime reload
```

Run diagnostics:

```bash
./bin/spexus-agent runtime doctor
```

Stop a turn from the original Slack thread with `!stop` or an agent mention
containing `stop`. The separate `runtime cancel` CLI does not control an active Pi
process in another runtime; use the Slack command. Stopping does not undo tool
side effects or erase conversation history.

## 7. Pi protocol and verification

The runtime starts `pi --mode rpc` in the profile workspace, selects the configured
provider/model/system prompt and sends the prompt over stdin. It reads streaming
RPC events until `agent_settled`, then closes the child. A successful prompt
acknowledgement or `agent_end` alone is not treated as completion. The next turn
opens the same explicit session file; other threads have separate files.

Model and execution errors are posted in the original thread. Raw subprocess
stderr is not forwarded to Slack. Interactive extension dialogs are cancelled with
an explicit error; the generic durable human-request mechanism is prototype 3.

```bash
make verify-pi
```

This runs the real Pi process against a local deterministic HTTP model. It tests
profile application, session isolation, two simultaneous threads, deduplication,
`!stop`, continuation and model errors without paid model calls or live Slack
messages. `make tests` works without an installed Pi and skips opt-in Pi tests.

## 8. Recommended Publication Hygiene

Before pushing this repository to GitHub:

- remove local runtime artifacts you do not want to publish
- confirm Slack tokens are not present in tracked files
- confirm `task-reports/` and `bin/` are not committed unless intentionally needed
- run:

```bash
make tests
make cli
```

## 9. Delivery

Prepare a feature branch and PR after verification. The user performs the merge.
