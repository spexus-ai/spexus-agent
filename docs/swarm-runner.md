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

After Pi `StartPrompt` confirms a physical launch, the runner persists that fact before reporting `launched` to the backend. On observation transport failure or restart, only that durable physical-launch record is retried; a claim without launch evidence remains unknown and never causes another Pi start. A changed/disabled profile prevents a new start while an already running attempt keeps its pinned revision.

Client validates HTTPS CA/hostname with TLS1.2+, supplies bearer/instance headers, and uses only `/internal/agent/v1`. Coordinator must be ready before runner starts; initial heartbeat binds identity. Normal mailbox and control mailbox are polled separately, with heartbeat every10s. Retry retains request IDs. Definitive HTTP rejections are recorded; transient network/429/503 failures retain the durable outbox.

Worker input contains only its dispatch context. Owner input includes the triggering event, available worker profile metadata, and authoritative job state for results. Required model output is the whole final assistant JSON, after Pi `agent_settled`: worker `{outcome,summary,evidence,error}`; owner `{actions:[{kind,data}],reply}`. No prose/fences or hidden model retry. Evidence uses `{kind:"text"|"ref",label,content_or_ref,sha256?}`. Owner dispatch.data contains `worker_agent_id`, optional existing `job_id`, and DispatchPayload fields; review.data contains job_id/attempt_id plus ReviewPayload; cancel.data contains job_id/attempt_id plus CancelPayload. The runtime supplies stable message/attempt/turn IDs and defaults absent dispatch deadlines to60s and run timeout600s.

Journal metadata enables checking model launch counts separately from delivery counts. SIGTERM cancels/reaps Pi before release of the process lock; unknown starts remain interrupted for explicit operator reconciliation. This is a P2 safe-stop guard, not full P3 automatic crash recovery.

## Provider phases and final action boundary

Pi 0.84.2 may place several text blocks in one assistant message. For the last assistant `message_end` before `agent_settled`, the adapter selects text whose `textSignature` encodes `TextSignatureV1` phase `final_answer` when such blocks exist. It excludes `commentary` and unknown explicit phases. Providers with unphased or legacy opaque-ID text retain their plain final-message behavior. Malformed structured phase metadata is not promoted to unphased output. No draft text, code fence stripping, trailing-text repair, or commentary intent extraction occurs.

The owner must put every requested dispatch/review/cancel in its FINAL JSON `actions` array. An empty final array executes no action even if commentary described delegation. The final reply describes dispatch as requested until coordinator receipts establish acceptance; it cannot infer an executed assignment from model commentary. This boundary was checked against the installed Pi TextContent/TextSignatureV1 types and an actual P2 model session.

## Review evidence shape

Owner review actions require job_id, attempt_id, result_message_id (actual UUID strings), verdict (`accepted` or `revise`), a nonempty reason, and evidence. Evidence is an array of objects `{kind:"text"|"ref",label:string,content_or_ref:string,sha256?:string}`; strings such as `["sum:80"]` are rejected without coercion. The owner prompt includes every review field and element type, including a complete example, optional digest rules and bounds. Dispatch context refs likewise specify `{kind,ref,label}` objects rather than leaving the nonempty-array shape implicit. These clarifications do not change the wire schema or strict decoder.
