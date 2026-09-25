# Swarm runner executable contract

Run `spexus-swarm-runner --config /run/config/runner.json`. Operator diagnostics: `spexus-swarm-runner --config /run/config/runner.json --history` opens the journal read-only and prints safe launch/input/outbox metadata (no prompts, tokens or Pi history).

Config JSON fields:

```json
{
  "coordinator_url": "https://coordinator:8443",
  "ca_file": "/run/secrets/ca.crt",
  "credential_file": "/run/secrets/runtime-token",
  "tenant_id": "UUID",
  "project_id": "UUID",
  "agent_id": "orchestrator",
  "instance_id": "UUID",
  "role": "owner",
  "profile_id": "orchestrator",
  "available_models": ["openai-codex/gpt-6-luna"],
  "state_directory": "/var/lib/swarm",
  "workspace": "/workspace",
  "pi_binary": "pi",
  "targets": [
    {"agent_id": "worker-a", "profile_id": "worker-a"},
    {"agent_id": "worker-b", "profile_id": "worker-b"}
  ]
}
```

Worker config uses role `worker`, its bound `profile_id`, and `targets: []`. No editable profile file is mounted. The coordinator reads the scoped active snapshot from the backend before dispatch, and the runner reads it again before each start. Exact canonical UTF-8 bytes and SHA-256 are checked independently; the dispatch includes generation and digest. An atomic backend launch claim must match both before Pi starts. The runner pins the claim, bytes, model, and reasoning in SQLite. `available_models` is the deployment's actual Pi model allowlist; a backend-selected model outside it is denied. Tools and extensions stay empty. Pi credentials are runtime mounted into its normal auth location (`PI_CODING_AGENT_DIR`); they are never config/profile/prompt fields.

State directory is private, writable and persistent. It contains `runner.db`, an exclusive process lock, and `sessions/`. Workspace is private and may be empty; Pi tools and autoloaders are disabled. The configured instance_id is fixed for one process generation: restart requires the documented offline binding reconciliation, with proof that the previous process stopped. Journal startup never replays an input whose starting marker exists. Pending durable outbox can resume delivery; it never grants permission for a new model launch.

### Isolated local fixture: pause after launch claim

For the R4 crash experiment only, start one runner with `spexus-swarm-runner --config /config/worker-a.json --local-fixture --pause-after-claim` while the coordinator uses its own `serve --local-fixture` mode. Both runner flags are local command-line switches; `pause_after_claim` is rejected in runner JSON and no message or HTTP request can enable the pause. Normal runner invocation has no barrier. The state directory must be owned by the runner and not writable by other users. The runner creates `state_directory/claim-barrier/` with mode `0700`.

After the typed backend claim succeeds and its exact snapshot is pinned in SQLite, the runner writes `claim-<claim_id>.ready` with mode `0600` in that directory. The marker contains the claim ID, profile ID, revision, execution reference and mailbox sequence, without credentials or snapshot bytes. At this point `profile_launches.state` is `claimed`, `inbox.launches` is `0`, and Pi has not started. To release the fixture, an operator running as the runner's local user creates `claim-<claim_id>.release` as a regular mode `0600` file in the same directory; for example, from inside the fixture container, run `umask 077; : > /state/claim-barrier/claim-<claim_id>.release` when `/state` is the configured state directory. The runner refuses symlinks or files owned by another user.

To prove the crash boundary, capture the ready marker and backend claim readback, then kill and reap the isolated runner without creating the release file. Perform the normal offline instance-binding reconciliation before restarting with a new instance ID. Restart without the pause flags and without changing the pinned SQLite or backend rows. The starting claim remains unknown: no automatic Pi relaunch and no `launched` observation. This fixture switch must not be used for a production runner.

After Pi `StartPrompt` confirms a physical launch, the runner persists that fact before reporting `launched` to the backend. On observation transport failure or restart, only that durable physical-launch record is retried; a claim without launch evidence remains unknown and never causes another Pi start. A changed/disabled profile prevents a new start while an already running attempt keeps its pinned revision. A queued worker attempt invalidated before launch finishes with `profile_changed_before_launch` or `profile_disabled_before_launch`; a transient backend read failure finishes with `profile_backend_unavailable`. No claim or Pi launch is recorded in these cases. A new attempt performs a fresh read.

Client validates HTTPS CA/hostname with TLS1.2+, supplies bearer/instance headers, and uses only `/internal/agent/v1`. Coordinator must be ready before runner starts; initial heartbeat binds identity. Normal mailbox and control mailbox are polled separately, with heartbeat every10s. Retry retains request IDs. Definitive HTTP rejections are recorded; transient network/429/503 failures retain the durable outbox.

Worker input contains only its dispatch context. Owner input includes the triggering event, available worker profile metadata, and authoritative job state for results. Required model output is the whole final assistant JSON, after Pi `agent_settled`: worker `{outcome,summary,evidence,error}`; owner `{actions:[{kind,data}],reply}`. No prose or fences. One strict-output correction may run within the same pinned execution and claim; it never reads a new profile or retries an unknown launch after restart. Evidence uses `{kind:"text"|"ref",label,content_or_ref,sha256?}`. Owner dispatch.data contains `worker_agent_id`, optional existing `job_id`, and DispatchPayload fields; review.data contains job_id/attempt_id plus ReviewPayload; cancel.data contains job_id/attempt_id plus CancelPayload. The runtime supplies stable message/attempt/turn IDs and defaults absent dispatch deadlines to60s and run timeout600s.

Journal metadata enables checking model launch counts separately from delivery counts. SIGTERM cancels/reaps Pi before release of the process lock; unknown starts remain interrupted for explicit operator reconciliation. This is a P2 safe-stop guard, not full P3 automatic crash recovery.

## Provider phases and final action boundary

Pi 0.84.2 may place several text blocks in one assistant message. For the last assistant `message_end` before `agent_settled`, the adapter selects text whose `textSignature` encodes `TextSignatureV1` phase `final_answer` when such blocks exist. It excludes `commentary` and unknown explicit phases. Providers with unphased or legacy opaque-ID text retain their plain final-message behavior. Malformed structured phase metadata is not promoted to unphased output. No draft text, code fence stripping, trailing-text repair, or commentary intent extraction occurs.

The owner must put every requested dispatch/review/cancel in its FINAL JSON `actions` array. An empty final array executes no action even if commentary described delegation. The final reply describes dispatch as requested until coordinator receipts establish acceptance; it cannot infer an executed assignment from model commentary. This boundary was checked against the installed Pi TextContent/TextSignatureV1 types and an actual P2 model session.

## Review evidence shape

Owner review actions require job_id, attempt_id, result_message_id (actual UUID strings), verdict (`accepted` or `revise`), a nonempty reason, and evidence. Evidence is an array of objects `{kind:"text"|"ref",label:string,content_or_ref:string,sha256?:string}`; strings such as `["sum:80"]` are rejected without coercion. The owner prompt includes every review field and element type, including a complete example, optional digest rules and bounds. Dispatch context refs likewise specify `{kind,ref,label}` objects rather than leaving the nonempty-array shape implicit. These clarifications do not change the wire schema or strict decoder.
