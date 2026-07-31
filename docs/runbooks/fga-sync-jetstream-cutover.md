<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# FGA Sync JetStream Cutover and Rollback

## Phase 1: access mutations (`update_access` / `delete_access`)

This section covers only Phase 1 — migrating `lfx.fga-sync.update_access` and
`lfx.fga-sync.delete_access` together. Do not move only one subject: a delayed
older update could otherwise recreate publisher-managed authorization after a
newer deletion. **The purge step and the "consumer does not yet exist"
precondition below are Phase 1-only** — they assume a clean cutover from a
brand-new stream with no existing durable consumer. Phase 2 (below) adds
`member_put`/`member_remove` to an already-live stream and consumer, so those
steps do not apply and must not be repeated.

## Preconditions

- LFXV2-2830 and LFXV2-2828 are deployed; project and committee publishers use
  asynchronous publish for both subjects.
- The external platform test has passed with OpenFGA stopped and restored.
- The `fga-sync-events` stream exists with both subjects, 24-hour retention,
  file storage, and three replicas.
- The `fga-sync-access-mutation-consumer` does not yet exist.
- Owners of all affected publisher services are present for the maintenance
  window and can pause publishing.
- NATS publish ACLs restrict each publisher credential to its approved subjects.
  Record the ACL verification and the accepted generic-subject trust boundary
  (a permitted publisher can name any modeled object type) in the change log.
- JetStream account storage quotas and alerts can absorb the expected 24-hour
  access-mutation volume. The stream intentionally has no unacknowledged
  `maxBytes` discard limit.
- External monitoring has been tested against the access-mutation consumer's
  max-delivery advisory subject, including an advisory emitted while all
  fga-sync replicas are disconnected.

## Cutover

1. Start the platform maintenance window. For every publisher, record the exact
   operational control used to pause both access-mutation subjects and apply it.
2. Confirm no publisher still uses NATS request/reply. Verify publication has
   stopped by confirming the stream's last sequence does not change across two
   consecutive observation intervals.
3. Drain and stop every old fga-sync pod so no core update/delete subscription
   remains.
4. Purge the core-processed duplicate backlog, then verify the stream is empty.
   Record the purge and resulting empty stream state in the change log.
5. Deploy the new fga-sync version. It creates the durable consumer with
   `DeliverNewPolicy`; because the stream is empty, no pre-cutover message is
   replayed.
6. Confirm one durable consumer is active and update/delete subjects are absent
   from the core subscription set.
7. Resume both subjects at all publishers and end the maintenance window.

Do not run the OpenFGA outage drill inside this live cutover window.

## Post-cutover checks

- Publish one update and one deletion for a disposable object.
- Confirm `sync_ack` increases and `sync_transient_attempts`,
  `sync_terminal`, and `sync_max_deliver_exhausted` do not increase
  unexpectedly.
- Confirm no publisher waits for an application `OK` reply.
- Confirm `/readyz` reports NATS connectivity. It does not prove the JetStream
  consumer loop is healthy; verify the durable consumer separately.

## Rollback

Keep publishing paused until every retained message has an explicit,
order-safe disposition.

Preferred:

1. Record the consumer's initial pending plus ACK-pending count, and the
   baseline values for `sync_ack`, `sync_terminal`, and
   `sync_max_deliver_exhausted`.
2. Leave the new consumer running and drain the stream backlog in sequence
   order.
3. Confirm the consumer reports no pending or ACK-pending messages, and the
   combined `sync_ack` and `sync_terminal` increase equals the recorded
   backlog count. Terminally disposed messages (locally invalid payloads)
   clear pending delivery without increasing `sync_ack`, so both counters must
   be checked. Confirm `sync_max_deliver_exhausted` did not increase. Route any
   exhausted sequence through the fallback procedure instead of treating it as
   drained.
4. Stop the consumer and all new fga-sync pods.
5. Deploy the previous fga-sync version and confirm both core subscriptions are
   active without overlap.
6. Resume publishers.

Fallback when the consumer cannot drain safely:

1. Record the affected object type and UID for every retained access mutation.
2. Stop the consumer and deploy the previous fga-sync version.
3. Have each owning service re-read current database state and publish a fresh
   update or deletion through the restored core path.
4. Verify convergence, purge the now-superseded retained messages, and confirm
   the stream is empty.
5. Resume normal publishing.

Never release the maintenance window with undispositioned durable messages.
Never blindly replay retained payloads: an old full-state update can overwrite a
newer update or recreate publisher-managed authorization after deletion.

## Phase 2: membership (`member_put` / `member_remove`)

Phase 2 moves `lfx.fga-sync.member_put` and `lfx.fga-sync.member_remove` onto
the same stream and shared durable consumer as Phase 1, in three ordered
releases. Unlike Phase 1, the stream and consumer already exist and are live;
there is no fresh-stream purge and no "consumer does not yet exist"
precondition. Do not reuse the Phase 1 cutover steps above for this phase.

### Preconditions (release 2 gate)

Before release 2 (stream widening), confirm all four owning publisher services
— project-service, committee-service, meeting-service, and member-service —
are asynchronous-only (`Publish`, never `Request`/`Reply`) for `member_put` and
`member_remove`. This must be verified per-service because widening the
stream, not removing the core subscription, is the actual point of no return
for a request/reply caller: the moment membership subjects join the stream,
the stream acknowledges the publish and a surviving `nc.Request` caller reads
that JetStream storage ack as completion, even though fga-sync has not
processed the message yet and core subscription removal (release 3) has not
happened. Also confirm the release-1 shared-consumer binary (with
`FilterSubjects` covering all four subjects and the write-collision-ignore
options) is fully rolled out and confirmed live first.

### The three ordered releases

1. **Release 1 — shared consumer code.** Deploy the fga-sync binary that
   dispatches `member_put`/`member_remove` through the shared JetStream
   consumer and passes `on_duplicate: ignore` / `on_missing: ignore` to
   OpenFGA writes. At this point membership subjects are still core NATS only
   (not yet in the stream), so this release is inert for membership traffic.
2. **Release 2 — stream widening.** Add `lfx.fga-sync.member_put` and
   `lfx.fga-sync.member_remove` to the stream's `subjects` list via the Helm
   chart. From this point until release 3, every membership message is
   delivered twice: once via the pre-existing core NATS subscription (still
   running) and once via the JetStream consumer.
3. **Release 3 — core subscription removal.** Remove the core NATS
   subscriptions for `member_put`/`member_remove` so JetStream is the only
   delivery path. Do not deploy this release before release 2 has been
   applied and verified: doing so briefly removes the *only* delivery path
   for membership messages (core), before the stream capture that would carry
   them, and those messages are lost permanently.

### The overlap window (between release 2 and release 3)

During the overlap window, expect **each membership message applied twice —
but no collision errors**, because `on_duplicate: ignore` / `on_missing:
ignore` (shipped in release 1 as part of write-collision handling) makes the
second application a no-op rather than a write error. Concretely:

- A duplicate `member_put` re-adds the same relation tuple; OpenFGA reports
  success on the redundant write instead of a duplicate-write error.
- A duplicate `member_remove` re-deletes an already-removed relation tuple;
  OpenFGA reports success on the redundant delete instead of a missing-tuple
  error.

If a collision-driven error burst appears instead, the write-collision-ignore
options from release 1 are not in effect — treat that as a signal to remove
the stream subjects (roll back release 2) rather than riding out the window.
Keep the window short and attended: membership arrives at roughly 2.25
messages per minute, so an unattended window accumulates duplicate work
quickly. Watch consumer ack lag for the whole window, not just at the end;
rising ack lag is the signal to proceed to release 3 immediately or roll back.

### Post-cutover checks (Phase 2)

- `sync_terminal` rises to a new floor once membership's proven-invalid
  payloads (missing `username`/`uid`, malformed JSON, wrong operation, empty
  relation entries) start terminating through the shared consumer instead of
  being silently dropped by the old core handler. **This floor increase is
  expected** and traces to LFXV2-2907 publisher-side payload gaps — do not
  read it as a fga-sync regression.
- After release 3, confirm double processing has stopped: each membership
  message is applied once, and `sync_terminal`'s membership contribution
  matches the LFXV2-2907 rate rather than twice it (the release-2 overlap
  rate).
- A terminated `member_remove` leaves the tuple(s) in place — fga-sync does
  not retry or repair on the publisher's behalf. Attribution and repair of a
  terminated message belong to the owning publisher, not to a fga-sync
  operator action.

### Rollback (Phase 2)

Restoring the core subscriptions after release 3 recreates the overlap
window described above (double delivery, no-op collisions) — this is
expected and safe on its own. The unsafe action is blindly replaying retained
JetStream membership payloads after a rollback: retained pre-rollback history
can be stale relative to what the restored core path has since processed, so
never replay it wholesale. If specific membership state is suspect, have the
owning service re-read current database state and republish.

## Consumer state loss (disaster recovery)

The stream retains messages for 24 hours regardless of ACK state, so if the
`fga-sync-access-mutation-consumer` durable's state is ever lost (deleted by
mistake, or otherwise absent) while the stream is non-empty, letting
`DeliverAllPolicy` auto-create a replacement would replay the full retained
backlog, including stale updates a later deletion already superseded.

fga-sync uses `DeliverNewPolicy`, so this state recovers automatically:

1. A running replica that receives `jetstream.ErrConsumerDeleted` keeps the
   process and unrelated handlers available while retrying durable
   creation/binding every two seconds. Startup follows the same creation path
   when the durable is already missing.
2. Retained pre-recreation messages are not replayed or purged; they age out
   under the existing 24-hour `maxAge`.
3. Messages published after recreation begin processing immediately. No pod,
   access-check handler, or core NATS subscription waits for manual consumer
   recovery.

This is an explicit availability-over-completeness boundary. Ordinary pod or
NATS outages do **not** trigger it: when the durable still exists, replicas bind
its stored cursor and resume pending messages. Only loss of the durable state
skips retained history. If monitoring detects such a loss and the skipped
window matters, owning services may re-read current database state and publish
fresh updates/deletions; never replay the retained snapshots blindly.

Do not add an application-side stream purge for this case. Multiple replicas
can observe a missing consumer concurrently, and a delayed purge from one
replica could delete a fresh message after another replica has already created
the replacement consumer. `DeliverNewPolicy` establishes the new start boundary
atomically during consumer creation without destructive stream operations.
