# FGA Contract Catalog

This page is the onboarding entry point for service owners adding FGA sync to a
new resource service. For the full list of FGA-protected object types and the
operations each service supports, see
[`docs/fga-protected-types.md`](fga-protected-types.md).

Each service owns its FGA contract (`docs/fga-contract.md` in that repo), which
is the authoritative reference for the object types it manages, the NATS
subjects it publishes to, and the payload shape for each operation. When a
service's access control logic changes, update that service's contract; update
this repo's protected-types inventory only when the service's object types or
supported operations change.

## Adding a New Service

When a new service starts publishing FGA sync messages:

1. Add or update `docs/fga-contract.md` in that service's repo. Use an
   existing resource service as a shape-only example, but do not copy
   service-specific relations or references; the target service owns its own
   contract.
2. Add a row to the service table in
   [`docs/fga-protected-types.md`](fga-protected-types.md)
   with the service name, object types, and a link to its contract.

All integrations must publish to the generic `lfx.fga-sync.*` subjects. See the
[client guide](client-guide.md) for caller examples, and
[`docs/fga-sync-contract.md`](fga-sync-contract.md) for the authoritative
generic subjects, envelope shape, tuple format, cache behavior, and
access-check semantics.
