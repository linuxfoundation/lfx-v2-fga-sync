# FGA Contract Catalog

This page is the onboarding entry point for service owners adding FGA sync to a
new resource service. For the full list of FGA-protected object types and the
operations each service supports, see
[`docs/fga-protected-types.md`](fga-protected-types.md).

Each service owns its FGA contract (`docs/fga-contract.md` in that repo) — the authoritative reference for the object
types it manages, the NATS subjects it publishes to, and the payload shape for each operation. When a service's access
control logic changes, update that service's contract; update this repo's protected-types inventory only when the
service's object types or supported operations change.

---

## Services

The "Object Types" column lists the `object_type` string values carried in each FGA sync message payload. fga-sync's
handlers are generic and do not require a constant per type; where `pkg/constants/fga.go` does define a type prefix,
it matches these values (without the trailing colon used internally).

| Service | Object Types | FGA Contract |
|---|---|---|
| [lfx-v2-project-service](https://github.com/linuxfoundation/lfx-v2-project-service) | `project` | [fga-contract.md](https://github.com/linuxfoundation/lfx-v2-project-service/blob/main/docs/fga-contract.md) |
| [lfx-v2-committee-service](https://github.com/linuxfoundation/lfx-v2-committee-service) | `committee` | [fga-contract.md](https://github.com/linuxfoundation/lfx-v2-committee-service/blob/main/docs/fga-contract.md) |
| [lfx-v2-meeting-service](https://github.com/linuxfoundation/lfx-v2-meeting-service) | `v1_meeting`, `v1_past_meeting` | [fga-contract.md](https://github.com/linuxfoundation/lfx-v2-meeting-service/blob/main/docs/fga-contract.md) |
| [lfx-v2-voting-service](https://github.com/linuxfoundation/lfx-v2-voting-service) | `vote`, `vote_response` | [fga-contract.md](https://github.com/linuxfoundation/lfx-v2-voting-service/blob/main/docs/fga-contract.md) |
| [lfx-v2-survey-service](https://github.com/linuxfoundation/lfx-v2-survey-service) | `survey`, `survey_response` | [fga-contract.md](https://github.com/linuxfoundation/lfx-v2-survey-service/blob/main/docs/fga-contract.md) |
| [lfx-v2-mailing-list-service](https://github.com/linuxfoundation/lfx-v2-mailing-list-service) | `groupsio_service`, `groupsio_mailing_list` | [fga-contract.md](https://github.com/linuxfoundation/lfx-v2-mailing-list-service/blob/main/docs/fga-contract.md) |
| [lfx-v2-member-service](https://github.com/linuxfoundation/lfx-v2-member-service) | `b2b_org`, `project_membership` | [fga-contract.md](https://github.com/linuxfoundation/lfx-v2-member-service/blob/main/docs/fga-contract.md) |

---

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
