<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# FGA Sync JetStream Cutover and Rollback

This runbook migrates `lfx.fga-sync.update_access` and
`lfx.fga-sync.delete_access` together. Do not move only one subject: a delayed
older update could otherwise recreate publisher-managed authorization after a
newer deletion.

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
   `DeliverAllPolicy`; because the stream is empty, no pre-cutover message is
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
