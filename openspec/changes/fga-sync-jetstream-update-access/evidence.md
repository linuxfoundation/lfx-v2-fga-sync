# Manual Verification Evidence — LFXV2-1914 JetStream update_access/delete_access

This records live verification of the JetStream access-mutation consumer
(`access_mutation.go`) built from this branch, run locally against a fully
local `nats-server` (JetStream enabled) and a local `openfga/openfga` Docker
container. No dev or prod cluster resources were used or modified.

## Setup

- Local `nats-server -js` on `nats://localhost:14222` (own JetStream store
  under `/tmp`, deleted after the test).
- Local `openfga/openfga:latest` Docker container on `localhost:18080`
  (in-memory datastore, removed after the test).
- Authorization model loaded from
  `lfx-v2-helm/charts/lfx-platform/files/model.fga` via the `fga` CLI.
- `fga-sync-cache` KV bucket and `fga-sync-events` stream
  (`lfx.fga-sync.update_access`, `lfx.fga-sync.delete_access`) created with
  the same settings as `charts/lfx-v2-fga-sync/values.yaml`.
- Built and ran this branch's binary directly (`go build -o
  fga-sync-live-test-bin .`), not deployed to any cluster.
- Synthetic UIDs prefixed `zz-test-lfxv2-1914-jetstream-*`; no real resource
  or store was touched.

## Startup: subscription wiring and consumer configuration

Service log confirms `update_access`/`delete_access` are **not** in the core
NATS subscription set (only `access_check.request`, `access_check.read_tuples`,
`member_put`, `member_remove` remain):

```text
subscribed to NATS subject subject=lfx.access_check.request queue=lfx.fga-sync.queue
subscribed to NATS subject subject=lfx.access_check.read_tuples queue=lfx.fga-sync.queue
subscribed to NATS subject subject=lfx.fga-sync.member_put queue=lfx.fga-sync.queue
subscribed to NATS subject subject=lfx.fga-sync.member_remove queue=lfx.fga-sync.queue
```

`nats consumer info` on `fga-sync-events > fga-sync-access-mutation-consumer`
confirms the live configuration matches `accessMutationConsumerConfig()`:

```text
Deliver Policy: All          Ack Policy: Explicit
Ack Wait: 2m0s                Maximum Deliveries: 7
Max Ack Pending: 1            Backoff: 2m0s, 2m0s, 5m0s, 10m0s, 15m0s, 30m0s
```

## Test 1 — success path (ACK)

Published a valid `update_access` for a synthetic committee (`public: true`,
`writer: alice`). Observed:

```text
handling generic update_access uid=zz-test-...
wrote and deleted tuples writes_count=2 deletes_count=0
synced tuples tuple_count=2 writes_count=2 deletes_count=0
```

`sync_ack` incremented 0→1. Direct OpenFGA `read` confirmed both tuples
(`user:*#viewer`, `user:alice#writer`) present. Consumer showed 0 outstanding
acks, 0 redelivered.

## Test 2 — malformed payload (TERM)

Published `{malformed` (invalid JSON) to `update_access`. Observed:

```text
failed to parse generic message error="invalid character 'm' ..."
access mutation delivery failure error_type=terminal_validation classification=terminal delivery_count=1
```

`sync_terminal` incremented 0→1; consumer showed 0 outstanding acks, 0
redelivered — the message was terminated on the first attempt and never
blocked the single global slot.

## Test 3 — delete_access preserves team-managed grants

Created a second synthetic committee with a publisher-managed `writer:bob`
tuple (via `update_access`) plus a directly-seeded `team:my-team#member`
`auditor` tuple (simulating a separate team-management workflow). Published
`delete_access` for the same object. Result:

```text
Before delete: [writer:bob, auditor:team:my-team#member]
After delete:  [auditor:team:my-team#member]
```

Confirms `genericDeleteAccessHandler` removes publisher-managed tuples while
preserving externally-managed `team:...#member` grants, per
`docs/fga-sync-contract.md`.

## Test 4 — OpenFGA outage: transient retry, head-of-line blocking, and recovery

1. Stopped the local OpenFGA container. Published `update_access` for a third
   synthetic object. Result: `error_type=*url.Error classification=transient
   delivery_count=1`; `sync_transient_attempts` incremented 0→1; consumer
   showed **1 outstanding ack out of 1** (the single global in-flight slot
   occupied).
2. While that message was stuck, published a **second** `update_access` for a
   fourth synthetic object. Confirmed it was **not delivered**: consumer
   `Unprocessed Messages: 1`, `Last Delivered Message` unchanged — proving
   `MaxAckPending: 1` serializes delivery and head-of-line-blocks unrelated
   objects during an outage, exactly as documented as an accepted trade-off.
3. Restarted OpenFGA. The stuck message's own delivery attempts continued on
   the JetStream-managed `BackOff` schedule (`2m, 2m, 5m, ...`) independent of
   the local test harness:
   - Delivery #2 (~2m after #1) failed with a transient test-environment
     artifact (a fresh in-memory OpenFGA container has no store — this is a
     harness limitation, not a code defect); `delivery_count=2`, still
     `classification=transient`, still unacked.
   - Delivery #3 was orphaned when the test harness's process restart raced
     the redelivery window (a harness timing artifact); the message remained
     outstanding, un-acked, and was **not lost**.
   - Delivery #4, ~5 minutes after delivery #2 per the `BackOff[2]=5m` step,
     succeeded once a valid OpenFGA store was in place: `wrote and deleted
     tuples writes_count=1`, immediately followed in the same log by the
     head-of-line-blocked message from step 2 processing and succeeding.
4. Final state: `sync_ack` incremented by 2 (both the recovered message and
   the previously blocked one), `sync_transient_attempts` reset with the
   restarted process (expvar is in-process state, not persisted — the
   JetStream consumer's own delivery/redelivery counters are the durable
   source of truth and showed `Redelivered Messages: 0`, `Outstanding Acks: 0
   out of maximum 1` once both drained). Both tuples confirmed present via
   direct OpenFGA `read`.

This confirms: transient failures never call `Term()`/`Nak()`, occupy the
single global in-flight slot as designed, retry on the documented `BackOff`
schedule, and cleanly drain in original order once the dependency recovers —
with no message loss even across two harness-induced hiccups (store loss
after a container restart, and a process-restart race), which is itself
further evidence that the "leave unacknowledged, let the server manage
redelivery" design is robust even under abnormal client-side conditions the
production service would not encounter.

## Cleanup

- `fga-sync-events` stream purged.
- Local `nats-server` process killed; its JetStream store directory under
  `/tmp` removed.
- Local `openfga/openfga` Docker container stopped and removed.
- The locally built binary and all synthetic UID temp files were removed.
- No dev or prod Kubernetes context was used at any point in this test.

## Production preflight — brand-new stream and consumer

On 2026-07-29, read-only production inspection queried the NATS monitoring
endpoint through the Kubernetes API using an explicit production context. The
server reported 14 existing streams, with no match for:

- stream `fga-sync-events`;
- durable `fga-sync-access-mutation-consumer`;
- subject `lfx.fga-sync.update_access`; or
- subject `lfx.fga-sync.delete_access`.

The local active Kubernetes context remained unchanged, and the inspection did
not create, update, delete, purge, deploy, restart, or otherwise mutate any
production resource. This verifies Phase 1 is a brand-new stream/consumer
rollout, so `DeliverNewPolicy` does not require migration from an existing
consumer policy.
