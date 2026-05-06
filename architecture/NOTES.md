# Notes

## Strong Signals

- The current repository is still a local single-binary system, not a hosted control plane.
- `internal/cli/runtime_handler.go` is the operational hotspot and the dominant C3 orchestrator.
- `runtime start` requires an existing `~/.config/spexus-agent/storage.sqlite3` file and performs startup recovery for queued/running executions before entering the Slack loop.
- Slack integration is split cleanly into websocket/socket-mode transport and Web API posting/provisioning.
- Project import depends on live `git` subprocess calls for repository validation and remote-name derivation.
- Runtime durability is strongly grounded in SQLite tables for projects, threads, dedupe, locks, and executions.

## Weak Signals / Gaps

- Spexus exists as product context and target vision, but not as a confirmed direct integration surface in current code.
- `codesearch` currently requires repo-local Hugging Face cache environment variables for reproducible indexing/search in this sandboxed environment.

## Follow-Ups

- Bake the repo-local `codesearch` cache environment into the reusable run command or helper script.
- Consider splitting `runtime_handler.go` if the runtime control plane grows further.
