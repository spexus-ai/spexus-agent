# Swarm P2 API implementation boundary

Normative contract: SP-STD-021. Module import: `github.com/spexus-ai/spexus-agent/internal/swarm`.
DTOs are published in `types.go`; all method signatures below are the implementation commitment. This note does not claim service implementation complete.

## Construction and local trusted integration

```go
func Open(ctx context.Context, path string, cfg Config) (*Store, error)
func (s *Store) Close() error
func (s *Store) Handler() http.Handler
func (s *Store) RegisterFeature(ctx context.Context, feature Feature) error
func (s *Store) Ingest(ctx context.Context, featureID string, input InputPayload) (Receipt, bool, error)
func (s *Store) StopFeature(ctx context.Context, featureID, actorID, reason string) error
func (s *Store) ContinueFeature(ctx context.Context, featureID, actorID string) error
func (s *Store) History(ctx context.Context, featureID string) (History, error)
func ReadHistory(ctx context.Context, path, featureID string) (History, error)
func (s *Store) QueueSlackNotice(ctx context.Context, featureID, sourceEventID, text string) error
func (s *Store) ClaimSlack(ctx context.Context) (*SlackDelivery, error)
func (s *Store) SettleSlack(ctx context.Context, id, status, slackTS string) error
func (s *Store) Sweep(ctx context.Context) error
func ReconcileOffline(ctx context.Context, path string, cfg Config, request ReconcileRequest) error
```

`Open` takes the exclusive coordinator filesystem lock and validates tenant/project, three bound agent IDs, the backend profile service connection, and static feature anchors. Close releases the lock. Executable profile bytes live in the scoped backend revision store, never in coordinator JSON. `RegisterFeature` is idempotent with immutable anchor/owner/scope; no public registration API exists. `Ingest` is trusted-only and validates fixture channel/thread/actor, deduplicates source event, and returns receipt and duplicate bool. Stop/Continue validate actor against the feature allowlist. Continue refuses unresolved nonterminal work; no replay. `History` is local operator-only safe export.

`ClaimSlack` atomically queued→sending, returns nil when none. Existing sending rows recovered at coordinator startup as delivery_unknown. Caller must reconcile unknown rows from History before retry; SettleSlack accepts sent (requires slackTS), delivery_unknown, failed, or queued (explicitly reconciled absent) and records audit. Message correlation = SlackDelivery.ID; use client_msg_id/metadata when posting. Never blindly retry sending/unknown. `Sweep` runs at least once per second for acceptance/start/run deadlines; heartbeat age diagnostic only. Offline reconcile opens same exclusive lock; fails while coordinator open, requires stopped container identity/check time evidence and exact old binding; never runs a model.

## HTTPS boundary

Serve Handler using TLS1.2+ listener configured by command; Handler rejects non-TLS except GET /healthz and /readyz. Bearer token SHA matches AgentConfig; UUID X-Agent-Instance-ID on every protected request. First heartbeat establishes binding; all other requests require it. Paths under APIPrefix `/internal/agent/v1`:

* POST /agents/self/heartbeat → HeartbeatRequest/Response
* POST /messages → Envelope/Receipt (201 new,200 duplicate)
* GET /mailbox?lane=normal|control&limit=20&wait_seconds=20 → MailboxResponse. Delivery embeds envelope at top level plus mailbox_seq/received_at.
* POST /acks → AckRequest/Response
* GET /jobs/{job_id}?limit=20&cursor=... → JobView
* POST /owner-turns/start → OwnerStartRequest/Receipt
* GET /owner-turns/{turn_id} → OwnerTurn
* POST /owner-turns/{turn_id}/finish → OwnerFinishRequest/Receipt

Errors: `{ "error": { "code": "...", "message": "...", "retryable": false }, "request_id": "UUID" }`, no internal stack or secret. Size128KiB, strict fields. APIError has Status and Code. UUIDs use NewID(). Payloads use json.Marshal exact exported types. CausationID nil serializes null. Internal dispatch `Profile` includes immutable executable ID, generation, exact snapshot digest, model, and reasoning. Coordinator checks it against a fresh backend active read; runner checks canonical bytes and model availability, then obtains an atomic typed launch claim before Pi start. `tools` and `extensions` are empty in the Web profile v1 path.

Owner final action bridge adds owner_turn_id on dispatch/review/cancel; worker lifecycle omits it. Initial dispatch may use null causation; worker accepted points dispatch; started points accepted; result points started (or cancel for interrupted/cancelled). Review points result. All agent events require canonical RFC3339 UTC sent_at. start/finish duplicate semantics are as STD21; a repeated receipt is historical, so GET job/turn before physical spawn.

## Executable/packaging needs

Coordinator command loads Config, DB path, TLS cert/key; serves Handler on private 8443; runs Sweep every second plus Slack bridge. Runtime instances persist private state. Config includes only hashed runtime credentials, while each runner mounts its own clear token separately. Coordinator has no Pi credential and runners have no Slack credentials. TLS CA/hostname must be verified by client. No publicly bound service port. Separate fresh state volumes; startup refuses changed immutable config/schema. Command must expose operator history and offline reconcile, not an online reset endpoint.

`ReadHistory` uses a read-only SQLite connection and coherent read transaction; it is safe while coordinator holds its exclusive writer process lock. `QueueSlackNotice` is an idempotent trusted local ingress status response; empty TurnID means notice, not model result. Both notice and model response share delivery_unknown semantics.
