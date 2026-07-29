# LFXV2-1914 validation update draft

## Validation update — Path 1 confirmed

Production and GitHub investigations support migrating
`lfx.fga-sync.update_access` and `lfx.fga-sync.delete_access` together to direct
JetStream consumption with an asynchronous-only publisher contract.

### Production evidence

Datadog production data for the last 30 days shows:

- 193,165 asynchronous `nats.publish` spans to `lfx.fga-sync.*`
- 0 synchronous `nats.request` spans to `lfx.fga-sync.*`
- All 126,213 observed `update_access` messages were asynchronous
- One `lfx.fga-sync.update_access` request failed because OpenFGA returned
  `context deadline exceeded`
- 39 NATS disconnect events were recorded
- No max-reconnect exhaustion event was observed over 90 days

The OpenFGA timeout is an example of the ticket's failure mode: core NATS had
already delivered the message, so the handler failure could not trigger
redelivery.

### GitHub caller audit

All 27 active LFX v2 repositories were scanned on their default branches.

Findings:

- Runtime `X-Sync: true` exists only in `linuxfoundation/lfx-self-serve`
- 10 header declarations produce 16 request callsites:
  - Project: 3
  - Committee: 1
  - Meeting: 2
  - Survey: 1
  - Voting: 2
  - Org Lens/member-service: 7
- None synchronously reaches `lfx.fga-sync.update_access`
- The only typed `XSync: true` assignment outside the UI is a
  committee-service test
- Voting-service was separately verified to ignore `X-Sync` and publish FGA
  messages with `PublishMsg`

Although no production caller currently uses synchronous FGA request/reply,
project-service and committee-service contain latent paths that can route Phase
1 access mutations through request/reply.

### Decision and Phase 1 scope

Proceed with direct JetStream delivery for `lfx.fga-sync.update_access` and
`lfx.fga-sync.delete_access`.

`delete_access` is included because update-only ordering is unsafe. An older
update can remain pending during an outage while a later deletion succeeds on
core NATS; the delayed update could then retry and recreate authorization tuples
for the deleted resource. One stream and consumer preserve update/delete order.

1. Provision a limits-retention stream for both subjects with `maxAge: 24h`.
2. Consume through one shared durable pull consumer with explicit
   acknowledgement.
3. Remove both subjects from core NATS queue subscriptions in the same release.
4. Define both subjects as asynchronous-only while preserving their subjects
   and `GenericFGAMessage` envelopes.
5. Set `nats.MaxReconnects(-1)` and remove reconnect-exhaustion process exit.

`member_put` and `member_remove` remain on core NATS until Phase 2, LFXV2-2831.
The synchronous `access_check` and `read_tuples` subjects are not part of this
migration.

### Publisher prerequisites and contract clarification

The original acceptance criterion that publishers require no changes is not
literally compatible with direct JetStream capture. A latent NATS `Request`
could receive JetStream's persistence acknowledgement instead of fga-sync's
application `"OK"` response.

The subject and payload do not change, but these linked stories must deploy
before stream provisioning:

- LFXV2-2830 — project-service makes update/delete access asynchronous-only.
- LFXV2-2828 — committee-service makes update/delete access asynchronous-only.

After those changes:

- Publishers use core NATS `Publish` for both subjects.
- HTTP `X-Sync: true` may still wait for applicable indexer acknowledgements.
- `X-Sync` does not guarantee OpenFGA convergence before the HTTP response.
- The JetStream adapter hides its ACK reply subject from `INatsMsg.Reply`, so
  the existing handler cannot send application `"OK"` to the ACK endpoint.

Required contract documentation:

- `lfx-v2-fga-sync/docs/fga-sync-contract.md`
- `lfx-v2-fga-sync/docs/client-guide.md`
- `lfx-v2-project-service/docs/fga-contract.md`
- `lfx-v2-committee-service/docs/fga-contract.md`

### Consumer and delivery policy

Use one shared durable pull consumer:

- Stream: `fga-sync-events`
- Consumer: `fga-sync-access-mutation-consumer`
- Subjects: `lfx.fga-sync.update_access`, `lfx.fga-sync.delete_access`
- `DeliverPolicy: DeliverNewPolicy`
- `AckPolicy: AckExplicitPolicy`
- `MaxAckPending: 1`
- `MaxDeliver: 7`
- Processing deadline: 90 seconds
- `BackOff: [2m, 2m, 5m, 10m, 15m, 30m]`

`update_access` is a full-state replacement and `delete_access` removes that
state. Consumer-wide `MaxAckPending: 1` allows one globally pending message and
prevents an older delayed update from being applied after a later deletion.

The six BackOff values cover the intervals between seven deliveries; the final
`30m` value repeats before the max-delivery advisory. Authoritative exhaustion
therefore occurs at approximately 94 minutes. The accepted trade-off is global
head-of-line blocking for both subjects; later messages remain durably stored.

`DeliverNewPolicy` is safe for this brand-new rollout because production
read-only verification found neither the `fga-sync-events` stream nor the
access-mutation durable, and the fenced cutover creates the consumer only after
the stream is verified empty. Normal restarts resume the durable's stored
cursor. If the durable state itself is later lost, automatic recreation starts
at the current stream tail without replaying or purging ambiguous retained
history. New messages resume without manual intervention or blocking unrelated
access-check/core handlers; retained pre-recreation history expires under the
24-hour limit and may require fresh source-of-truth reconciliation.
If deletion occurs while replicas remain running, `ErrConsumerDeleted` closes
the affected consume loop; each replica keeps unrelated handlers available and
retries shared durable creation/binding every two seconds until local
consumption restarts.

Delivery outcomes:

- **ACK:** processing succeeds. A cache-invalidation warning after a successful
  OpenFGA write remains a successful outcome. Positive relationship cache
  entries are seeded only after the OpenFGA write succeeds.
- **Unacknowledged:** every error not explicitly marked terminal, including
  OpenFGA unavailable, timeout, connection failure, unknown SDK errors, 408,
  409, 429, 5xx, and operational 401/403. JetStream applies the configured
  `BackOff`; the wrapper does not call `Nak()` or `NakWithDelay()`.
- **TERM:** only proven local payload/schema validation failures wrapped by a
  typed terminal sentinel, such as malformed JSON, missing required fields,
  wrong operation, or locally detected invalid reference format.

ACK and TERM counters increment only after the acknowledgement call succeeds.
If `Ack()` or `Term()` returns an error, the failure is logged and traced and the
message remains unacknowledged for redelivery.

Recognized OpenFGA invalid-tuple errors retain the existing behavior: remove the
invalid tuple and continue the remaining batch. Every other OpenFGA validation
error that propagates from `FgaService` remains unmarked and transient; HTTP 400
alone is not terminal. A persistent propagated validation error can therefore
occupy the global slot for approximately 94 minutes before exhaustion and
authoritative recovery.

If OpenFGA remains unavailable or operationally misconfigured, messages exhaust
sequentially behind the global one-message limit. Continued publication can
outpace processing and messages can exceed the 24-hour retention window.
Max-delivery advisories, consumer lag, and oldest-message age therefore require
monitoring; recovery publishes fresh authoritative state rather than replaying
expired snapshots.

This fail-safe classification prevents an unknown operational failure from
being terminated and silently recreating the tuple-loss problem.

### Exhaustion, recovery, and observability

The service will expose these `/debug/vars` counters:

- `sync_ack`
- `sync_transient_attempts`
- `sync_terminal`
- `sync_max_deliver_exhausted`

JetStream max-delivery advisories are the authoritative retry-exhaustion event
type. A connected fga-sync replica consumes them through a core NATS queue
subscription, fetches the retained message by stream sequence with
`Stream.GetMsg`, and decodes safe object context. External platform monitoring
must capture and alert on the same subject when no replica is connected.

Recovery does not blindly replay a retained message because newer state may
already exist. Operators have the owning service re-read current database state
and publish a fresh update or deletion as appropriate.

Existing trace-context propagation and the `nats.process` span remain.
`/readyz` continues to report NATS connection/draining state but does not attest
that the JetStream consume loop is healthy; consumer-aware readiness redesign is
outside Phase 1.

### Implementation plan

1. Add stream and consumer constants and the durable pull-consumer lifecycle.
2. Add the JetStream `INatsMsg` adapter and typed terminal sentinel at existing
   handler validation returns.
3. Apply the 90-second processing deadline and ACK/unacknowledged/TERM policy.
4. Remove the core update and delete subscriptions; retain membership and query
   subscriptions.
5. Add infinite NATS reconnect and ordered shutdown: call
   `ConsumeContext.Stop()`, cancel service context, wait for `Closed()`, then
   drain NATS. Every `ClosedHandler` path releases the wait group.
6. Add counters, structured logs, best-effort in-service max-delivery advisory
   handling, external advisory monitoring, and the fresh-state recovery
   procedure.
7. Add the Helm Stream CRD with 24-hour retention.
8. Add unit coverage for consumer configuration, outcomes, timeout, adapter
   reply isolation, reconnect, shutdown, and subscription wiring.
9. Add external platform integration coverage using real JetStream and OpenFGA.
10. Update fga-sync and publisher contract documentation.

### Pre-production verification

The Jira-required external platform integration test will:

1. Stop OpenFGA.
2. Publish an update followed by deletion for the same object.
3. Verify the update is redelivered and the deletion remains pending.
4. Restore OpenFGA before retry exhaustion.
5. Verify update then deletion are applied in order, no publisher-managed tuples
   remain, and pre-existing externally managed `team:*` grants are unchanged.

This test runs before production rollout approval, outside the live maintenance
window. Production logs and metrics supplement but do not replace it.

### Production cutover and rollback

Use a one-time operational platform maintenance window; no publisher
maintenance-mode feature is added by this change.

Cutover order:

1. Deploy and verify LFXV2-2830 and LFXV2-2828.
2. Provision the stream without starting the durable consumer; core subscribers
   continue processing.
3. Pause mutation ingress and asynchronous producers, then verify publication
   of both subjects has stopped.
4. Drain and stop both old core subscribers.
5. Purge the duplicate pre-cutover backlog and verify the stream is empty.
6. Create the `DeliverNewPolicy` consumer against the empty stream and verify
   neither core subscription remains.
7. Release the maintenance window.

Rollback keeps publication paused until every durable message has an explicit,
order-safe disposition:

1. Preferred: record the backlog and counters, drain and ACK every sequence,
   verify no exhaustion occurred, stop the consumer, then restore both core
   subscribers without overlap.
2. Fallback if the consumer cannot safely drain: record every affected object
   from pending stream sequences, stop the consumer, restore core subscriptions,
   have owning services publish fresh state derived from their databases, verify
   convergence, and purge the superseded retained messages.
3. Forbidden: release the window with undispositioned durable messages or
   blindly bulk-replay retained payloads.

### Acceptance-criteria alignment

- **OpenFGA failure results in JetStream redelivery:** satisfied for Phase 1
  update/delete access, verified by the external outage integration test.
- **fga-sync reconnects indefinitely:** satisfied by `MaxReconnects(-1)` and
  removal of reconnect-exhaustion `os.Exit(1)`.
- **Existing contracts continue:** subjects and payload envelopes remain
  unchanged, but both commands become asynchronous-only. LFXV2-2830 and
  LFXV2-2828 are blocking prerequisites; applicable indexer `X-Sync` behavior
  remains unchanged.
- **Clear terminal/transient policy:** satisfied by typed terminal validation,
  fail-safe transient default, seven-delivery cap, max-delivery advisories,
  structured logs, and expvar counters.

Recommended clarification to the third acceptance criterion:

> Existing update/delete access subjects and payload envelopes remain unchanged.
> Publishers use asynchronous core NATS `Publish`; project-service and
> committee-service no longer route either subject through request/reply.
> Applicable indexer `X-Sync` behavior remains unchanged.
