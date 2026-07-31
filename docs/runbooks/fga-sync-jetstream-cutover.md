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
the same stream and shared durable consumer as Phase 1. Unlike Phase 1, the
stream and consumer already exist and are live; there is no fresh-stream
purge and no "consumer does not yet exist" precondition. Do not reuse the
Phase 1 cutover steps above for this phase.

### Deployment shape: one combined release, not three

The fga-sync-jetstream-membership change ships the shared-consumer code, the
Helm stream-widening (adding `member_put`/`member_remove` to the stream's
`subjects`), and the core-subscription removal in the same binary and chart
version — they deploy together as a single `helm upgrade`, not as three
separately-timed releases. This means the moment the new version is live: the
stream is already widened and the old core subscriptions are already gone,
with no observable window in between where only one of the two has happened.

This trades a design goal (verify stream-widening is capturing traffic before
removing the fallback) for simplicity: there is no core-NATS fallback the
instant this deploys, and no scripted gate confirms the stream capture
succeeded before that happens. That risk is accepted deliberately — see
"Accepted risk" below — rather than mitigated by splitting the rollout.

### Preconditions

Before deploying this change to an environment, confirm both:

1. All four owning publisher services — committee-service, meeting-service,
   mailing-list-service, and member-service — are asynchronous-only
   (`Publish`, never `Request`/`Reply`) for `member_put` and `member_remove`,
   and that deployment is live in that environment. Project-service only
   issues `update_access`/`delete_access` and is not affected. This must be
   verified per-service: the moment membership subjects join the stream, the
   stream acknowledges the publish and a surviving `nc.Request` caller reads
   that JetStream storage ack as completion, even though fga-sync has not
   actually processed the message yet.
2. Phase 1 (`update_access`/`delete_access` on JetStream) has been live and
   stable in that environment for a soak period, since Phase 2 shares the
   same stream and durable consumer and a Phase 1 regression would now also
   affect membership traffic.

### Accepted risk: no verification pause before the core fallback disappears

Because stream-widening and core-subscription-removal land in the same
deploy, if the widened stream silently fails to capture membership traffic
(a Helm value not applied, a replica still on old config, a consumer
`FilterSubjects` mismatch), there is no fallback path left to catch those
messages — core no longer has a subscription for them. Immediately after
deploying, manually check that `sync_ack` is increasing and
`sync_max_deliver_exhausted` is not (see "Post-cutover checks" below), the
same way Phase 1's cutover is verified above. Do not treat the deploy as
successful until that check has been done; there is no automated gate doing
it for you.

### The no-op guarantee, for the rollback case

`on_duplicate: ignore` / `on_missing: ignore` (write-collision handling)
exists primarily for the rollback path, not for a normal deploy: a normal
deploy has no window where the same membership message is delivered via both
core and JetStream, because core disappears in the same step the stream
widens. The window this guards against arises if the core subscriptions are
*restored* later (see "Rollback" below), which recreates double delivery
against a stream that is still widened. During that restored-core window,
expect **each membership message applied twice — but no collision errors**,
because the no-op options make the second application a no-op rather than a
write error. Concretely:

- A duplicate `member_put` re-adds the same relation tuple; OpenFGA reports
  success on the redundant write instead of a duplicate-write error.
- A duplicate `member_remove` re-deletes an already-removed relation tuple;
  OpenFGA reports success on the redundant delete instead of a missing-tuple
  error.

If a collision-driven error burst appears instead, the write-collision-ignore
options are not in effect — treat that as a signal to roll back (see
"Rollback" below) rather than riding out the window. Keep any such window
short and attended: membership arrives at roughly 2.25 messages per minute,
so an unattended window accumulates duplicate work quickly.

**What the no-op guarantee does and does not cover.** `on_duplicate: ignore` /
`on_missing: ignore` only make an *exact-match* redundant write or delete a
no-op — the same tuple applied twice ends up in the same state either way.
They do not impose any ordering between two *different* messages for the same
user/object. If core subscriptions are ever restored while the stream is
still widened (the rollback scenario), the core path is not serialized by the
JetStream consumer's `MaxAckPending: 1`, so an older `member_put` delivered
via core can finish after the JetStream consumer has already applied that
same put and a subsequent `member_remove` or `delete_access`. In that case
the tuple is absent when the late core `member_put` runs, so it is a
legitimate write rather than a duplicate, and it recreates access the newer
message removed. This residual reordering risk is not closed by this change
and is bounded only by keeping any restored-core window short and watching
for it, not eliminated. If a suspiciously recent grant reappears on an object
with intervening membership or deletion activity, treat it as this scenario
and have the owning service re-read current state and republish rather than
assuming it is legitimate.

### Post-cutover checks (Phase 2)

- `sync_ack` should increase and `sync_max_deliver_exhausted` should not,
  within minutes of the deploy (see "Accepted risk" above) — do this check
  manually right after deploying, the same way Phase 1's cutover is checked.
- `sync_terminal` rises to a new floor once membership's proven-invalid
  payloads (missing `username`/`uid`, malformed JSON, wrong operation, or an
  empty relation entry on `member_put` specifically — `member_remove` drops
  empty relation entries rather than terminating on them) start terminating
  through the shared consumer instead of being silently dropped by the old
  core handler. **This floor increase is expected** and traces to LFXV2-2907
  publisher-side payload gaps — do not read it as a fga-sync regression.
- A terminated `member_remove` leaves the tuple(s) in place — fga-sync does
  not retry or repair on the publisher's behalf. Attribution and repair of a
  terminated message belong to the owning publisher, not to a fga-sync
  operator action.

### Rollback (Phase 2)

Restoring the core subscriptions after this change is live recreates the
overlap window described above, including its residual reordering risk (see
"What the no-op guarantee does and does not cover" above) — this is
expected, not a new failure mode introduced by rollback. The unsafe action is
blindly replaying retained JetStream membership payloads after a rollback:
retained pre-rollback history can be stale relative to what the restored
core path has since processed, so never replay it wholesale. If specific
membership state is suspect, have the owning service re-read current
database state and republish.

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
