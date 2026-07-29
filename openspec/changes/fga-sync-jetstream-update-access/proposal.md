# Durable JetStream delivery for FGA sync (Phase 1: update/delete access)

Tracks Jira LFXV2-1914.

## Why

fga-sync consumes access-sync messages over core NATS `QueueSubscribe`
(`main.go:329-367`), which has no acknowledgement semantics. When OpenFGA (or
NATS) is unavailable, the handler error is logged (`main.go:346-354`) but the
message is already considered consumed — there is no redelivery, so the FGA
tuple update is permanently lost and authorization state drifts from the
database. The connection also exits the process after the default 60 reconnect
attempts (`main.go:181-201`), turning a transient NATS blip into an outage.

This is the first of two phases. Phase 1 establishes the durable-delivery
foundation and migrates `lfx.fga-sync.update_access` together with
`lfx.fga-sync.delete_access`. Phase 2 (LFXV2-2831) migrates `member_put` and
`member_remove`.

`delete_access` is included because ordering only `update_access` is unsafe. An
older update can remain pending during an OpenFGA outage while a later
`delete_access` succeeds immediately on core NATS; when the older update retries,
it can recreate authorization tuples for the deleted resource. Processing both
subjects through the same ordered consumer ensures deletion cannot be bypassed
by an older delayed update.

Retained production traces observed 126,213 asynchronous `update_access`
publishes and no synchronous FGA requests during the 30-day window. This shows
the latent request/reply paths were idle in observed traffic, not impossible:
the code audit found those paths in project-service and committee-service.
LFXV2-2830 and LFXV2-2828 remove them and are blocking deployment dependencies.

## What Changes

- Add a JetStream **Stream** (via the fga-sync Helm chart) that captures
  `lfx.fga-sync.update_access` and `lfx.fga-sync.delete_access`, so both
  operations are persisted in one stream order before delivery. Retention
  `maxAge: 24h`.
- Add one shared durable **pull consumer** for both subjects with explicit
  acknowledgement. Phase 1 limits processing to one globally in-flight message
  so a failed older full-state update cannot be bypassed by a newer update or
  deletion. Configure `MaxAckPending: 1`, `MaxDeliver: 7`, a 90-second
  processing deadline, and consumer `BackOff` values of `2m`, `2m`, `5m`,
  `10m`, `15m`, and `30m`. `BackOff` supplies the acknowledgement timeout and
  delayed redelivery schedule. The six values cover the intervals between seven
  deliveries; the last `30m` value repeats once before the server emits the
  max-delivery advisory, giving approximately 94 minutes from initial delivery
  to authoritative exhaustion. The accepted cost is that one transiently
  failing message stalls both subjects across replicas during that window;
  later messages remain durably stored.
- Create the durable with `DeliverNewPolicy`. At the initial fenced cutover the
  stream is verified empty, so `DeliverNewPolicy` and `DeliverAllPolicy` are
  equivalent. Normal restarts bind the existing durable and resume its stored
  cursor. If the durable state itself is lost while the limits-retention stream
  still contains up to 24 hours of history, automatic recreation starts at the
  current stream tail instead of replaying messages whose prior disposition is
  unknown. This avoids a stale update recreating authorization after a later
  deletion, does not purge the stream, and requires no manual intervention to
  restore new-message processing. The accepted availability-over-completeness
  trade-off is that retained messages predating recreation are skipped and age
  out under `maxAge`; authoritative reconciliation is required if that rare
  state-loss event must be repaired.
- Treat runtime `jetstream.ErrConsumerDeleted` as a consumer-loop recovery
  signal, not a process-fatal error. Keep access-check and other core handlers
  running while one local recovery loop per replica retries every two seconds
  to recreate/bind the durable with the same `DeliverNewPolicy` configuration
  and restarts consumption. Shutdown must stop whichever consume context is
  current. This
  closes the gap where startup can recreate a missing durable but a deletion
  while pods remain running otherwise leaves `Consume()` permanently closed.
- Add a **delivery outcome wrapper** around `genericUpdateAccessHandler` and
  `genericDeleteAccessHandler`:
  - Edit the existing validation returns in `genericUpdateAccessHandler`,
    `genericDeleteAccessHandler`, and `processStandardAccessUpdate` to wrap only
    proven local payload/schema failures with a typed terminal sentinel. The
    wrapper does not duplicate validation, and tuple-generation behavior is
    unchanged.
  - Leave every unmarked error **unacknowledged** by default, including unknown
    SDK errors, OpenFGA unreachable, timeout, and 5xx. JetStream uses the
    consumer `BackOff` schedule to redeliver the same message; the wrapper does
    not call `Nak()` or `NakWithDelay()`.
  - **TERM** on the terminal sentinel, including malformed JSON, missing
    `object_type`/`uid`, wrong `operation`, locally detected invalid reference
    format, and other proven local payload/schema validation errors. TERM stops
    delivery but the message remains in the limits-retention stream for up to
    24 hours.
  - Preserve the existing `FgaService` behavior that removes a recognized
    OpenFGA invalid tuple and continues the remaining batch. Any other OpenFGA
    validation error that propagates from `FgaService` is unmarked and therefore
    transient; do not classify it by HTTP 400 alone.
  - Authentication, authorization, timeout, conflict, throttling, transport,
    and 5xx failures are not classified as terminal merely because of their
    HTTP status class. In particular, treating every no-status error as terminal
    would TERM an OpenFGA connection/SDK failure and recreate the tuple-loss
    failure this change is intended to fix.
  - **ACK** when processing succeeds. Existing cache-invalidation warning
    behavior remains unchanged and is outside this delivery migration.
  - Seed positive relationship cache entries only after the corresponding
    OpenFGA write succeeds. A transient write failure must not create a cached
    authorization grant before redelivery.
  - Increment `sync_ack` or `sync_terminal` only after the corresponding
    `Ack()` or `Term()` call succeeds. If either call returns an error, log the
    acknowledgement failure and leave the message unacknowledged for
    redelivery.
  - The JetStream message adapter hides the JetStream ACK reply subject from
    `INatsMsg.Reply`, so the existing handler cannot send application `"OK"` to
    the ACK endpoint.
- Apply a 90-second per-message processing deadline, shorter than the first
  two-minute `BackOff` value, so a slow OpenFGA operation cannot be redelivered
  concurrently while the first attempt is still running.
- Rely on consumer-wide `MaxAckPending: 1`: an unacknowledged transient failure
  occupies the only pending slot, so newer updates and deletions remain stored
  until the failed stream sequence succeeds, terminates, or reaches
  `MaxDeliver`. This preserves cross-subject stream order without a custom
  sequence gate, source revisions, or distributed per-object locking.
- **Cut both subjects off the core `QueueSubscribe` path** in the same change,
  so exactly one delivery path exists for each. `member_put`, `member_remove`,
  and both `access_check` subjects remain on core NATS. Phase 2 migrates the
  membership subjects; synchronous access-check queries remain out of scope.
- Use a fenced deployment handoff. During the maintenance window, stop
  publication, drain old core subscribers, purge the temporary duplicate
  backlog captured by the stream, verify the stream is empty, and then create a
  `DeliverNew` durable consumer. This concrete purge mechanism avoids a dynamic
  `OptStartSeq` configuration while ensuring old core-processed messages are not
  replayed. Production was verified before rollout to have neither the
  `fga-sync-events` stream nor the access-mutation durable, so no immutable
  existing consumer policy requires migration.
- Set `MaxReconnects(-1)` (infinite) and stop relying on `os.Exit(1)` for
  reconnect exhaustion; keep the existing graceful-shutdown path.
- On graceful shutdown, call `ConsumeContext.Stop()` to stop new callbacks and
  discard buffered deliveries, cancel the service context so in-flight work
  returns unacknowledged, wait for `ConsumeContext.Closed()`, and then drain the
  NATS connection. Do not call both `Stop()` and `Drain()`. Ensure every
  `ClosedHandler` path releases `gracefulCloseWG`.
- Add expvar counters at `/debug/vars`: `sync_ack`,
  `sync_transient_attempts`, `sync_terminal`, and
  `sync_max_deliver_exhausted`, and consume JetStream max-delivery advisories as
  the authoritative exhausted-message event type. Phase 1 uses a simple core
  NATS queue subscription: connected fga-sync replicas increment and enrich the
  in-process counter, while external platform monitoring must capture and alert
  on the same advisory subject when no replica is connected. A second durable
  advisory stream is intentionally deferred. TERM outcomes are already known
  locally and do not require a termination-advisory subscription. Use the
  advisory stream sequence to fetch the retained message with `Stream.GetMsg`,
  then decode the safe object context needed by recovery. If lookup or decoding
  fails, still increment the exhaustion counter and log the advisory identity
  plus enrichment error.
- Preserve trace-context extraction and the existing `nats.process` consumer
  span. Additional JetStream-specific span attributes are deferred.
- Update `docs/fga-sync-contract.md` and `docs/client-guide.md` for
  `update_access` and `delete_access`: asynchronous-only publishing,
  at-least-once delivery,
  ACK/unacknowledged-redelivery/TERM behavior, and the fact that HTTP `X-Sync`
  does not wait for OpenFGA convergence.
- Enforce the asynchronous-only publisher contract for both subjects after
  LFXV2-2830 and LFXV2-2828 deploy and all other publishers are verified
  asynchronous-only. Subjects and envelopes remain unchanged, and applicable
  indexer request/reply behavior is unaffected.

## Required Delivery Guarantees

- **Ordered full-state application and deletion:** `update_access` is a
  full-state replacement and `delete_access` removes publisher-managed state
  while preserving existing externally managed `team:*` grants. Identical
  update replay is safe, but an older update applied after a newer update or
  deletion is not. Phase 1 therefore serializes both subjects and proves that a
  transiently failed older message cannot be bypassed. The accepted trade-off
  is global head-of-line blocking for approximately 94 minutes; later messages
  remain durably stored rather than being lost.
- **Bounded, observable retry:** retry timing spans the supported OpenFGA outage
  test and permits seven total processing attempts before authoritative
  exhaustion at approximately 94 minutes.
  Transient failures remain unacknowledged and are redelivered by the
  server-managed `BackOff` schedule. JetStream advisories, not a callback-side
  prediction alone, identify final exhaustion. If OpenFGA remains unavailable
  or operationally misconfigured (including persistent 401/403, throttling, or
  model/store errors), each message can consume the global slot until
  exhaustion before the next sequence advances. Continued publication can then
  outpace processing and messages older than the 24-hour stream limit can
  expire. This is not automatic OpenFGA recovery; advisory/lag/oldest-message
  monitoring and fresh authoritative publication remain required.
- **Recoverable final failure:** terminal and exhausted messages are recorded
  with stream sequence and classification. Operators use that evidence to have
  the owning service re-read its authoritative database state and publish a
  fresh update or deletion as appropriate. The retained payload is not blindly
  replayed because newer state may already have succeeded.
- **Automatic durable-state-loss recovery:** if the durable is missing, it is
  recreated with `DeliverNewPolicy` at the current stream tail. Existing
  retained history is not replayed or purged, new messages begin processing
  immediately, and access-check/core-subscription processing is not blocked.
  This is distinct from an ordinary pod or NATS reconnect, where the durable
  still exists and its pending messages resume normally. If deletion is
  detected by a running consume loop, only that loop is restarted; the process
  and unrelated handlers remain available.
- **No mixed transport window:** rollout and rollback fence old core subscribers
  from the JetStream consumer and explicitly decide the disposition of retained
  pre-cutover or rollback messages.

## Migration and Rollback

The cutover uses a one-time operational platform maintenance window covering all
publishers of `update_access` or `delete_access`. This change does not add
maintenance-mode code to publisher services. The runbook defines how operations
pauses mutation ingress and asynchronous producers using platform controls, then
verifies that publication has stopped. Messages are never silently discarded.
The cutover itself does not discard unprocessed messages: only the
core-processed duplicate backlog is purged while publication is paused. A later
loss of the durable consumer's own state is a separate disaster-recovery
boundary: automatic `DeliverNewPolicy` recreation intentionally skips retained
history rather than risking stale authorization replay.

1. Deploy and verify project-service LFXV2-2830 and committee-service
   LFXV2-2828 so neither subject can use request/reply, and verify all other
   publishers are asynchronous-only.
2. Provision the stream without starting the durable consumer. Core subscribers
   continue processing while the stream captures a temporary duplicate backlog.
3. Enable the operational maintenance window and verify publication of both
   subjects has stopped.
4. Drain and stop both old fga-sync core subscribers.
5. Purge the stream's temporary duplicate backlog and verify the stream is
   empty.
6. Create the durable pull consumer with `DeliverNewPolicy`, start it against
   the empty stream, and verify that neither core subscription remains.
7. Release the maintenance window after the durable consumer is active.

The OpenFGA-outage drill is pre-production platform verification, not a
cutover gate-release condition.

Rollback uses the same gate in reverse and SHALL NOT release it with
undispositioned durable messages. First stop publication and drain the
JetStream backlog in stream order through the healthy consumer until it is empty
and acknowledged; then stop the consumer and restore both core subscribers
without overlap. If the consumer cannot safely drain, record every affected
object from the pending stream sequences, restore the core subscribers, have
each owning service re-read current authoritative database state and publish a
fresh update or deletion, verify convergence, and purge the superseded retained
messages before releasing the gate. Blind bulk replay of retained payloads is
forbidden. A later forward cutover repeats the purge-and-empty-stream handoff.

## Capabilities

### New Capabilities

- `fga-sync-message-delivery`: How fga-sync durably receives and acknowledges
  ordered update/delete access messages — stream persistence, durable consumer,
  transient-vs-terminal delivery outcomes, redelivery on OpenFGA downtime, and
  NATS reconnect behavior.

### Modified Capabilities
<!-- None. There is no existing spec in openspec/specs/ for message delivery;
     this introduces the first delivery spec rather than modifying one. -->

## Impact

- **Code:** `main.go` (NATS connect options, subscription wiring, consumer
  lifecycle and shutdown), a new pull-consumer/adapter/ack wrapper,
  `pkg/constants/nats.go` (stream/consumer names), a typed terminal validation
  sentinel with fail-safe transient default, validation-return edits in
  `handler_generic.go` and `handler.go`,
  `mock.go` (JetStream message mock), and a small delivery metrics/advisory
  helper. Tuple generation remains unchanged. `FgaService` moves positive cache
  seeding after successful OpenFGA writes to prevent a failed delivery from
  temporarily caching access that OpenFGA did not grant.
- **Helm:** new `charts/lfx-v2-fga-sync/templates/nats-stream.yaml` + `values.yaml`
  stream config (name, subject, retention, replicas), following the
  committee-service `nats-streams.yaml` pattern. Requires the NACK JetStream
  controller configured in the platform NACK JetStream values.
- **Docs:** `docs/fga-sync-contract.md`, `docs/client-guide.md`, the manual
  recovery/cutover procedure, and `CLAUDE.md` if reconnect behavior changes.
  LFXV2-2830 and LFXV2-2828 update the project-service and committee-service
  `docs/fga-contract.md` files.
- **Runtime contract:** `update_access` and `delete_access` become
  asynchronous-only and at-least-once. Subjects and envelopes are unchanged;
  the optional application `"OK"` reply is no longer supported for either
  subject. Ordered processing prevents a delayed update from bypassing deletion.
- **Cross-repo:** project-service LFXV2-2830 and committee-service LFXV2-2828
  expand to both subjects and remain blocking Phase 1 dependencies. Other
  publishers must be verified asynchronous-only. Member-service LFXV2-2829 and
  the membership mutation subjects belong to Phase 2 LFXV2-2831.
- **Verification:** automated platform integration coverage stops OpenFGA,
  publishes an `update_access` followed by `delete_access`, verifies deletion
  remains pending behind the failed update, restores OpenFGA before retry
  exhaustion, and verifies the update then deletion are applied in stream order
  with no publisher-managed tuples remaining and pre-existing `team:*` grants
  unchanged. A separate disposable non-production drill deletes and recreates
  the durable, verifies retained history is skipped without a stream purge,
  verifies a newly published mutation is delivered, and confirms unrelated
  access-check/core handlers remain available. Never run the consumer-deletion
  drill in production.

## Non-goals

- Migrating `member_put` and `member_remove` (Phase 2).
- Migrating the request/reply subjects `lfx.access_check.request` and
  `lfx.access_check.read_tuples` — these are synchronous, cache-first queries, not
  tuple-loss paths; JetStream would change their reply semantics.
- Preserving request/reply or an application `"OK"` response for
  `lfx.fga-sync.update_access` or `lfx.fga-sync.delete_access`.
- Changing publishers to JetStream publish APIs or adding publisher-side storage
  acknowledgements.
- Adding event IDs, source revisions, distributed per-object locks, a dedicated
  quarantine/DLQ, or a reconciliation service.
- Redesigning cache invalidation, publisher authorization boundaries,
  `/debug/vars` security, readiness probes, or existing payload/tuple logging.
  The existing `/readyz` endpoint reports NATS connection/draining state only;
  it does not attest that the JetStream consume loop is healthy.
- Adding an in-repo NATS+OpenFGA integration harness. Automated outage coverage
  remains in the platform integration stack (`lfx-v2-helm`/`load-mock-data`).
- Prometheus metrics (expvar is used to match the existing pattern).
- Changing the `GenericFGAMessage` envelope or any tuple format.
