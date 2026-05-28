# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

> **Central LFX skills:**
> - Start with `/lfx-skills:lfx` for cross-repo tasks, "where does X live" questions, owner/peer repo routing, or missing checkouts.
> - Use `/lfx-skills:lfx-platform-architecture` for platform composition, V2 service classes, write/read/access-check flows, cross-service responsibilities, NATS/KV ownership, and the OpenFGA/Heimdall coordination order across `lfx-v2-helm`, owning services, and `lfx-v2-argocd`.
> - The repo-local path-scoped `fga-sync-dev` skill auto-attaches on Go paths (`**/*.go`, `go.mod`, `go.sum`, `Makefile`, `pkg/**`, `charts/**`, `scripts/**`) and owns repo-local Go style for this service: single `package main` layout, slog wrapper, `INatsMsg`/`FgaService` mocks, generic resource-agnostic handlers, JetStream KV cache pattern, NATS queue-group conventions, tests, formatting, and linting.
> - The repo-local `fga-tuple-emission` peer skill owns the publisher-side workflow (resource services emitting `GenericFGAMessage`) and the fga-sync handler side of that contract.
> - **This repo owns the FGA tuple-emission contract and the access-check contract.** Other V2 services route here for envelope shapes, generic subjects, tuple format, cache behavior, and `lfx.access_check.*` semantics. See `docs/fga-sync-contract.md` and `docs/fga-protected-types.md`.
> - Repo-owned docs under `docs/` are canonical for FGA Sync contracts, onboarding, and caller examples. If the plugin is missing, install with `/plugin marketplace add linuxfoundation/lfx-skills` then `/plugin install lfx-skills@lfx-skills`.

## Service Overview

FGA Sync is a high-performance Go microservice that synchronizes authorization data between NATS messaging and OpenFGA (Fine-Grained Authorization). It provides cached relationship checks and real-time access control updates for the LFX Platform v2.

## Architecture

### Core Components

- **NATS Message Handlers**: Process access checks, read-tuples queries, and resource permission sync (updates, deletes, member operations)
- **OpenFGA Client**: Manages authorization relationships and batch operations
- **JetStream Cache**: High-performance KeyValue store for relationship caching
- **Health Endpoints**: Kubernetes-ready liveness and readiness probes

## Agent Guidance

Repo-owned guidance is split:

- `docs/fga-sync-contract.md`: authoritative cross-repo contract. Generic FGA envelope, generic subjects, tuple format, cache behavior, model boundaries, and the two-step access-debug tree. Other repos read this.
- `docs/fga-protected-types.md`: index of FGA-protected object types by service and the operations each one publishes.
- `docs/client-guide.md`: NATS API reference for callers (request/reply queries, sync message formats, integration examples).
- `docs/fga-catalog.md`: onboarding entry point for service owners adding FGA sync to a new resource service.
- `.claude/skills/fga-sync-dev/`: repo-local Go conventions for fga-sync's own code (see Central LFX skills callout).
- `.claude/skills/fga-tuple-emission/`: publisher-side workflow shared between resource services and fga-sync handlers.

Read these before changing access sync behavior or advising resource services on emitted access messages.

## Consumed Cross-Repo Contracts

This repo depends on contracts owned elsewhere. Do not copy or infer them from
local examples. Read the owner file before changing model coordination,
indexing coordination, or publisher guidance.

- OpenFGA model:
  `lfx-v2-helm/charts/lfx-platform/templates/openfga/model.yaml`
- Generic indexer event contract:
  `lfx-v2-indexer-service/docs/indexer-contract.md`
- Per-resource FGA emission contracts:
  `<resource-service>/docs/fga-contract.md`

Use `/lfx-skills:lfx` if an owner repo is missing locally, the path has moved,
or the task needs additional peer repos.

### Message Flow

1. NATS messages arrive on subjects (e.g., `lfx.access_check.request`)
2. Queue groups ensure load balancing across service instances
3. Handlers process messages, interact with cache/OpenFGA, and send replies
4. Cache invalidation occurs on resource updates/deletions

### Key Dependencies

- `github.com/nats-io/nats.go` - NATS messaging client
- `github.com/openfga/go-sdk` - OpenFGA authorization client
- Standard library for HTTP server and JSON processing

## Common Development Commands

The full target list lives in the `Makefile` (run `make help`). The path-scoped
`fga-sync-dev` skill auto-attaches on Go paths and owns the canonical
build/test/lint workflow. Quick reference:

- `make build` / `make run` / `make dev` (build with debug symbols)
- `make test` / `make test-coverage`
- `make check` (runs `fmt`, `vet`, `lint`)
- `make docker-build`, `make helm-install-local`

## Configuration

### Required Environment Variables

- `NATS_URL`: NATS server connection URL (e.g., `nats://localhost:4222`)
- `OPENFGA_API_URL`: OpenFGA API endpoint (e.g., `http://localhost:8080`)
- `OPENFGA_STORE_ID`: OpenFGA store ID
- `OPENFGA_AUTH_MODEL_ID`: OpenFGA authorization model ID

### Optional Environment Variables

- `CACHE_BUCKET`: JetStream KeyValue bucket name (default: `fga-sync-cache`)
- `USE_CACHE`: Enable JetStream KV reads for access checks when set to `true`
  (chart default is true; binary default is false)
- `PORT`: HTTP server port (default: `8080`)
- `DEBUG`: Enable debug logging (default: `false`)

## Message Formats

Full message format details (access check request/response, read-tuples,
generic sync envelope, tuple-format rejection conditions) live in
[`docs/fga-sync-contract.md`](docs/fga-sync-contract.md),
the canonical FGA contract owned by this repo. Read that file before changing
any subject, envelope shape, or reply contract.

For the full per-operation reference and best practices, see
[`docs/client-guide.md`](docs/client-guide.md).

## Testing

Use `make test` (or `make test-coverage` for an HTML report). Test files are
co-located with the file under test (`handler_access_test.go` next to
`handler_access.go`, etc.) and drive handlers via the `INatsMsg` and `FgaService`
mocks in `mock.go`. There is no in-repo integration suite; integration coverage
lives in the platform stack via `lfx-v2-helm` and `load-mock-data`.

The path-scoped `fga-sync-dev` skill auto-attaches on `**/*_test.go`
and owns the table-driven test pattern and mocking conventions.

## Code Architecture

Handler shape, `FgaService` abstraction rules, JetStream KV cache pattern, error handling, NATS queue-group conventions, slog usage, and the table-driven test pattern with `INatsMsg`/`FgaService` mocks all live in the path-scoped `fga-sync-dev` skill at `.claude/skills/fga-sync-dev/SKILL.md`. It auto-attaches when you open any Go file in this repo.

Key invariants enforced there:

- All sync handlers are generic over `GenericFGAMessage`; new resource types must NOT require fga-sync code changes.
- `FgaService` stays object-agnostic; domain logic lives in handlers.
- Subject strings, queue group (`lfx.fga-sync.queue`), and KV bucket name come from `pkg/constants`, never inlined at call sites.
- Cache invalidation uses a single `inv` timestamp key; every successful OpenFGA write must bump it.
- Service continues running on individual message failures; sync handlers return errors for subscription-layer logging and only send `OK` replies after successful processing.

## Performance and Observability

- Batch OpenFGA operations (up to 100 tuples per request); cache-first reads via
  the JetStream KV bucket. See `docs/fga-sync-contract.md` for cache
  behavior and invalidation semantics.
- Expvar counters at `/debug/vars`: `cache_hits`, `cache_misses`, `cache_stale_hits`.
- Health endpoints `/livez` and `/readyz` for Kubernetes probes.
- Structured JSON logging via the slog wrapper (see `fga-sync-dev` skill).

## Deployment

### Local Development

```bash
# Set environment variables
export NATS_URL="nats://localhost:4222"
export OPENFGA_API_URL="http://localhost:8080"
export OPENFGA_STORE_ID="01K1GTJZW163H839J3YZHD8ZRY"
export OPENFGA_AUTH_MODEL_ID="01K1H4TFHDSBCZVZ5EP6HHDWE6"

# Run the service
make run
```

### Kubernetes

```bash
# Deploy with Helm
helm install fga-sync ./charts/lfx-v2-fga-sync \
  --set nats.url=nats://lfx-platform-nats.lfx.svc.cluster.local:4222 \
  --set fga.apiUrl=http://lfx-platform-openfga.lfx.svc.cluster.local:8080 \
  --set fga.storeId=01K1GTJZW163H839J3YZHD8ZRY \
  --set fga.modelId=01K1H4TFHDSBCZVZ5EP6HHDWE6
```

## Troubleshooting

- **Build failures**: ensure Go matches `go.mod` (currently 1.25+) and run `go mod tidy`.
- **NATS / OpenFGA connection**: verify `NATS_URL` and `OPENFGA_API_URL`.
- **Cache or stale access checks**: see the cache behavior and "Debugging Access Issues"
  sections in `docs/fga-sync-contract.md` (covers `/debug/vars` counters, the
  `inv` invalidation key, and the `scripts/audit/list-tuple-changes` CLI).
- **Verbose logging**: set `DEBUG=true`.
- **Health**: `/livez` and `/readyz`.
