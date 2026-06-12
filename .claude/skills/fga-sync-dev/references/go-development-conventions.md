<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# Go Development Conventions (V2 baseline)

Longer-form Go style conventions shared across LFX V2 Go services. The inline body of `SKILL.md` is the fga-sync-specific overlay (single `package main`, slog wrapper, `INatsMsg` mocks, JetStream KV cache pattern). This reference covers shared patterns that are not repeated there: idiomatic Go style, error mapping, context propagation, pagination, and review hygiene.

Read this when onboarding to V2 Go style. Most fga-sync work does not need it.

## Domain errors and HTTP/transport mapping

- Prefer typed domain errors over sentinel `errors.New` strings.
- New code should preserve the shared V2 mapping at the transport boundary: validation to 400, not found to 404, conflict to 409, internal to 500, unavailable to 503.
- Wrap upstream errors with `%w` so `errors.Is` and `errors.Unwrap` still work.
- Translate domain errors at the transport boundary. Do not return raw upstream HTTP errors or third-party SDK payloads to the caller.
- Do not introduce a parallel sentinel-error family if the repo already has typed domain errors. fga-sync currently returns handler errors to the NATS subscription loop for logging, with subject-specific replies only where documented (`read_tuples` JSON errors and access-check text errors).

## Request context

- Construct or accept `context.Context` at every public function boundary that does I/O. `context.Background()` is acceptable at the top of a NATS handler before any plumbing exists; pass it through to `FgaService` calls and to slog `*Context` methods.
- Use typed context keys or constants; do not use bare strings.
- Forward context values into downstream calls (NATS, OpenFGA, HTTP) only when the receiving contract needs them.
- Do not read HTTP headers from service-layer code. Middleware owns request-context setup.

## Pagination (for V2 services that expose list endpoints)

- Standard list shape is `page_size` and opaque `page_token`.
- Default `page_size` is 50; the maximum is normally 1000.
- Return a next `page_token` only when another page exists.
- `page_token` is opaque to clients; service owns its encoding.
- Wrapper services translate upstream pagination to this shape at the service boundary.

fga-sync currently exposes no public list endpoints; the `lfx.access_check.read_tuples` handler paginates internally against OpenFGA and returns the full result set in one reply.

## Tests (shared V2 patterns)

- Depend on interfaces for external systems: NATS clients, OpenFGA clients, KV stores, HTTP clients. fga-sync's equivalents are `INatsMsg`, `FgaService`, and the KV bucket interface; mocks live in `mock.go`.
- Keep mocks in the repo's existing mock layout.
- Use table-driven tests for branching behavior; one test function per exported method with cases added to the table.
- Co-locate `*_test.go` with the file under test.
- Run the repo's normal test target (`make test`) before handing off. Use race detection when the target supports it.

## Formatting and review hygiene

- Run `gofmt`/`goimports` via the repo's formatter target, `make fmt`.
- Run the repo's lint target, `make lint` (golangci-lint), when Go code changes. `make check` runs format, vet, and lint.
- Preserve required license headers on new files (`// Copyright The Linux Foundation and each contributor to LFX.` plus `// SPDX-License-Identifier: MIT`).
- Document exported Go symbols when the linter requires it; otherwise add comments only where the code is not self-explanatory.
- Update repo-owned docs or contracts in the same change as code that changes behavior. For fga-sync that means `docs/fga-sync-contract.md` for any subject, envelope, reply, or cache-protocol change.
