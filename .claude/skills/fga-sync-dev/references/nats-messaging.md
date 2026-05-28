<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# NATS Messaging (fga-sync consumer)

Read `lfx-skills:lfx-platform-architecture` for cross-repo NATS and KV ownership and the access-check flow. Read `docs/fga-sync-contract.md` for the `GenericFGAMessage` envelope, tuple format, and cache-invalidation contract. This file lists only what fga-sync subscribes to and how it replies.

## Subscriptions

All subscriptions are wired in `createQueueSubscriptions` in `main.go` and share the queue group `constants.FgaSyncQueue` (`"lfx.fga-sync.queue"`). Only one replica handles each message when scaled horizontally.

| Subject (constant) | Value | Handler | Purpose |
| --- | --- | --- | --- |
| `AccessCheckSubject` | `lfx.access_check.request` | `accessCheckHandler` | Batch access check (used by query-service) |
| `ReadTuplesSubject` | `lfx.access_check.read_tuples` | `readTuplesHandler` | Read direct tuples for a user + object type |
| `GenericUpdateAccessSubject` | `lfx.fga-sync.update_access` | `genericUpdateAccessHandler` | Full sync of all relations for a resource |
| `GenericDeleteAccessSubject` | `lfx.fga-sync.delete_access` | `genericDeleteAccessHandler` | Remove all relations on resource delete |
| `GenericMemberPutSubject` | `lfx.fga-sync.member_put` | `genericMemberPutHandler` | Add or update a per-user relation |
| `GenericMemberRemoveSubject` | `lfx.fga-sync.member_remove` | `genericMemberRemoveHandler` | Remove a per-user relation |

Subject strings live in `pkg/constants/nats.go`. Do not hardcode them at call sites.

## Reply semantics

- `lfx.access_check.request`: plain text, one line per requested check, tab-delimited `{object}#{relation}@user:{principal}\t{true|false}`. Missing lines mean denied. Replies are not ordered; callers must match by request token.
- `lfx.access_check.read_tuples`: JSON. Success is `{"results": ["object#relation@user:{principal}", ...]}`. Failure is `{"error": "..."}`.
- `lfx.fga-sync.*`: `OK` on success only when `message.Reply() != ""`. Failures are returned to `subscribeToSubject` for logging with `subject` and `queue`; there is no standardized NATS error body for sync-subject failures.

## When adding a new subscription

1. Add the subject string to `pkg/constants/nats.go` with a doc comment that includes the wire value.
2. Add a `HandlerFunc` to one of the `handler_*.go` files; sign it with `INatsMsg`, not `*nats.Msg`, so tests can drive it from `mock.go`.
3. Append a `subscriptionConfig` entry to the slice in `createQueueSubscriptions` in `main.go`.
4. Add a row above and reflect the reply shape in `docs/fga-sync-contract.md` if the subject is part of the cross-repo contract.
5. Add table-driven tests in `handler_*_test.go` using the existing mocks.
