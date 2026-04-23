# Spexus Agent How-To

This document explains how to build, configure, and run Spexus Agent locally.

## 1. Build

Prerequisites:

- Go 1.22 or newer
- `acpx` installed on `PATH`

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
- ACPX dispatch lifecycle
- rendered thread replies

To enable raw Slack websocket frame logging as well:

```bash
SPEXUS_AGENT_DEBUG_RAW_SOCKET=1 ./bin/spexus-agent runtime start --debug
```

If you wrap the runtime with a user-level systemd service, remember that it will not automatically inherit your interactive shell environment. Rebuild the service environment after exporting variables such as `MCP_BEARER_AUTH`, `CODEX_HOME`, `OPENAI_API_KEY`, or `ANTHROPIC_API_KEY`, otherwise MCP and model access may work in a terminal session but fail in Slack-triggered turns.

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

Cancel an active Slack thread session:

```bash
./bin/spexus-agent runtime cancel <thread_ts>
```

## 7. ACPX Expectations

Spexus Agent expects:

- `acpx` to be installed and available on `PATH`
- ACPX prompt execution to work in the project working directory
- strict JSON output support through:

```bash
acpx --format json --json-strict ...
```

If ACPX fails, Spexus Agent will post a thread-level session error into Slack and print the ACPX output in `--debug` mode.

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

## 9. Publish to GitHub

Example target repository:

```text
git@github.com:spexus-ai/spexus-agent.git
```

If you initialize Git locally, a typical sequence is:

```bash
git init
git add .
git commit -m "Initial commit"
git branch -M main
git remote add origin git@github.com:spexus-ai/spexus-agent.git
git push -u origin main
```
