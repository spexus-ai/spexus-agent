# Spexus Agent

Spexus Agent is a Go-based command-line and foreground runtime bridge between Slack, Pi, and Spexus-managed project context.

It is designed for teams that want to:

- register local or remote repositories as Spexus-managed projects
- provision and reuse Slack channels per project
- run a foreground Slack Socket Mode runtime that turns Slack messages into Pi prompts
- persist project and runtime state in a local SQLite database
- operate the workflow from a local CLI without a separate web service

## What It Does

The project provides three main command groups:

- `config`: initialize and validate local configuration, including Slack credentials
- `project`: import repositories, provision Slack channels, and inspect the local project registry
- `runtime`: run the Slack-facing foreground event loop, reload state, inspect health, and close active thread sessions

At runtime, Spexus Agent:

1. receives Slack events through Socket Mode
2. resolves the Slack channel to a registered project
3. maps the Slack thread to a Pi session name
4. treats agent mentions as command invocations and plain thread replies as prompts
5. invokes `pi` through JSONL RPC
6. translates Pi events into Slack thread replies
7. stores thread and deduplication state in SQLite

## Repository Scope

This repository is focused on the local agent and runtime workflow. It does not include:

- a hosted control plane
- packaged installers

The current runtime is intended to be run in the foreground from a terminal. It also includes local user-level systemd helper targets in the `Makefile` for operators who want to install and restart the runtime as a per-user service.

## Requirements

- Go 1.22 or newer
- Pi 0.84.2 or newer with RPC `agent_settled` support, installed on `PATH`
- Slack app credentials for Socket Mode
- a Linux or Unix-like environment with a writable home directory

## Quick Start

Build the CLI:

```bash
make cli
```

Initialize configuration:

```bash
./bin/spexus-agent config init
./bin/spexus-agent config set-base-workspace /absolute/path/to/workspace
./bin/spexus-agent config slack-auth login
```

Add the `agent` profile to `config.json` (see [HOWTO.md](./HOWTO.md)), including its model, system prompt and absolute workspace. Provider authentication is managed by Pi.

Import that workspace as a project:

```bash
./bin/spexus-agent project import-local /absolute/path/to/repo
```

Run the foreground runtime:

```bash
./bin/spexus-agent runtime start --debug
```

For full setup and Slack configuration details, see [HOWTO.md](./HOWTO.md).

## Slack and Pi Integration

The runtime uses:

- Slack Web API for channel provisioning and thread replies
- Slack Socket Mode for inbound message delivery
- Pi through JSONL RPC for prompt execution and structured event output

In Slack threads, a normal human reply without an agent mention is sent to Pi as the prompt text. Mention the agent when you want the local command surface, such as `status`, `ask <prompt>`, or `stop`. Use `!stop` inside a thread to interrupt the current turn, then send another reply to continue its saved history.

Assistant output is streamed into the Slack thread: the runtime posts the first partial answer, updates that message while it remains under the Slack text limit, and opens the next thread message when the current chunk reaches the limit.

Raw Socket Mode frame logging is disabled by default, even when `--debug` is enabled. To enable websocket-level tracing for troubleshooting:

```bash
SPEXUS_AGENT_DEBUG_RAW_SOCKET=1 ./bin/spexus-agent runtime start --debug
```

## Project Layout

- `cmd/spexus-agent`: CLI entrypoint
- `internal/cli`: command routing and handlers
- `internal/config`: config storage and validation
- `internal/slack`: Slack HTTP and Socket Mode clients
- `internal/piadapter`: Pi RPC subprocess adapter
- `internal/runtime`: runtime coordination, event translation, and Slack rendering
- `internal/storage`: SQLite-backed persistence

## License

This project is licensed under the Apache License 2.0. See [LICENSE](./LICENSE).

## Prototype verification

```bash
make tests       # deterministic unit/runtime tests
make verify-pi   # real Pi + local model endpoint, no provider charges or Slack posts
```

The Pi integration tests cover thread history, independent conversations, duplicate
Slack deliveries, concurrent threads, cancellation, continuation and visible model
errors. A live Slack smoke check additionally needs the configured Slack app and a
test channel.

Set `SPEXUS_AGENT_HOME=/absolute/path` to keep a prototype's config and SQLite
separate from another runtime. One foreground process owns that runtime directory.
Sessions live in its `pi-sessions/<agent-id>` directory by default. Runtime crash
recovery marks interrupted work failed; automatic restart of unfinished work is a
later prototype.
