---
name: fga-sync-dev
description: Path-scoped Go coding conventions for the lfx-v2-fga-sync repository. Owns repo-local truth for the single-package main layout, slog logging style, NATS QueueSubscribe + JetStream KV cache patterns, the generic resource-agnostic handler shape, table-driven tests against the INatsMsg/FgaService mocks, license headers, and Makefile-driven lint/format/build. Auto-attaches on Go files, go.mod/go.sum, Makefile, charts, scripts, and pkg. Defers cross-repo platform topology to lfx-skills:lfx-platform-architecture, the FGA tuple-emission contract to docs/fga-sync-contract.md, and the publisher-side workflow to the peer fga-tuple-emission skill.
paths:
  - "**/*.go"
  - "go.mod"
  - "go.sum"
  - "Makefile"
  - "pkg/**"
  - "charts/**"
  - "scripts/**"
  - ".claude/skills/fga-sync-dev/**"
allowed-tools: Read, Glob, Grep, Edit, Write, Bash
---

# Development Conventions (lfx-v2-fga-sync)

Repo-local Go conventions for fga-sync. This service is a single Go `package main` at the repo root, not the multi-module Goa layout most V2 repos use. Conventions encode that shape plus the fga-sync-specific NATS, cache, and handler patterns.

## Boundaries

- Platform topology, V2 service classification, write/read/access-check flows across repos, and OpenFGA model coordination order live in `lfx-skills:lfx-platform-architecture`. Read it before tracing cross-repo work.
- The `GenericFGAMessage` envelope, generic subjects, tuple format, cache invalidation protocol, model boundaries, and the two-step access-debug tree live in `docs/fga-sync-contract.md`. That doc is the contract other repos read. Update it in the same PR as any envelope or subject change.
- The peer `fga-tuple-emission` skill (do not edit from here) owns the publisher-side workflow for resource services that emit tuples.
- This skill owns how fga-sync's own Go code is structured and how its handlers, cache, and subscriptions are wired.

## Repo layout

| Area | Where |
| --- | --- |
| Entry point, NATS wiring, HTTP probes, graceful shutdown | `main.go` |
| Shared `HandlerService`, `INatsMsg`, response stubs | `handler.go` |
| Per-subject handlers | `handler_access.go`, `handler_read_tuples.go`, `handler_generic.go` |
| `FgaService` and OpenFGA tuple I/O | `fga.go`, `fga_client.go` |
| Test mocks for NATS, OpenFGA, KV | `mock.go` |
| Constants (relations, object types, subjects, queue, KV bucket) | `pkg/constants/` |
| Typed payloads | `pkg/types/` |
| Shared helpers | `pkg/utils/` |
| Helm chart | `charts/lfx-v2-fga-sync/` |
| Domain and contract docs | `docs/` |

There is no `cmd/`, `internal/`, `api/`, or `gen/` tree. Do not introduce a Goa design here; this service is NATS-only on the inbound side and OpenFGA-SDK on the outbound side.

## Go conventions

### Package and imports

- Everything implementation-level is `package main`. Add new files to the root unless they are pure helpers, constants, or typed payloads that belong under `pkg/`.
- Group imports in three blocks: standard library, third-party, then `github.com/linuxfoundation/lfx-v2-fga-sync/...`. Run `make fmt` to enforce.
- Match the existing constructor style on `HandlerService` and `FgaService`. Both are passed by value or by small struct; do not introduce pointer receivers without a reason.

### Logging

- Use the package-level `logger` (a `*slog.Logger` wrapped by `slog-otel`). Do not call `fmt.Println`, `fmt.Printf`, `log.Print*`, or `log.Println` for runtime logging.
- Use `*Context` variants (`InfoContext`, `WarnContext`, `DebugContext`, `ErrorContext`) so trace and span IDs flow through. Pass the same `ctx` you handed to `FgaService` calls.
- Use the package-level `errKey = "error"` constant for error fields; do not invent new keys for the same concept.
- Include stable structured fields when available: `subject`, `queue`, `object_type`, `object_id`, `relation`, `principal`, `operation`, `count`.
- Never log raw payloads, bearer tokens, or anything that may contain PII. Logging `string(message.Data())` at info level is acceptable for the existing access-check and read-tuples flows because those payloads are tuple-shaped, not user content; do not extend that pattern to new payload types without checking.
- Honor `DEBUG` and the `-d` flag for debug logging; both already wire into `logOptions.Level`.

### Error handling

- Return errors from handlers; `subscribeToSubject` logs them with `subject` and `queue` context. Do not log-and-swallow inside handlers unless you also send the subject's documented failure reply.
- Reply contract by subject (see also `references/nats-messaging.md`):
  - `lfx.access_check.request`: tab-separated `{object}#{relation}@user:{principal}\t{true|false}` per line. Absent line means denied.
  - `lfx.access_check.read_tuples`: JSON `{"results": [...]}` on success, `{"error": "..."}` on failure.
  - `lfx.fga-sync.*`: `OK` on success only when `message.Reply() != ""`; failures are returned to the subscription loop for logging and do not have a standardized NATS error body.
- Wrap upstream OpenFGA or NATS errors with `fmt.Errorf("...: %w", err)` so `errors.Is`/`errors.Unwrap` keep working.
- Graceful degradation on cache miss is intentional: fall through to a direct OpenFGA query rather than failing the request.
- The service must keep running on individual message failures. Never call `os.Exit` from a handler.

### Handlers (generic, resource-agnostic)

- All sync handlers (`genericUpdateAccessHandler`, `genericDeleteAccessHandler`, `genericMemberPutHandler`, `genericMemberRemoveHandler`) operate on `GenericFGAMessage`. They must stay object-type-agnostic. New resource types must NOT require a new handler.
- If a new resource type seems to need handler changes, the envelope is drifting. Push the change into `GenericFGAMessage` fields or into the model in `lfx-v2-helm`, not into fga-sync.
- `FgaService` should stay generic: tuple read, write, delete, sync. Domain knowledge about projects, meetings, committees, etc. stays out of `FgaService`.
- `buildObjectID(objectType, uid)` in `handler.go` is the single helper for `"type:uid"` formatting. Do not concat inline.
- Use `pkg/constants` for relation names, object-type prefixes, subjects, and the queue group. Do not hardcode subject strings, queue strings, or relation literals at call sites.

### NATS subscriptions

- All subscriptions are added to the slice in `createQueueSubscriptions` in `main.go`. Adding a new subject means adding a `subscriptionConfig` entry and a `HandlerFunc`; the helper handles error logging.
- Queue group for every subscription is `constants.FgaSyncQueue` (value `"lfx.fga-sync.queue"`) so only one replica handles each message when scaled.
- `INatsMsg` is the interface used in handler signatures; tests use the `mock.go` fake. Do not take `*nats.Msg` directly in handler signatures, or unit tests will not be able to drive them.
- Drain the NATS connection during graceful shutdown via the existing path in `main.go`. `gracefulShutdownSeconds` (25s) must stay higher than the NATS client request timeout and lower than the pod's `terminationGracePeriodSeconds`.

### JetStream KV cache

- Cache bucket name comes from `CACHE_BUCKET` (default `fga-sync-cache`). Do not hardcode the bucket name; use `constants.KVBucketNameSyncCache` or the resolved `cacheBucketName`.
- Cache keys are `rel.{base32-encoded-relation}`. Values are raw text booleans (`true` or `false`); freshness comes from the NATS KV entry timestamp. Do not change either shape without coordinating with consumers and updating `docs/fga-sync-contract.md`.
- Invalidation is a single `inv` timestamp key. Every successful OpenFGA write must bump it (`invalidateCache`); any cached entry older than `inv` is treated as stale.
- Stale hits are counted separately at `/debug/vars` (`cache_stale_hits`); do not collapse them into `cache_hits`.
- Local development against an externally written OpenFGA store should run with `USE_CACHE=false` to avoid serving stale results.

### Tests

- Co-locate `*_test.go` with the file under test (`handler_access_test.go` next to `handler_access.go`, etc.).
- Use the `INatsMsg` interface and the mocks in `mock.go` to drive handlers; do not stand up a real NATS connection in unit tests.
- Depend on the `FgaService` surface; do not call the OpenFGA SDK directly from handler tests.
- Use table-driven tests for branching behavior. One test function per exported (or top-level) handler, with cases added to the table.
- Run `make test` (or `make test-coverage` for an HTML report) before handing off. There is no in-repo integration suite; integration coverage lives in the platform stack via `lfx-v2-helm` and `load-mock-data`.

### Formatting, lint, license

- Run `make fmt` and `make lint` (golangci-lint) on every Go change. `make check` runs format, vet, and lint together; prefer it before handing off.
- Every new Go file starts with:

  ```go
  // Copyright The Linux Foundation and each contributor to LFX.
  // SPDX-License-Identifier: MIT
  ```

- Document exported Go symbols when the linter requires it; otherwise add comments only where the code is not self-explanatory.
- Update `docs/fga-sync-contract.md` in the same PR as any change to subjects, envelopes, reply shapes, cache keys, or invalidation behavior. That doc is read by other repos.

## Cross-repo coordination

OpenFGA model changes touch multiple repos and must ship in coordinated PRs. See the OpenFGA + Heimdall coordination section in `lfx-skills:lfx-platform-architecture` for the canonical order (model in `lfx-v2-helm`, emitted envelope in the owning service, Heimdall ruleset in the owning service's chart, local FGA contract docs, then `lfx-v2-argocd` rollout). fga-sync itself should not need code changes for a new resource type.

## References

- `references/nats-messaging.md`: fga-sync's exact subscriptions, queue group name, and per-subject reply shapes. Read when wiring a new subject or auditing reply behavior.
- `references/go-development-conventions.md`: longer-form Go conventions shared across V2 services (logging, errors, request context, pagination, tests, formatting). Read when onboarding to V2 Go style; the inline body above is the fga-sync-specific overlay.
