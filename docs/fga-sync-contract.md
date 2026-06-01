<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# OpenFGA Access Control Contract (Authoritative)

This is the **owner document** for the platform's FGA access sync envelope, generic
FGA NATS subjects, tuple format, cache behavior, and access-check semantics. Other
services link here rather than copy.

The platform uses ReBAC (Relationship-Based Access Control) via OpenFGA. Permissions
are stored as OpenFGA tuples and checked at query time by `lfx-v2-query-service` via
this service (`lfx-v2-fga-sync`).

## Generic Subjects (publishers should use these)

| Subject | Purpose | Reply |
| --- | --- | --- |
| `lfx.fga-sync.update_access` | Create/update access tuples for a resource | `OK` on success if reply subject is provided |
| `lfx.fga-sync.delete_access` | Delete all tuples for a resource (on delete) | `OK` on success if reply subject is provided |
| `lfx.fga-sync.member_put` | Add a user to a resource with one or more relations | `OK` on success if reply subject is provided |
| `lfx.fga-sync.member_remove` | Remove specific or all relations for a user | `OK` on success if reply subject is provided |
| `lfx.access_check.request` | Batch authorization check (used by query-service) | text body |
| `lfx.access_check.read_tuples` | Read all direct tuples for a user + object_type | JSON body |

Handlers are generic: **publishers do not need fga-sync code changes when adding a
new resource type that is defined in the OpenFGA model**. Use the generic
envelope below.

## Tuple Format

```text
{object_type}:{object_id}#{relation}@{user_type}:{user_id}
```

Examples:

- `project:proj-123#writer@user:alice`: alice is a writer on this project
- `project:proj-123#viewer@user:*`: anyone (public) can view this project
- `project:child-456#parent@project:parent-123`: parent-child hierarchy link
- `committee:abc#member@user:bob`: bob is a member of this committee

### Tuple-format rejection conditions

fga-sync rejects malformed envelopes before writing to OpenFGA, and the
subscription loop logs the returned error with subject and queue context. Sync
subjects only send `OK` after successful processing; they do not currently send a
standardized error reply body. Agents debugging missing access should grep
service logs first.

| Condition | Behavior |
| --- | --- |
| `username` missing/empty on `member_put` or `member_remove` | Message rejected |
| `uid` missing/empty on any sync operation | Message rejected |
| `relations` empty on `member_put` | Message rejected |
| `relations` empty on `member_remove` | Removes ALL relations for that user (intentional) |
| `object_type` empty in envelope | Message rejected |
| Unknown `operation` value | Message rejected |
| `references` value with an empty `type` or empty `id` in `type:id` format | Message rejected |
| Tuple rejected by OpenFGA with `validation_error` | Invalid tuple is logged, removed from the batch, and the remaining batch is retried |
| Non-validation OpenFGA write/read error | Operation fails and is logged |

## Access Message Envelope: `GenericFGAMessage`

All generic sync subjects use this envelope:

```go
type GenericFGAMessage struct {
    ObjectType string      `json:"object_type"` // e.g. "committee"
    Operation  string      `json:"operation"`   // matches subject suffix
    Data       interface{} `json:"data"`
}
```

### `update_access` (create/update)

```json
{
  "object_type": "committee",
  "operation": "update_access",
  "data": {
    "uid": "resource-uuid",
    "public": false,
    "relations": {
      "writer": ["alice"],
      "auditor": ["bob"]
    },
    "references": {
      "project": ["parent-project-uuid"]
    },
    "exclude_relations": ["participant"]
  }
}
```

- `relations` is a full sync: any relation key not included is removed.
- `references` values can be bare UIDs (handler prepends the map key as the type
  prefix, e.g. `"project": ["abc"]` → `project:abc`) or full `type:uid` strings.
  Both are accepted.
- `references.project` produces tuple `committee:{committee_uid}#project@project:{project_uid}`,
  enabling permission inheritance from the parent project.
- `exclude_relations` lets a publisher manage some relations separately (e.g. members
  managed by a different subject). Those relations are left untouched.

### `delete_access` (on resource delete)

```json
{
  "object_type": "committee",
  "operation": "delete_access",
  "data": {"uid": "abc-123-uuid"}
}
```

Purges **all** OpenFGA tuples for that object across all relations.

### `member_put` / `member_remove`

```go
// Add member
GenericFGAMessage{ObjectType: "committee", Operation: "member_put",
    Data: map[string]interface{}{
        "uid": committeeUID, "username": "alice", "relations": []string{"member"},
        // optional: "mutually_exclusive_with": []string{"viewer"}
    }}

// Remove member (empty relations = remove all)
GenericFGAMessage{ObjectType: "committee", Operation: "member_remove",
    Data: map[string]interface{}{
        "uid": committeeUID, "username": "alice", "relations": []string{},
    }}
```

`member_put` is idempotent and supports `mutually_exclusive_with` for role transitions.
See `docs/client-guide.md` for the full reference and additional examples.

## Access Check Subjects (consumed by query-service)

### `lfx.access_check.request`

Multiple relationship checks, one per line; each formatted `object#relation@user`:

```text
project:7cad5a8d-19d0-41a4-81a6-043453daf9ee#writer@user:456
project:7cad5a8d-19d0-41a4-81a6-043453daf9ee#viewer@user:456
```

Reply is plain text, one line per check, tab-delimited `{request}\t{true|false}`.

**Order is not guaranteed** (cached results may be returned first); callers must
match on the request token, not by index.

### `lfx.access_check.read_tuples`

Returns all direct OpenFGA tuples for a given user and object type. Paginates internally.

```json
// Request
{"user": "user:auth0|alice", "object_type": "project"}

// Response success
{"results": ["project:uuid1#writer@user:auth0|alice"]}

// Response error
{"error": "failed to read tuples"}
```

## OpenFGA Model Boundaries

The authorization model lives in
`lfx-v2-helm/charts/lfx-platform/templates/openfga/model.yaml`. fga-sync does not
own model definitions; it only writes tuples that the model allows.

| Concern | Owner |
| --- | --- |
| Object types, relations, computed relations, hierarchical relations | `lfx-v2-helm` (model.yaml) |
| Generic sync handlers and tuple I/O | `lfx-v2-fga-sync` (this repo) |
| Per-resource emission rules (what relations a service sends) | Each resource service's `docs/fga-contract.md` |
| Heimdall `openfga_check` rules per HTTP verb/path | Each service's Helm chart `ruleset.yaml` |
| Runtime access checks (`lfx.access_check.request`) | `lfx-v2-fga-sync` answers them; `lfx-v2-access-check` exposes an HTTP wrapper |

### Permission inheritance pattern in the model

```text
type project
  relations
    define parent: [project]
    define writer: [user] or writer from parent
    define auditor: [user] or writer or auditor from parent
    define viewer: [user:*] or auditor or auditor from parent

type committee
  relations
    define project: [project]
    define writer: writer from project
    define auditor: auditor from project
    define viewer: [user:*] or auditor from project
```

- A `writer` on a parent project is automatically a `writer` on all child projects.
- A `writer` on a project is automatically a `writer` on all its committees.
- Public resources use `user:*` (wildcard): the query-service bypasses the FGA check
  entirely and filters OpenSearch by `public: true` instead.

### Model evolution policy

- **Adding a new object type or relation**: edit `model.yaml` in `lfx-v2-helm` AND
  bump the model version (Argo redeploys the new model). Existing tuples remain valid
  for relations that still exist.
- **Renaming a relation**: breaking. All existing tuples for that relation become
  unreachable; coordinate a migration.
- **Removing an object type**: breaking. Tuples become orphaned. Delete via a
  `delete_access` pass before removing the type from the model.
- **Heimdall rules** must be updated in the same PR cycle as a new model object type.
  Without an `openfga_check` rule on the routes, the gateway will not enforce.

## Cache Behavior

fga-sync caches access check results in a NATS JetStream KV bucket (`fga-sync-cache`).

| Aspect | Detail |
| --- | --- |
| Cache key | Base32-encoded relation tuple `rel.{encoded-relation}` |
| Cache value | Raw text boolean: `true` or `false`; freshness uses the NATS KV entry timestamp |
| Invalidation | A single `inv` timestamp key, every successful OpenFGA write bumps it, making all older cached entries stale |
| Stale handling | Stale hits are counted separately at `/debug/vars` and then rechecked against OpenFGA |
| Fallback | Cache miss falls through to a direct OpenFGA query |

### Debugging cache behavior

- Counters at `/debug/vars`: `cache_hits`, `cache_misses`, `cache_stale_hits`.
- If access checks return wrong/old results, look for `"cache invalidation failed"`
  in fga-sync logs. The `inv` key may have failed to bump.
- Manually invalidate by writing any value to the `inv` key in the `fga-sync-cache`
  bucket; this forces every cached entry to be treated as stale on next read.
- A successful any-type OpenFGA write re-invalidates. When in doubt, trigger any
  `update_access` on any resource and stale entries clear globally.

## Publishing Access Messages (Go Code Example)

```go
msg := GenericFGAMessage{
    ObjectType: "sponsorship",
    Operation:  "update_access",
    Data: map[string]interface{}{
        "uid":    resource.UID,
        "public": resource.Public,
        "relations": map[string][]string{
            "writer": {"alice"},
        },
        "references": map[string][]string{
            "project": {resource.ProjectUID},
        },
    },
}

payload, err := json.Marshal(msg)
if err != nil {
    return err
}
nc.Publish("lfx.fga-sync.update_access", payload)
```

## Debugging Access Issues

When a user can't see a resource they should have access to, there are two root
causes. Check them in this order:

### 1. Indexing problem: document missing or stale in OpenSearch

Query OpenSearch directly:

```bash
curl "$OPENSEARCH_URL/lfx-resources/_search" -H 'Content-Type: application/json' -d '{
  "query": {"bool": {"must": [
    {"term": {"object_type": "committee"}},
    {"term": {"object_id": "<uid>"}},
    {"term": {"latest": true}}
  ]}},
  "_source": ["access_check_object", "access_check_relation", "public"]
}'
```

- No results → index message was never published or indexer failed to process it.
- Results but `access_check_object` empty → `IndexingConfig` was missing or malformed.
  See `lfx-v2-indexer-service/docs/indexer-contract.md`.
- Fix: trigger a no-op update on the resource to republish both NATS messages.

### 2. Permissions problem: FGA tuple missing or wrong

Check existing tuples:

```bash
fga tuple read --object committee:<uid>
```

Common causes:

- `update_access` NATS message never published (check resource service logs for
  publish errors).
- Wrong `references` in the access message (wrong parent project UID).
- User's LFID in the JWT doesn't match the username stored in the tuple.
- Member payload was missing `username`. fga-sync rejects `member_put` and
  `member_remove` when username is empty.
- Cache is stale. Any successful OpenFGA write re-invalidates, or manually write to
  the `inv` KV key.

### Auditing recent tuple changes

For a quick view of recent OpenFGA writes/deletes across the store, run the
`list-tuple-changes` CLI from this repo:

```bash
go run ./scripts/audit/list-tuple-changes -since 1h -type committee
```

See `scripts/audit/list-tuple-changes/README.md` for flags (`-since`, `-type`,
`-all-pages`) and example output.

## FGA Contract: Per-Service Documentation

Services that follow the FGA contract pattern keep a `docs/fga-contract.md` at the
root of their repo. This is the authoritative reference for that service's object
types, message schemas, operations, relations, and trigger conditions, derived
directly from the source code.

**Read this before writing or modifying FGA message construction for an existing
service.** It tells you what subjects are used, what payload shape is expected, and
what conditions cause messages to be rejected or skipped by OpenFGA validation.

**Update it in the same PR as any FGA messaging change.** The doc must stay in sync
with the code.

The [committee-service](https://github.com/linuxfoundation/lfx-v2-committee-service/blob/main/docs/fga-contract.md)
is the reference implementation of this pattern. Use it as a template when adding a
contract to a new service.

For a full index of all services and their FGA object types, see
`docs/fga-protected-types.md`.
