## 1. Blocking dependencies and contract

- [ ] 1.1 Verify project-service LFXV2-2830 is deployed and cannot route
  `update_access` or `delete_access` through request/reply.
- [ ] 1.2 Verify committee-service LFXV2-2828 is deployed and cannot route
  committee or invite update/delete access through request/reply.
- [ ] 1.3 Verify applicable indexer `X-Sync` request/reply behavior remains
  unchanged while FGA publication is asynchronous.
- [ ] 1.4 Audit all other publishers and verify both migrated subjects are
  asynchronous-only before stream provisioning.

## 2. Constants and consumer configuration

- [x] 2.1 Add stream `fga-sync-events` and durable consumer
  `fga-sync-access-mutation-consumer` constants to `pkg/constants/nats.go`.
- [x] 2.2 Configure one shared durable pull consumer for the stream containing
  `lfx.fga-sync.update_access` and `lfx.fga-sync.delete_access` with
  `DeliverNewPolicy`, `AckExplicitPolicy`, `MaxAckPending: 1`, `MaxDeliver: 7`,
  and `BackOff: [2m, 2m, 5m, 10m, 15m, 30m]`.
- [x] 2.3 Do not configure a separate `AckWait`; document that consumer
  `BackOff` supplies the acknowledgement timeout. Verify the final `30m` value
  repeats before the max-delivery advisory, producing approximately 94 minutes
  from initial delivery to authoritative exhaustion.
- [x] 2.4 Enforce a 90-second processing context deadline for every delivery
  attempt.

## 3. Terminal marker and message adapter

- [x] 3.1 Add one terminal-validation sentinel without using `fgaIs4xx` as a
  blanket terminal predicate; every unmarked error defaults to transient.
- [x] 3.2 Wrap malformed payloads, missing fields, wrong operation, locally
  detected invalid reference format, and other proven local payload/schema
  validation failures at their existing validation points in
  `genericUpdateAccessHandler`,
  `genericDeleteAccessHandler` (`handler_generic.go`), and
  `processStandardAccessUpdate` (`handler.go`); do not duplicate validation in
  the wrapper.
- [x] 3.3 Preserve the existing `FgaService` behavior that removes a recognized
  OpenFGA invalid tuple and continues the batch. Verify every other propagated
  OpenFGA validation error remains unmarked and transient.
- [x] 3.4 Verify connection refused, context deadline, unknown SDK errors, 408,
  409, 429, 5xx, and operational 401/403 failures remain unmarked and transient.
- [x] 3.5 Add a JetStream adapter for `INatsMsg` that exposes data, subject, and
  headers but returns an empty application reply and never responds `"OK"` to
  the JetStream ACK subject.
- [x] 3.6 Preserve the existing successful outcome when an OpenFGA write
  succeeds but cache invalidation logs a warning.
- [x] 3.7 Seed positive relationship cache entries only after the corresponding
  OpenFGA write succeeds.

## 4. Durable pull-consumer lifecycle

- [x] 4.1 Create or bind the shared durable pull consumer and start its consume
  loop during startup. Normal restarts resume the durable cursor; if durable
  state is lost, recreate at the current stream tail without replaying or
  purging ambiguous retained history.
- [x] 4.2 Dispatch by subject to `genericUpdateAccessHandler` or
  `genericDeleteAccessHandler` with the enforced 90-second context.
- [x] 4.3 On success, call `Ack()` and increment `sync_ack`.
- [x] 4.4 On transient failure, leave the message unacknowledged; do not call
  `Nak()` or `NakWithDelay()`. Increment `sync_transient_attempts`.
- [x] 4.5 On terminal failure, call `Term()` and increment `sync_terminal`.
- [x] 4.6 Increment `sync_ack` or `sync_terminal` only after the corresponding
  acknowledgement returns nil. On ACK/TERM error, log and trace the failure and
  leave the message unacknowledged without issuing a fallback NAK.
- [x] 4.7 Ensure every replica binds the same durable consumer so its
  consumer-wide `MaxAckPending: 1` limit applies globally.
- [x] 4.8 On shutdown call `ConsumeContext.Stop()`, then wait for
  `ConsumeContext.Closed()` within a bounded grace period before canceling the
  service context, so an in-flight delivery attempt can finish normally
  instead of being aborted; only force-cancel if the grace period elapses.
  Then drain NATS. Do not also call `Drain()` on the consume context.
- [x] 4.9 Keep durable-state-loss recovery automatic and non-blocking:
  `DeliverNewPolicy` skips retained pre-recreation history, starts processing
  new messages immediately, and does not add a manual gate, destructive purge,
  or dependency between consumer recovery and access-check/core handlers.
- [x] 4.10 Add a small consumer lifecycle manager for runtime
  `jetstream.ErrConsumerDeleted`: keep unrelated handlers running, coalesce
  repeated local recovery signals, retry shared durable creation/binding every
  two seconds with the same `DeliverNewPolicy` config, restart local
  consumption, and stop the recovery loop/current consume context during
  graceful shutdown.

## 5. Remove the core NATS path

- [x] 5.1 Remove `GenericUpdateAccessSubject` and
  `GenericDeleteAccessSubject` from `createQueueSubscriptions`
  (`main.go:387-396`).
- [x] 5.2 Confirm `member_put`, `member_remove`, `access_check.request`, and
  `read_tuples` remain on core `QueueSubscribe`.
- [x] 5.3 Add a focused subscription-wiring test proving neither migrated
  subject is present in the core subscription set and both are handled by the
  shared JetStream consumer.

## 6. NATS connection hardening

- [x] 6.1 Set `nats.MaxReconnects(-1)` in `main.go`.
- [x] 6.2 Remove the reconnect-exhaustion `os.Exit(1)` dependency while
  retaining intentional signal-driven graceful shutdown. Ensure every
  `ClosedHandler` path calls `gracefulCloseWG.Done()` exactly once.
- [ ] 6.3 Test disconnect/reconnect behavior and confirm the durable consumer
  resumes processing.

## 7. Exhaustion, recovery, tracing, and metrics

- [x] 7.1 Add expvar counters `sync_ack`, `sync_transient_attempts`,
  `sync_terminal`, and `sync_max_deliver_exhausted`.
- [x] 7.2 Document and test that `sync_transient_attempts` counts each failed
  attempt while `sync_max_deliver_exhausted` increments once per advisory.
- [x] 7.3 Subscribe to JetStream max-delivery advisories and use them as the
  authoritative exhaustion signal. Log app-issued TERM locally without adding
  a termination-advisory subscription.
- [x] 7.4 For max-delivery advisories, fetch the retained message by stream
  sequence with `Stream.GetMsg` and decode its safe object context. If lookup or
  decoding fails, still increment exhaustion and log the advisory identity,
  sequence, delivery count, and enrichment error.
- [x] 7.5 Document recovery as an owning-service re-read of current database
  state followed by a fresh update or deletion as appropriate; do not blindly
  replay the retained message.
- [x] 7.6 Preserve incoming trace extraction and the existing `nats.process`
  span; defer additional JetStream-specific span attributes.
- [ ] 7.7 Configure external platform monitoring for
  `$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES.fga-sync-events.fga-sync-access-mutation-consumer`
  so final expiry while no fga-sync replica is connected still alerts.

## 8. Helm stream provisioning

- [x] 8.1 Add `charts/lfx-v2-fga-sync/templates/nats-stream.yaml` using the
  platform Stream CRD pattern.
- [x] 8.2 Add values for stream `fga-sync-events`, subjects
  `lfx.fga-sync.update_access` and `lfx.fga-sync.delete_access`, limits
  retention, `maxAge: 24h`, `keep: true`, file storage, and three replicas.
- [x] 8.3 Verify `helm template` and `helm lint` render the stream without
  breaking the existing KV-bucket template.

## 9. Fenced cutover and rollback runbook

- [ ] 9.1 Define the one-time operational platform maintenance window across
  every publisher of either migrated subject, including the platform controls
  used to pause mutation ingress and asynchronous producers. Do not add
  publisher maintenance-mode code as part of this change.
- [ ] 9.2 Provision the stream without an active consumer while core subscribers
  continue processing.
- [ ] 9.3 Enable the operational maintenance window and verify publication of
  both subjects has stopped.
- [ ] 9.4 Drain and stop both old core subscribers.
- [ ] 9.5 Purge the core-processed duplicate backlog and verify the stream is
  empty.
- [ ] 9.6 Deploy the JetStream-enabled binary after old core subscribers stop,
  create the `DeliverNewPolicy` durable consumer against the empty stream,
  verify neither core subscription remains, then release the maintenance
  window.
- [x] 9.7 Document the preferred rollback: keep publication paused, drain and
  ACK the stream backlog in sequence order until empty, stop the consumer,
  restore both core subscribers without overlap, then release the window.
- [x] 9.8 Document the fallback when the consumer cannot safely drain: record
  every affected object from pending stream sequences, restore core
  subscriptions without overlap, trigger fresh authoritative-state update or
  deletion publication, verify convergence, and purge superseded retained
  messages before release.
- [ ] 9.9 Explicitly forbid releasing the window with undispositioned durable
  messages or blindly bulk-replaying retained payloads. Test both rollback
  branches and the later purge-and-empty-stream forward cutover.

## 10. Tests

- [x] 10.1 Add JetStream message, metadata, ACK, TERM, and max-delivery advisory
  mocks.
- [x] 10.2 Unit test success ACK and cache-invalidation-warning ACK.
- [x] 10.3 Unit test connection refused, context deadline, unknown SDK error,
  propagated OpenFGA validation error, and each listed operational status remain
  unacknowledged. Confirm a persistent propagated validation error occupies the
  single global slot until bounded exhaustion.
- [x] 10.4 Unit test malformed JSON, missing fields, wrong operation, and local
  invalid-reference format call TERM and do not write tuples.
- [x] 10.5 Unit test the adapter hides the JetStream ACK reply subject.
- [x] 10.6 Unit test ACK/TERM failures do not increment success counters, issue
  NAK, or prevent redelivery.
- [x] 10.7 Unit test the 90-second context is enforced before the first
  two-minute BackOff timeout.
- [x] 10.8 Unit test consumer configuration, max-delivery advisory enrichment,
  graceful shutdown, subscription wiring, and `DeliverNewPolicy`.
- [ ] 10.9 Add external platform integration verification with real JetStream:
  stop OpenFGA, publish an update followed by deletion for the same object,
  confirm deletion remains pending across replicas, restore OpenFGA, and verify
  the update then deletion apply before retry exhaustion with no
  publisher-managed tuples remaining and `team:*` grants unchanged.
- [ ] 10.10 In the external platform environment, publish both migrated subjects
  while all fga-sync instances are offline, start the service, and verify ordered
  delivery. Run platform verification before rollout approval and outside the
  live maintenance window.
- [x] 10.11 Add regression tests proving subjects and `GenericFGAMessage`
  envelope shapes remain unchanged.
- [ ] 10.12 Verify external monitoring captures a final max-delivery advisory
  emitted after the last fga-sync replica disconnects.
- [x] 10.13 Regression test that a failed OpenFGA write does not seed a positive
  relationship cache entry, while a successful write still does.
- [x] 10.14 Pin `DeliverNewPolicy` in the unit-tested consumer configuration and
  document the durable-state-loss boundary: ordinary offline periods resume the
  existing cursor; a missing durable starts at the current stream tail, does
  not purge retained history, and requires no manual support to process new
  messages. Document that persistent OpenFGA failure can exhaust messages
  sequentially and eventually exceed the 24-hour retention limit.
- [ ] 10.15 In the external platform integration environment, delete a
  disposable test durable after it has processed retained history, recreate it
  while service replicas remain running, verify old retained messages are not
  redelivered, verify a newly published message is delivered without restarting
  the service, and confirm access-check/core handlers remain available. Do not
  run this destructive consumer-state-loss drill in production.
- [x] 10.16 Unit test the lifecycle manager coalesces duplicate
  `ErrConsumerDeleted` notifications, retries creation/binding without a hot
  loop, replaces the closed consume context, propagates unrelated terminal
  errors to logging without recreation, and stops recovery cleanly on shutdown.

## 11. Documentation and quality gates

- [x] 11.1 Update `docs/fga-sync-contract.md` for async-only update/delete
  access, ordered at-least-once delivery, typed outcomes, TERM, recovery, and
  the existing preservation of externally managed `team:*` grants.
- [x] 11.2 Update `docs/client-guide.md` to remove update/delete request/reply
  examples; document asynchronous at-least-once delivery,
  ACK/unacknowledged-redelivery/TERM outcomes, and clarify that `X-Sync` does
  not wait for OpenFGA.
- [x] 11.3 Update `CLAUDE.md`/`README.md` if reconnect or operational guidance
  changes.
- [x] 11.4 Document that `/readyz` checks NATS connection/draining state but not
  JetStream consume-loop health; keep consumer-aware readiness redesign out of
  Phase 1.
- [x] 11.5 Run `make check`, `make test`, Helm lint/template validation, and a
  local OpenFGA-outage verification. `make check` and `make test` pass
  locally; Helm lint/template validated in section 8; the OpenFGA-outage
  scenario was verified end-to-end against a fully local nats-server +
  OpenFGA (see `evidence.md`). The platform-environment run against real dev
  infrastructure (10.9) is still pending.
