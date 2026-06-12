---
name: fga-tuple-emission
description: >
  Use when a resource service emits FGA tuples after a CRUD, when building or
  changing a `GenericFGAMessage` envelope, when debugging access checks ("user
  can't see X they should"), when editing fga-sync handlers, or when touching
  OpenFGA model boundaries or cache invalidation. This is the authoritative FGA
  contract skill: it covers both the publisher (per-service emission) side and
  the fga-sync handler side. Distinct from the `lfx-v2-fga-sync` repo as a
  deployment surface; this skill is about the tuple-emission and handler
  contract.
allowed-tools: Read, Glob, Grep, Edit, Write
paths:
  - 'handler*.go'
  - 'fga*.go'
  - 'pkg/**'
  - 'docs/fga*.md'
  - 'docs/client-guide.md'
---

# FGA Access Control

The platform uses ReBAC via OpenFGA. fga-sync is the single owner of tuple writes, cache, and access-check semantics. Resource services publish generic envelopes; this skill governs both sides of that contract.

## Workflow

1. **Read the authoritative contract** at `references/fga-patterns.md` (link
   to `docs/fga-sync-contract.md`). Confirm the change does not
   alter the `GenericFGAMessage` envelope, a generic subject, or the
   cache-invalidation protocol.
2. **Identify the change surface**:
   - **Resource service publisher**: usually
     `internal/infrastructure/nats/messaging_publish.go` or equivalent.
   - **fga-sync handler**: the root-level `handler_*.go` files (single `package main`; no `handlers/` subdir).
   - **OpenFGA model**: `lfx-v2-helm/charts/lfx-platform/templates/openfga/model.yaml`.
   - **Heimdall ruleset**: the owning service's chart `templates/ruleset.yaml`.
3. **For publisher changes**: ensure the envelope has correct `object_type`,
   `operation` matching the subject suffix, full-sync `relations` (any key not
   listed is removed), and `references` keyed by parent type. Bare UID values
   are fine; the handler prepends the type prefix.
4. **For member changes**: use `member_put` / `member_remove`. An empty
   `relations` on `member_remove` removes ALL relations for that user, verify
   that's what you want. Missing `username` is rejected by fga-sync; for
   non-LFID members, document the limitation in the service's
   `docs/fga-contract.md`.
5. **For model changes** in `lfx-v2-helm`: see the Heimdall coordination
   section in the central `/lfx-skills:lfx-platform-architecture` skill for the full
   ordering across `model.yaml`, the per-service `ruleset.yaml`, and the
   emitted access envelope. Ship them in coordinated PRs.
6. **For debugging access** ("user can't see X"): walk the two-step debug
   tree in `references/fga-patterns.md`, first OpenSearch (indexing or empty
   `access_check_*` fields), then OpenFGA tuples (missing or wrong references,
   cache staleness).
7. **Update the per-service contract doc** at `docs/fga-contract.md` in the
   owning service in the same PR as any publisher change.

## Anti-patterns

- Adding type-specific handlers in fga-sync. The handlers are generic; new
  resource types do not require fga-sync code changes.
- Calling OpenFGA directly from a resource service. Always go through
  fga-sync via `lfx.fga-sync.*` subjects.
- Assuming `lfx.access_check.request` results come back in order. Match on
  the request token.
- Forgetting the Heimdall ruleset update when adding a new FGA type.

## References

- `references/fga-patterns.md`: the authoritative access-control contract.
