# FGA Sync Client Guide - Generic Handlers

This guide explains how to use the **generic, resource-agnostic** FGA Sync handlers to manage fine-grained authorization for your resources.

## Overview

The FGA Sync service provides four universal NATS subjects that work with **any resource type** (projects, committees, meetings, etc.) without requiring resource-specific handlers.

### Benefits of Generic Handlers

- ✅ **Resource Agnostic** - Works with any object type
- ✅ **Multiple Relations** - Add/remove multiple relations atomically
- ✅ **Mutually Exclusive Relations** - Automatic cleanup of conflicting roles
- ✅ **Future Proof** - New resource types require no service changes
- ✅ **Consistent API** - Same pattern for all operations

---

## NATS Subjects

| Subject | Purpose |
|---------|---------|
| `lfx.fga-sync.update_access` | Create/update access control for a resource |
| `lfx.fga-sync.delete_access` | Delete all access control for a resource |
| `lfx.fga-sync.member_put` | Add member(s) with one or more relations |
| `lfx.fga-sync.member_remove` | Remove member relations |

---

## Message Format

All messages use the **GenericFGAMessage** envelope:

```json
{
  "object_type": "your_resource_type",
  "operation": "operation_name",
  "data": {
    // Operation-specific fields
  }
}
```

### Fields

- **`object_type`** *(required, string)* - Your resource type (e.g., `"committee"`, `"project"`, `"meeting"`)
- **`operation`** *(required, string)* - Must match the NATS subject operation
- **`data`** *(required, object)* - Operation-specific payload (see below)

---

## 1. Update Access Control

**Subject:** `lfx.fga-sync.update_access`

Updates or creates access control for a resource. This is a full sync operation - any relations not included will be removed.

### Data Fields

```json
{
  "object_type": "committee",
  "operation": "update_access",
  "data": {
    "uid": "committee-123",
    "public": true,
    "relations": {
      "member": ["user1", "user2"],
      "viewer": ["user3"]
    },
    "references": {
      "parent": ["parent-committee-456"],
      "project": ["project-789"]
    },
    "exclude_relations": ["participant"]
  }
}
```

#### Data Object Fields

- **`uid`** *(required, string)* - Unique identifier for your resource
- **`public`** *(optional, boolean)* - If `true`, adds `user:*` as viewer (public access)
- **`relations`** *(optional, object)* - Map of relation names to arrays of usernames
  - Key: Relation name (e.g., `"member"`, `"viewer"`, `"editor"`)
  - Value: Array of usernames (e.g., `["user1", "user2"]`)
- **`references`** *(optional, object)* - Map of relation names to arrays of object UIDs
  - Key: Relation name (e.g., `"parent"`, `"project"`)
  - Value: Array of object UIDs in one of two formats:
    - **Just the ID:** `["parent-123"]` (the handler will prepend the type)
    - **Full type:ID format:** `["committee:parent-123"]` (used as-is)
  - The handler automatically detects which format you're using
- **`exclude_relations`** *(optional, array)* - Relations managed elsewhere (won't be synced)

### Examples

#### Basic Access Control

```json
{
  "object_type": "project",
  "operation": "update_access",
  "data": {
    "uid": "project-123",
    "public": false,
    "relations": {
      "editor": ["alice", "bob"],
      "viewer": ["charlie"]
    }
  }
}
```

#### With Parent Reference (ID Only)

```json
{
  "object_type": "committee",
  "operation": "update_access",
  "data": {
    "uid": "subcommittee-456",
    "public": true,
    "relations": {
      "member": ["user1", "user2"]
    },
    "references": {
      "parent": ["parent-committee-123"]
    }
  }
}
```

> **Note:** The parent reference uses just the ID `"parent-committee-123"`. The handler automatically prepends `"committee:"` to create the full reference `"committee:parent-committee-123"`.

#### With Parent Reference (Full Type:ID Format)

```json
{
  "object_type": "committee",
  "operation": "update_access",
  "data": {
    "uid": "subcommittee-456",
    "public": true,
    "relations": {
      "member": ["user1", "user2"]
    },
    "references": {
      "parent": ["committee:parent-committee-123"]
    }
  }
}
```

> **Note:** This example uses the full `"committee:parent-committee-123"` format. The handler detects the colon and uses the value as-is. Both formats produce the same result.

#### With Excluded Relations

Use `exclude_relations` when some relations are managed by separate member operations:

```json
{
  "object_type": "meeting",
  "operation": "update_access",
  "data": {
    "uid": "meeting-789",
    "public": false,
    "relations": {
      "organizer": ["alice"]
    },
    "references": {
      "project": ["project-123"]
    },
    "exclude_relations": ["participant", "host"]
  }
}
```

> **Note:** The `participant` and `host` relations are managed by separate `member_put`/`member_remove` operations, so they're excluded from the sync.

### Go Example

```go
import (
    "encoding/json"
    "github.com/nats-io/nats.go"
)

type UpdateAccessData struct {
    UID              string              `json:"uid"`
    Public           bool                `json:"public"`
    Relations        map[string][]string `json:"relations"`
    References       map[string][]string `json:"references,omitempty"`
    ExcludeRelations []string            `json:"exclude_relations,omitempty"`
}

type GenericFGAMessage struct {
    ObjectType string      `json:"object_type"`
    Operation  string      `json:"operation"`
    Data       interface{} `json:"data"`
}

// Send update access message
msg := GenericFGAMessage{
    ObjectType: "committee",
    Operation:  "update_access",
    Data: UpdateAccessData{
        UID:    "committee-123",
        Public: true,
        Relations: map[string][]string{
            "member": {"alice", "bob"},
            "viewer": {"charlie"},
        },
    },
}

payload, _ := json.Marshal(msg)
nc.Request("lfx.fga-sync.update_access", payload, 5*time.Second)
```

---

## 2. Delete Access Control

**Subject:** `lfx.fga-sync.delete_access`

Deletes **all** access control tuples for a resource. Typically used when a resource is deleted.

### Data Fields

```json
{
  "object_type": "committee",
  "operation": "delete_access",
  "data": {
    "uid": "committee-123"
  }
}
```

#### Data Object Fields

- **`uid`** *(required, string)* - Unique identifier for the resource to delete

### Examples

#### Delete Committee Access

```json
{
  "object_type": "committee",
  "operation": "delete_access",
  "data": {
    "uid": "committee-123"
  }
}
```

#### Delete Meeting Access

```json
{
  "object_type": "meeting",
  "operation": "delete_access",
  "data": {
    "uid": "meeting-456"
  }
}
```

### Go Example

```go
type DeleteAccessData struct {
    UID string `json:"uid"`
}

msg := GenericFGAMessage{
    ObjectType: "project",
    Operation:  "delete_access",
    Data: DeleteAccessData{
        UID: "project-123",
    },
}

payload, _ := json.Marshal(msg)
nc.Request("lfx.fga-sync.delete_access", payload, 5*time.Second)
```

---

## 3. Add Member(s)

**Subject:** `lfx.fga-sync.member_put`

Adds a user to a resource with one or more relations. Supports **atomic multi-relation updates** and **mutually exclusive relation handling**.

### Data Fields

```json
{
  "object_type": "committee",
  "operation": "member_put",
  "data": {
    "uid": "committee-123",
    "username": "alice",
    "relations": ["member"],
    "mutually_exclusive_with": ["guest"]
  }
}
```

#### Data Object Fields

- **`uid`** *(required, string)* - Unique identifier for the resource
- **`username`** *(required, string)* - Username (without `user:` prefix)
- **`relations`** *(required, array)* - Array of relation names to add
- **`mutually_exclusive_with`** *(optional, array)* - Relations to auto-remove (for role transitions)

### Examples

#### Add Single Relation

```json
{
  "object_type": "committee",
  "operation": "member_put",
  "data": {
    "uid": "committee-123",
    "username": "alice",
    "relations": ["member"]
  }
}
```

#### Add Multiple Relations (Atomic)

Add both `host` and `invitee` relations in a single atomic operation:

```json
{
  "object_type": "past_meeting",
  "operation": "member_put",
  "data": {
    "uid": "past-meeting-456",
    "username": "bob",
    "relations": ["host", "invitee"]
  }
}
```

#### Mutually Exclusive Relations

When promoting a participant to host, automatically remove the `participant` relation:

```json
{
  "object_type": "meeting",
  "operation": "member_put",
  "data": {
    "uid": "meeting-789",
    "username": "charlie",
    "relations": ["host"],
    "mutually_exclusive_with": ["participant", "host"]
  }
}
```

> **Behavior:** The handler will:
>
> 1. Check existing tuples for the user
> 2. Remove any relations listed in `mutually_exclusive_with` (if present)
> 3. Add the new relation(s) from `relations` array
> 4. Skip writes if the user already has the correct relations (idempotent)

### Go Example

```go
type MemberData struct {
    UID                   string   `json:"uid"`
    Username              string   `json:"username"`
    Relations             []string `json:"relations"`
    MutuallyExclusiveWith []string `json:"mutually_exclusive_with,omitempty"`
}

// Single relation
msg := GenericFGAMessage{
    ObjectType: "committee",
    Operation:  "member_put",
    Data: MemberData{
        UID:       "committee-123",
        Username:  "alice",
        Relations: []string{"member"},
    },
}

// Multiple relations
msg := GenericFGAMessage{
    ObjectType: "past_meeting",
    Operation:  "member_put",
    Data: MemberData{
        UID:       "past-meeting-456",
        Username:  "bob",
        Relations: []string{"host", "invitee", "attendee"},
    },
}

// Mutually exclusive
msg := GenericFGAMessage{
    ObjectType: "meeting",
    Operation:  "member_put",
    Data: MemberData{
        UID:                   "meeting-789",
        Username:              "charlie",
        Relations:             []string{"host"},
        MutuallyExclusiveWith: []string{"participant", "host"},
    },
}

payload, _ := json.Marshal(msg)
nc.Request("lfx.fga-sync.member_put", payload, 5*time.Second)
```

---

## 4. Remove Member(s)

**Subject:** `lfx.fga-sync.member_remove`

Removes specific relations or all relations for a user from a resource.

### Data Fields

```json
{
  "object_type": "committee",
  "operation": "member_remove",
  "data": {
    "uid": "committee-123",
    "username": "alice",
    "relations": ["member"]
  }
}
```

#### Data Object Fields

- **`uid`** *(required, string)* - Unique identifier for the resource
- **`username`** *(required, string)* - Username (without `user:` prefix)
- **`relations`** *(required, array)* - Array of relation names to remove
  - **Empty array `[]`** - Removes ALL relations for this user

### Examples

#### Remove Specific Relations

Remove only the `invitee` relation, keeping other relations intact:

```json
{
  "object_type": "past_meeting",
  "operation": "member_remove",
  "data": {
    "uid": "past-meeting-456",
    "username": "bob",
    "relations": ["invitee"]
  }
}
```

#### Remove Multiple Specific Relations

```json
{
  "object_type": "past_meeting",
  "operation": "member_remove",
  "data": {
    "uid": "past-meeting-456",
    "username": "charlie",
    "relations": ["host", "organizer"]
  }
}
```

#### Remove ALL Relations

Use an **empty array** to remove all relations for the user:

```json
{
  "object_type": "committee",
  "operation": "member_remove",
  "data": {
    "uid": "committee-123",
    "username": "alice",
    "relations": []
  }
}
```

> **Behavior:** When `relations` is an empty array, the handler calls `DeleteTuplesByUserAndObject()` to remove all tuples for this user-object pair.

### Go Example

```go
type MemberData struct {
    UID       string   `json:"uid"`
    Username  string   `json:"username"`
    Relations []string `json:"relations"`
}

// Remove specific relation
msg := GenericFGAMessage{
    ObjectType: "committee",
    Operation:  "member_remove",
    Data: MemberData{
        UID:       "committee-123",
        Username:  "alice",
        Relations: []string{"member"},
    },
}

// Remove all relations
msg := GenericFGAMessage{
    ObjectType: "committee",
    Operation:  "member_remove",
    Data: MemberData{
        UID:       "committee-123",
        Username:  "alice",
        Relations: []string{}, // Empty array
    },
}

payload, _ := json.Marshal(msg)
nc.Request("lfx.fga-sync.member_remove", payload, 5*time.Second)
```

---

## Complete Use Case Examples

### Use Case 1: Committee Lifecycle

#### Create Committee with Initial Members

```json
{
  "object_type": "committee",
  "operation": "update_access",
  "data": {
    "uid": "tech-committee-001",
    "public": false,
    "relations": {
      "admin": ["alice"],
      "member": ["bob", "charlie"]
    },
    "references": {
      "project": ["linux-foundation"]
    }
  }
}
```

#### Add New Member

```json
{
  "object_type": "committee",
  "operation": "member_put",
  "data": {
    "uid": "tech-committee-001",
    "username": "dave",
    "relations": ["member"]
  }
}
```

#### Promote Member to Admin

```json
{
  "object_type": "committee",
  "operation": "member_put",
  "data": {
    "uid": "tech-committee-001",
    "username": "bob",
    "relations": ["admin", "member"]
  }
}
```

#### Remove Member

```json
{
  "object_type": "committee",
  "operation": "member_remove",
  "data": {
    "uid": "tech-committee-001",
    "username": "charlie",
    "relations": []
  }
}
```

#### Delete Committee

```json
{
  "object_type": "committee",
  "operation": "delete_access",
  "data": {
    "uid": "tech-committee-001"
  }
}
```

---

### Use Case 2: Meeting with Dynamic Participants

#### Create Meeting

```json
{
  "object_type": "meeting",
  "operation": "update_access",
  "data": {
    "uid": "meeting-2026-01-15",
    "public": false,
    "relations": {
      "organizer": ["alice"]
    },
    "references": {
      "project": ["project-123"],
      "committee": ["tech-committee-001"]
    },
    "exclude_relations": ["participant", "host"]
  }
}
```

> **Note:** We exclude `participant` and `host` because they'll be managed via member operations.

#### Register Participant

```json
{
  "object_type": "meeting",
  "operation": "member_put",
  "data": {
    "uid": "meeting-2026-01-15",
    "username": "bob",
    "relations": ["participant"]
  }
}
```

#### Promote Participant to Host

```json
{
  "object_type": "meeting",
  "operation": "member_put",
  "data": {
    "uid": "meeting-2026-01-15",
    "username": "bob",
    "relations": ["host"],
    "mutually_exclusive_with": ["participant", "host"]
  }
}
```

> **Result:** Bob's `participant` relation is automatically removed and replaced with `host`.

#### Cancel Participant Registration

```json
{
  "object_type": "meeting",
  "operation": "member_remove",
  "data": {
    "uid": "meeting-2026-01-15",
    "username": "bob",
    "relations": ["participant", "host"]
  }
}
```

---

### Use Case 3: Past Meeting with Multiple Participant Roles

#### Record Past Meeting Participant

A participant can have multiple roles (invitee, attendee, host):

```json
{
  "object_type": "past_meeting",
  "operation": "member_put",
  "data": {
    "uid": "past-meeting-123",
    "username": "alice",
    "relations": ["host", "invitee", "attendee"]
  }
}
```

#### Update Participant Status

Remove `attendee` if they didn't actually attend:

```json
{
  "object_type": "past_meeting",
  "operation": "member_remove",
  "data": {
    "uid": "past-meeting-123",
    "username": "alice",
    "relations": ["attendee"]
  }
}
```

> **Result:** Alice keeps `host` and `invitee` relations but loses `attendee`.

---

## Response Format

All operations return a simple `"OK"` string on success:

```text
OK
```

Errors are logged server-side and returned as error messages in the NATS reply.

---

## Best Practices

### 1. Use Idempotent Operations

The handlers are designed to be idempotent. Calling `member_put` multiple times with the same data will only write once:

```json
// First call: Creates tuple
{"object_type": "committee", "operation": "member_put", "data": {"uid": "123", "username": "alice", "relations": ["member"]}}

// Second call: Skips write (logs "no changes needed")
{"object_type": "committee", "operation": "member_put", "data": {"uid": "123", "username": "alice", "relations": ["member"]}}
```

### 2. Atomic Multi-Relation Updates

When a user needs multiple relations, send them in one `member_put` operation:

```json
// ✅ Good: Atomic operation
{
  "object_type": "past_meeting",
  "operation": "member_put",
  "data": {
    "uid": "meeting-123",
    "username": "alice",
    "relations": ["host", "invitee", "attendee"]
  }
}

// ❌ Bad: Three separate operations (race conditions possible)
// member_put with ["host"]
// member_put with ["invitee"]
// member_put with ["attendee"]
```

### 3. Exclude Dynamically Managed Relations

If some relations are managed separately, exclude them from `update_access`:

```json
{
  "operation": "update_access",
  "data": {
    "uid": "meeting-123",
    "relations": {
      "organizer": ["alice"]
    },
    "exclude_relations": ["participant", "host"]
  }
}
```

### 4. Use Mutually Exclusive for Role Transitions

When users can only have one role at a time, use `mutually_exclusive_with`:

```json
{
  "operation": "member_put",
  "data": {
    "uid": "meeting-123",
    "username": "bob",
    "relations": ["host"],
    "mutually_exclusive_with": ["participant", "host"]
  }
}
```

### 5. Empty Array for Complete Removal

To remove all relations for a user, use an empty `relations` array:

```json
{
  "operation": "member_remove",
  "data": {
    "uid": "committee-123",
    "username": "alice",
    "relations": []
  }
}
```

---

## Performance Characteristics

Based on production testing:

| Operation | Average Response Time | Notes |
|-----------|----------------------|-------|
| `update_access` | 30-60ms | Depends on number of relations |
| `delete_access` | 7-10ms | Fast deletion |
| `member_put` (new) | 8-11ms | Creates new tuple |
| `member_put` (existing) | 5-6ms | Idempotent, skips write |
| `member_put` (mutually exclusive) | 80-90ms | Extra read/delete operations |
| `member_remove` (specific) | 8-9ms | Deletes specific tuples |
| `member_remove` (all) | 8-9ms | Deletes all user tuples |

---

## Error Handling

### Common Errors

**Missing Required Field:**

```json
// Error: object_type is required
{
  "operation": "member_put",
  "data": { ... }
}
```

**Empty Username:**

```json
// Error: username is required
{
  "object_type": "committee",
  "operation": "member_put",
  "data": {
    "uid": "committee-123",
    "username": "",
    "relations": ["member"]
  }
}
```

**Empty Relations Array in member_put:**

```json
// Error: relations array cannot be empty for member_put
{
  "object_type": "committee",
  "operation": "member_put",
  "data": {
    "uid": "committee-123",
    "username": "alice",
    "relations": []
  }
}
```

**Invalid JSON:**

```text
Error: failed to parse generic message
```

---

## Migration from Resource-Specific Handlers

If you're currently using resource-specific subjects like `lfx.put_member.committee`, here's how to migrate:

### Before (Resource-Specific)

```json
// Subject: lfx.put_member.committee
{
  "username": "alice",
  "committee_uid": "committee-123"
}
```

### After (Generic)

```json
// Subject: lfx.fga-sync.member_put
{
  "object_type": "committee",
  "operation": "member_put",
  "data": {
    "uid": "committee-123",
    "username": "alice",
    "relations": ["member"]
  }
}
```

### Benefits of Migration

- ✅ Support for multiple relations
- ✅ Mutually exclusive relation handling
- ✅ Works with any resource type
- ✅ No service changes needed for new resource types

---

## FAQ

**Q: Can I mix resource-specific and generic handlers?**
A: Yes! Both continue to work. Migrate at your own pace.

**Q: What happens if I send the same member_put twice?**
A: The handler is idempotent - it checks existing tuples and skips the write if the relation already exists.

**Q: Can I add multiple relations in one operation?**
A: Yes! Use the `relations` array: `"relations": ["host", "invitee", "attendee"]`

**Q: How do I remove all relations for a user?**
A: Send `member_remove` with an empty relations array: `"relations": []`

**Q: What's the difference between `update_access` and `member_put`?**
A: `update_access` is a full sync (sets complete state), while `member_put` adds specific relations incrementally.

**Q: Can I use any object_type value?**
A: Yes! The handlers are resource-agnostic. Use any string like `"committee"`, `"project"`, `"working_group"`, etc.

---

## Quick Reference

```bash
# Update Access
nats request lfx.fga-sync.update_access '{"object_type":"committee","operation":"update_access","data":{"uid":"123","public":true,"relations":{"member":["alice"]}}}'

# Delete Access
nats request lfx.fga-sync.delete_access '{"object_type":"committee","operation":"delete_access","data":{"uid":"123"}}'

# Add Member
nats request lfx.fga-sync.member_put '{"object_type":"committee","operation":"member_put","data":{"uid":"123","username":"alice","relations":["member"]}}'

# Remove Member
nats request lfx.fga-sync.member_remove '{"object_type":"committee","operation":"member_remove","data":{"uid":"123","username":"alice","relations":[]}}'
```
