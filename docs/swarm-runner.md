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
  "agent_id": "UUID",
  "instance_id": "UUID",
  "role": "owner",
  "profile_file": "/run/profiles/owner.json",
  "state_directory": "/var/lib/swarm",
  "workspace": "/workspace",
  "pi_binary": "pi",
  "targets": [
    {"agent_id": "WORKER_UUID", "profile_file": "/run/profiles/worker-a.json"}
  ]
}
```

Worker config uses role `worker` and `targets: []`. Each profile is the exact immutable UTF-8 JSON `swarm.TextProfile` snapshot used by coordinator: `id`, `model` (`provider/model`), `reasoning`, `prompt`, `tools: []`, `extensions: []`. Files are hashed as read, without reserialization. No extra tool or extension is accepted. Targets provide only allowed dispatch profile metadata to the owner; the actual owner model chooses dispatch/review/cancel. Runtime fills IDs and authority. Pi credentials are runtime mounted into its normal auth location (`PI_CODING_AGENT_DIR`); they are never config/profile/prompt fields.

State directory is private, writable and persistent. It contains `runner.db`, an exclusive process lock, and `sessions/`. Workspace is private and may be empty; Pi tools and autoloaders are disabled. The configured instance_id is fixed for one process generation: restart requires the documented offline binding reconciliation, with proof that the previous process stopped. Journal startup never replays an input whose starting marker exists. Pending durable outbox can resume delivery; it never grants permission for a new model launch.

Client validates HTTPS CA/hostname with TLS1.2+, supplies bearer/instance headers, and uses only `/internal/agent/v1`. Coordinator must be ready before runner starts; initial heartbeat binds identity. Normal mailbox and control mailbox are polled separately, with heartbeat every10s. Retry retains request IDs. Definitive HTTP rejections are recorded; transient network/429/503 failures retain the durable outbox.

Worker input contains only its dispatch context. Owner input includes the triggering event, available worker profile metadata, and authoritative job state for results. Required model output is the whole final assistant JSON, after Pi `agent_settled`: worker `{outcome,summary,evidence,error}`; owner `{actions:[{kind,data}],reply}`. No prose/fences or hidden model retry. Evidence uses `{kind:"text"|"ref",label,content_or_ref,sha256?}`. Owner dispatch.data contains `worker_agent_id`, optional existing `job_id`, and DispatchPayload fields; review.data contains job_id/attempt_id plus ReviewPayload; cancel.data contains job_id/attempt_id plus CancelPayload. The runtime supplies stable message/attempt/turn IDs and defaults absent dispatch deadlines to60s and run timeout600s.

Journal metadata enables checking model launch counts separately from delivery counts. SIGTERM cancels/reaps Pi before release of the process lock; unknown starts remain interrupted for explicit operator reconciliation. This is a P2 safe-stop guard, not full P3 automatic crash recovery.

## Provider phases and final action boundary

Pi 0.84.2 may place several text blocks in one assistant message. For the last assistant `message_end` before `agent_settled`, the adapter selects text whose `textSignature` encodes `TextSignatureV1` phase `final_answer` when such blocks exist. It excludes `commentary` and unknown explicit phases. Providers with unphased or legacy opaque-ID text retain their plain final-message behavior. Malformed structured phase metadata is not promoted to unphased output. No draft text, code fence stripping, trailing-text repair, or commentary intent extraction occurs.

The owner must put every requested dispatch/review/cancel in its FINAL JSON `actions` array. An empty final array executes no action even if commentary described delegation. The final reply describes dispatch as requested until coordinator receipts establish acceptance; it cannot infer an executed assignment from model commentary. This boundary was checked against the installed Pi TextContent/TextSignatureV1 types and an actual P2 model session.
