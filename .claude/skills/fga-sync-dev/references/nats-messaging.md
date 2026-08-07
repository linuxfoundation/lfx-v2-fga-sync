<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# NATS Messaging (fga-sync consumer)

Read `lfx-skills:lfx-platform-architecture` for cross-repo NATS and KV ownership and the access-check flow. Read `docs/fga-sync-contract.md` for the `GenericFGAMessage` envelope, tuple format, and cache-invalidation contract. This file lists only what fga-sync subscribes to and how it replies.

## Subscriptions

Core subscriptions are wired in `createQueueSubscriptions` in `main.go` and
share `constants.FgaSyncQueue`. All four generic sync subjects
(`update_access`, `delete_access`, `member_put`, `member_remove`) share one
durable JetStream consumer with `MaxAckPending: 1`, so only one message is
pending globally.

| Subject (constant) | Value | Handler | Purpose |
| --- | --- | --- | --- |
| `AccessCheckSubject` | `lfx.access_check.request` | `accessCheckHandler` | Batch access check (used by query-service) |
| `ReadTuplesSubject` | `lfx.access_check.read_tuples` | `readTuplesHandler` | Read direct tuples for a user + object type |
| `GenericUpdateAccessSubject` | `lfx.fga-sync.update_access` | `genericUpdateAccessHandler` | JetStream full sync of publisher-managed relations |
| `GenericDeleteAccessSubject` | `lfx.fga-sync.delete_access` | `genericDeleteAccessHandler` | JetStream removal of publisher-managed relations; preserves `team:*` grants |
| `GenericMemberPutSubject` | `lfx.fga-sync.member_put` | `genericMemberPutHandler` | JetStream add or update of a per-user relation |
| `GenericMemberRemoveSubject` | `lfx.fga-sync.member_remove` | `genericMemberRemoveHandler` | JetStream removal of a per-user relation |

Subject strings live in `pkg/constants/nats.go`. Do not hardcode them at call sites.

## Reply semantics

- `lfx.access_check.request`: plain text, one line per requested check, tab-delimited `{object}#{relation}@user:{principal}\t{true|false}`. Missing lines mean denied. Replies are not ordered; callers must match by request token.
- `lfx.access_check.read_tuples`: JSON. Success is `{"results": ["object#relation@user:{principal}", ...]}`. Failure is `{"error": "..."}`.
- None of the four generic sync subjects (`update_access`, `delete_access`,
  `member_put`, `member_remove`) send an application reply. The shared
  consumer ACKs success, leaves transient failures unacknowledged, and
  terminates proven local validation failures; JetStream ACK/terminate/
  redeliver is the only delivery signal.

## When adding a new subscription

1. Add the subject string to `pkg/constants/nats.go` with a doc comment that includes the wire value.
2. Add a `HandlerFunc` to one of the `handler_*.go` files; sign it with `INatsMsg`, not `*nats.Msg`, so tests can drive it from `mock.go`.
3. Append a `subscriptionConfig` entry to `queueSubscriptionConfigs` for core
   NATS, or add it to the JetStream consumer only when ordered durable delivery
   is part of the contract.
4. Add a row above and reflect the reply shape in `docs/fga-sync-contract.md` if the subject is part of the cross-repo contract.
5. Add table-driven tests in `handler_*_test.go` using the existing mocks.
