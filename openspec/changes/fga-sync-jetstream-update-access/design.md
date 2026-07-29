## Context

fga-sync consumes all six NATS subjects — four sync subjects and two access-check
subjects — through core NATS `QueueSubscribe` (`main.go:329-367`, wired in
`createQueueSubscriptions` at `main.go:370-416`). Core NATS has no ack/redelivery:
when `genericUpdateAccessHandler` returns an error (`main.go:346-354`) the message
is already consumed and lost. The NATS connection also uses the default
`MaxReconnects` (60) and calls `os.Exit(1)` when reconnects are exhausted
(`main.go:181-201`).

JetStream is already available in the process — it is used for the access-check
KV cache bucket (`main.go:209-216`) — but not for message consumption. The
selected Path 1 keeps the `lfx.fga-sync.update_access` and
`lfx.fga-sync.delete_access` subjects and `GenericFGAMessage` envelope, changes
both subjects to asynchronous-only publishing, and consumes them through one
durable pull consumer. Project-service LFXV2-2830 and committee-service
LFXV2-2828 remove their latent request/reply paths before the stream starts
capturing either subject.

This design covers Phase 1 only: the delivery foundation plus migrating
`lfx.fga-sync.update_access` and `lfx.fga-sync.delete_access`. Phase 2 reuses
this foundation for `member_put` and `member_remove`.

## Goals / Non-Goals

**Goals:**

- Durable, at-least-once delivery of update and delete access operations with
  redelivery on transient OpenFGA/NATS failure.
- Exactly one delivery path for each migrated subject.
- Globally ordered full-state application and deletion with one pending message.
- A bounded retry window with authoritative exhaustion detection and safe
  recovery from current source state.
- Infinite NATS reconnect; no process exit on reconnect exhaustion.
- A typed terminal-validation marker with fail-safe transient default,
  observable counters, and JetStream advisories.
- Asynchronous-only update and delete access after blocking publisher changes.

**Non-Goals:**

- Migrating the two membership sync subjects (Phase 2).
- Migrating the request/reply access-check subjects.
- In-repo automated NATS+OpenFGA integration harness.
- JetStream publisher APIs, source revisions, distributed per-object locking,
  dedicated DLQ/reconciliation, cache redesign, or publisher authorization
  redesign.
- Prometheus metrics; envelope/tuple format changes.

## Decisions

### D1: Stream scope — capture update and delete access together

The Phase 1 stream captures `lfx.fga-sync.update_access` and
`lfx.fga-sync.delete_access`. The two membership subjects stay entirely on core
NATS until Phase 2.

- **Why delete is included:** if a delayed update retries after a later
  core-delivered deletion, it can recreate authorization tuples for a deleted
  resource. One stream and consumer preserve the update/delete order.
- **Alternative (update only):** rejected because it cannot prevent
  cross-transport authorization recreation.
- **Alternative (wildcard `lfx.fga-sync.>`):** rejected because membership
  messages would accumulate without a consumer until Phase 2.

### D2: Asynchronous-only publisher contract is a deployment prerequisite

Publishers keep the same subjects and payloads but use core NATS `Publish` only
for update and delete access. Project-service LFXV2-2830 and committee-service
LFXV2-2828 must cover both subjects and deploy before stream provisioning. All
other publishers must be verified asynchronous-only. Applicable indexer
request/reply behavior remains unchanged.

The JetStream adapter never exposes its ACK reply subject as an application
reply. `Reply()` returns empty and `Respond()` is not used for either migrated
subject, so handlers cannot send `"OK"` to the JetStream ACK endpoint.

### D3: One shared durable pull consumer with global serialization

Create or bind one pull consumer over the stream containing exactly the update
and delete access subjects. Every replica binds the same durable; no
`DeliverSubject` or `DeliverGroup` is configured.

- Stream: `fga-sync-events`.
- Consumer: `fga-sync-access-mutation-consumer`.
- `DeliverPolicy: DeliverNewPolicy`.
- `AckPolicy: AckExplicitPolicy`.
- `MaxAckPending: 1` across the shared consumer.
- `MaxDeliver: 7`.
- Processing deadline: 90 seconds.
- Consumer `BackOff`: `2m`, `2m`, `5m`, `10m`, `15m`, `30m`.

`BackOff` supplies the acknowledgement timeout and redelivery schedule, so a
separate `AckWait` value is not configured. The first two-minute value exceeds
the enforced 90-second processing deadline.

The global one-message limit is deliberate. `update_access` is full-state
replacement and `delete_access` removes publisher-managed state while retaining
the existing externally managed `team:*` grants. An older update applied after
deletion can restore stale authorization. Later messages remain stored while
the head message retries. With seven deliveries and six BackOff values, the
last `30m` delay repeats before the max-delivery advisory; authoritative
exhaustion and the accepted head-of-line window occur at approximately 94
minutes.

`DeliverNewPolicy` governs only first creation of the durable. The fenced
cutover verifies the stream is empty before creation, so it does not omit any
post-cutover message. On ordinary process or NATS restarts the durable still
exists and resumes its stored cursor, including pending redeliveries. If the
durable state itself is lost while retained stream history remains, automatic
recreation begins at the current stream tail. Retained history is neither
replayed nor purged; it expires under `maxAge`. This avoids both stale
authorization replay and a destructive multi-replica purge race, keeps new
access mutations moving without operator intervention, and accepts that the
ambiguous pre-recreation history may require source-of-truth reconciliation.

The NATS Go `Consume()` loop does not recreate a durable after terminal
`jetstream.ErrConsumerDeleted`; it reports the error and closes. A small
consumer lifecycle manager therefore owns the current `ConsumeContext`. Each
replica uses a buffered local recovery signal to coalesce repeated errors and
independently retries `CreateOrUpdateConsumer` with bounded backoff until it
recreates or binds the shared durable and restarts its local consume loop. A
fixed two-second retry interval keeps recovery quick and avoids a hot loop
without introducing configurable backoff machinery.
Recovery never purges the stream and does not terminate the process, so
access-check and other core handlers stay available. The manager's `Stop()`
stops recovery attempts and the current consume context, preserving the
existing bounded shutdown sequence.

### D4: Typed terminal marker with fail-safe transient default

The selected handler is called with a 90-second `context.WithTimeout`. Existing
validation returns in `genericUpdateAccessHandler`,
`genericDeleteAccessHandler` (`handler_generic.go`), and
`processStandardAccessUpdate` (`handler.go`) wrap proven payload/schema errors
with one terminal sentinel. The wrapper does not duplicate validation and maps
outcomes:

- Success: call `Ack()`.
- Terminal sentinel: call `Term()`.
- Every unmarked error: return without ACK, NAK, or TERM. JetStream redelivers
  the same pending message using `BackOff`.

`Ack()` and `Term()` are successful delivery outcomes only when they return nil.
Increment `sync_ack` or `sync_terminal` only after success. If either call
fails, log and trace the acknowledgement failure and leave the message
unacknowledged; do not issue a fallback NAK.

Terminal errors include malformed JSON, missing required fields, wrong
operation, locally detected invalid reference format, and other proven local
payload/schema validation failures. The existing `FgaService` behavior for a
recognized OpenFGA invalid tuple remains unchanged: remove that tuple and
continue the remaining batch. Any other OpenFGA validation error that propagates
from `FgaService` remains unmarked and transient. Transport errors, context
deadline, connection refused, unknown SDK errors, 408, 409, 429, 5xx, and
operational 401/403 configuration failures also remain transient.
Classification does not use `fgaIs4xx` as a blanket terminal predicate.

TERM stops consumer delivery but does not delete the message from the limits
stream; it remains available until the 24-hour stream retention removes it.

The existing behavior that logs and suppresses a cache-invalidation warning
after a successful OpenFGA write remains a successful delivery outcome.
Positive relationship cache seeding occurs only after `WriteAndDeleteTuples`
returns success. Otherwise a transient OpenFGA failure could leave the message
unacknowledged while making a not-yet-written grant appear authorized from
cache.

### D5: Preserve existing tracing and isolate the ACK subject

The adapter exposes message data, subject, and headers but hides the JetStream
ACK reply subject from `INatsMsg`. The wrapper extracts incoming trace context
and creates the existing `nats.process` consumer span. Additional
JetStream-specific span attributes are deferred.

### D6: Remove the core path without a mixed-version window

`GenericUpdateAccessSubject` and `GenericDeleteAccessSubject` are removed from
`createQueueSubscriptions` (`main.go:387-396`). A focused subscription-wiring
test verifies both are absent from the core set. The operational maintenance
window prevents old core pods and new JetStream pods from processing
concurrently during rollout.

### D7: Infinite reconnect and ordered shutdown

Set `nats.MaxReconnects(-1)` and remove the reconnect-exhaustion `os.Exit(1)`
dependency from the `ClosedHandler` (`main.go:181-201`). Intentional signal
shutdown remains unchanged.

Shutdown uses one explicit consume-context strategy:

1. Call `ConsumeContext.Stop()` to stop new callbacks and discard buffered
   deliveries for later redelivery; do not also call `Drain()`.
2. Wait for `ConsumeContext.Closed()` up to a bounded grace period
   (`accessMutationShutdownGrace`, 5s). `Closed()` only fires once any
   in-flight delivery attempt has returned, so canceling the service context
   beforehand would abort an attempt that might otherwise succeed within
   milliseconds, forcing an unnecessary `BackOff` cycle on redelivery after
   restart. Waiting first lets that attempt finish normally in the common
   case.
3. Cancel the service context only if the grace period elapses first, then
   wait for `ConsumeContext.Closed()`; this bounds total shutdown time if an
   attempt is genuinely stuck, instead of blocking for the full 90s attempt
   deadline.
4. Drain the NATS connection.

Every `ClosedHandler` path must call `gracefulCloseWG.Done()` exactly once,
including an unexpected terminal connection close. This prevents
`gracefulCloseWG.Wait()` from blocking after removal of `os.Exit(1)`.

### D8: Stream provisioned by the fga-sync Helm chart

Add `charts/lfx-v2-fga-sync/templates/nats-stream.yaml` and a `values.yaml`
entry mirroring the existing platform Stream CRD pattern: subjects
`lfx.fga-sync.update_access` and `lfx.fga-sync.delete_access`,
`retention: limits`, `maxAge: 24h`, `keep: true`, file storage, and three
replicas to match the NATS cluster.

### D9: Authoritative exhaustion and safe recovery

Expose `sync_ack`, `sync_transient_attempts`, `sync_terminal`, and
`sync_max_deliver_exhausted` through `/debug/vars`. Subscribe to JetStream
max-delivery advisories; those server events, not a callback prediction, are
authoritative for retry exhaustion.

`sync_transient_attempts` increments once for each unmarked failed attempt, so
one exhausted message may increment it up to seven times.
`sync_max_deliver_exhausted` increments once from the server advisory while a
fga-sync replica is connected. Phase 1 deliberately uses a core NATS queue
subscription rather than a second durable advisory stream. External platform
monitoring must subscribe and alert on the same advisory subject to cover final
expiry after the last replica disconnects; durable in-service advisory capture
is deferred.
`sync_terminal` increments when the wrapper sends TERM; that locally known
outcome is logged without a separate termination-advisory subscription.

Logs include classification, stream sequence, delivery count, object type, and
safe resource context. Recovery does not blindly replay the retained message.
Operators use the advisory evidence to have the owning service re-read current
database state and publish a fresh update or deletion as appropriate.

The max-delivery advisory supplies the stream sequence but not the object
context. The advisory handler uses `Stream.GetMsg` with that sequence and
decodes the retained `GenericFGAMessage`. If lookup or decoding fails, it still
increments `sync_max_deliver_exhausted` and logs the advisory identity, stream
sequence, delivery count, and enrichment error.

### D10: Fenced cutover and rollback

The one-time gate is an operational platform maintenance window covering every
publisher of either migrated subject. This change adds no publisher
maintenance-mode feature. The runbook must identify the concrete platform
controls used to pause mutation ingress and asynchronous producers. Operations
confirms that update and delete publication has stopped.

Cutover order:

1. Deploy and verify expanded LFXV2-2830 and LFXV2-2828, and verify all other
   publishers are asynchronous-only.
2. Provision the stream without starting its consumer.
3. Enable the maintenance window and verify zero continuing publication for
   both subjects.
4. Drain and stop both old core subscribers.
5. Purge the core-processed duplicate backlog and verify the stream is empty.
6. Deploy the JetStream-enabled binary, create its `DeliverNewPolicy` durable
   consumer against the empty stream, and verify neither core subscription
   remains.
7. Release the maintenance window.

Rollback keeps publication paused until every durable message has an explicit,
order-safe disposition:

1. Preferred: allow the healthy durable consumer to drain the stream backlog in
   sequence order until every message is acknowledged and the backlog is empty.
   Then stop the consumer and restore both core subscribers without overlap.
2. Fallback when the consumer cannot safely drain: record every affected object
   from pending stream sequences, stop the consumer, restore both core
   subscribers, have owning services re-read authoritative database state and
   publish fresh updates or deletions, verify convergence, and purge the
   superseded retained messages.
3. Forbidden: release the maintenance window with undispositioned messages or
   blindly bulk-replay retained payloads.

Any later forward cutover repeats the purge-and-empty-stream handoff.

## Risks / Trade-offs

- **Global head-of-line blocking:** one transient message pauses all update and
  delete processing for approximately 94 minutes. A deletion queued behind the
  failure is delayed, but it cannot be bypassed or later undone by an older
  update. This is accepted because later messages remain durable.
- **Persistent propagated validation failure:** a locally valid payload that
  OpenFGA persistently rejects with an unrecognized validation error consumes
  the full approximately 94-minute global slot, then requires authoritative
  recovery. Recognized invalid-tuple errors are excluded because `FgaService`
  removes the invalid tuple and continues the batch.
- **Permanent OpenFGA outage or configuration failure:** every OpenFGA error
  remains transient unless local validation proves otherwise. A persistent
  outage, 401/403 configuration problem, throttling condition, or model/store
  mismatch can therefore consume approximately 94 minutes per message before
  advancing to the next sequence. If publication continues, the backlog can
  outlive the 24-hour stream retention and expire. Monitor exhaustion,
  consumer lag, and oldest-message age; recovery republishes fresh current
  state rather than replaying retained snapshots.
- **Durable consumer state loss:** ordinary pod/NATS outages are safe because
  the existing durable cursor survives. If that durable state is deleted or
  otherwise lost, `DeliverNewPolicy` recreates it at the stream tail so new
  processing resumes automatically without blocking access-check/core
  handlers. Retained pre-recreation history is skipped and expires naturally.
  This explicit availability-over-completeness boundary is safer than
  `DeliverAllPolicy` replay and simpler than coordinating a destructive purge
  across replicas. Runtime `ErrConsumerDeleted` closes `Consume()`, so the
  lifecycle manager must recreate/rebind and restart consumption without
  exiting the service; startup-only recreation is insufficient.
- **Terminal misclassification:** only the local terminal sentinel is
  terminated; unknown and operational errors retry by default.
- **Exhaustion leaves state stale:** advisories alert operators and recovery
  publishes fresh authoritative state instead of replaying an old snapshot.
- **Mixed-version double-processing:** the maintenance gate, verified backlog
  purge, and empty-stream consumer creation are required; a normal rolling
  update without the runbook is not supported.
- **Stream unavailable at startup:** consumer binding fails startup clearly.
- **Readiness coverage:** `/readyz` reports NATS connection/draining state but
  does not attest that the JetStream consume loop is healthy. Consumer-aware
  readiness redesign remains outside Phase 1.
- **24-hour stream expiry:** messages older than the retention limit cannot be
  recovered from the stream; operations must respond to advisories within that
  existing Phase 1 limit.

## Migration Plan

Before production rollout approval, automated verification in the external
platform integration environment stops OpenFGA, publishes an update followed by
a deletion, confirms the deletion remains pending behind the failed update,
restores OpenFGA before exhaustion, and verifies the update then deletion apply
in order with no publisher-managed tuples remaining and pre-existing `team:*`
grants unchanged. This drill does not run inside the live D10 maintenance window
or gate-release sequence. Also verify infinite NATS reconnect, offline publish
recovery, and consumer shutdown redelivery.

Production rollout and rollback then follow D10.

A separate disposable non-production drill verifies the D3 state-loss
boundary: after test history is processed, delete the test durable, restart the
service so it recreates the consumer, confirm retained history is not
redelivered, publish and ACK a new message, and confirm access-check/core
handlers remain available. Never delete the production durable to perform this
verification.

## Open Questions

- None blocking. The maintenance gate and globally serialized retry model are
  accepted Phase 1 decisions.
