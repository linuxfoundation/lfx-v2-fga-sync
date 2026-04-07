# FGA Contract Catalog

This document is the index of all services that publish FGA sync messages to this service, organized by service.

Each service owns its FGA contract — the authoritative reference for the object types it manages, the NATS subjects it publishes to, and the payload shape for each operation. When a service's access control logic changes, only that service's contract needs updating.

---

## Services

| Service | Object Types | FGA Contract |
|---|---|---|
| [lfx-v2-project-service](https://github.com/linuxfoundation/lfx-v2-project-service) | `project` | [fga-contract.md](https://github.com/linuxfoundation/lfx-v2-project-service/blob/main/docs/fga-contract.md) |
| [lfx-v2-committee-service](https://github.com/linuxfoundation/lfx-v2-committee-service) | `committee` | [fga-contract.md](https://github.com/linuxfoundation/lfx-v2-committee-service/blob/main/docs/fga-contract.md) |
| [lfx-v2-meeting-service](https://github.com/linuxfoundation/lfx-v2-meeting-service) | `v1_meeting`, `v1_past_meeting`, `v1_past_meeting_recording`, `v1_past_meeting_transcript`, `v1_past_meeting_summary` | [fga-contract.md](https://github.com/linuxfoundation/lfx-v2-meeting-service/blob/main/docs/fga-contract.md) |
| [lfx-v2-voting-service](https://github.com/linuxfoundation/lfx-v2-voting-service) | `vote`, `vote_response` | [fga-contract.md](https://github.com/linuxfoundation/lfx-v2-voting-service/blob/main/docs/fga-contract.md) |
| [lfx-v2-mailing-list-service](https://github.com/linuxfoundation/lfx-v2-mailing-list-service) | `groupsio_service`, `groupsio_mailing_list` | [fga-contract.md](https://github.com/linuxfoundation/lfx-v2-mailing-list-service/blob/main/docs/fga-contract.md) |

---

## Adding a New Service

When a new service starts publishing FGA sync messages:

1. Add a `docs/fga-contract.md` to that service's repo following the [committee-service pattern](https://github.com/linuxfoundation/lfx-v2-committee-service/blob/main/docs/fga-contract.md)
2. Add a row to the table above with the service name, object types, and a link to its contract

All services should publish to the generic `lfx.fga-sync.*` subjects. See the [client guide](client-guide.md) for message format details.
