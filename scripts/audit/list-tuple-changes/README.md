# list-tuple-changes

CLI tool for inspecting recent OpenFGA tuple changes across the entire store.

## Prerequisites

The following environment variables must be set:

```bash
export OPENFGA_API_URL="http://localhost:8080"
export OPENFGA_STORE_ID="<your-store-id>"
export OPENFGA_AUTH_MODEL_ID="<your-auth-model-id>"
```

## Usage

```bash
go run ./scripts/audit/list-tuple-changes [flags]
```

## Flags

| Flag | Default | Description |
| --- | --- | --- |
| `-since` | `1h` | How far back to look for changes. Accepts Go duration strings: `30m`, `2h`, `24h`, `168h`, etc. |
| `-type` | _(all types)_ | Filter results to a specific object type (e.g. `project`, `committee`, `meeting`). |
| `-all-pages` | `false` | When set, automatically pages through all results instead of stopping after the first page. |

## Examples

```bash
# Show all changes from the last hour (default)
go run ./scripts/audit/list-tuple-changes

# Show changes from the last 24 hours
go run ./scripts/audit/list-tuple-changes -since 24h

# Show only project tuple changes from the last 30 minutes
go run ./scripts/audit/list-tuple-changes -since 30m -type project

# Show all committee changes from the last week, paginating through everything
go run ./scripts/audit/list-tuple-changes -since 168h -type committee -all-pages
```

## Output

```text
Fetching tuple changes since 2026-04-02 09:00:00 (type: project)

[2026-04-02 09:45:12] WRITE     project:abc-123#writer@user:456
[2026-04-02 09:46:01] DELETE    project:abc-123#viewer@user:789
[2026-04-02 09:50:33] WRITE     project:xyz-789#viewer@user:*

Total changes: 3
```

Each line contains:

- **Timestamp** — local time when the change was recorded
- **Operation** — `WRITE` (tuple created) or `DELETE` (tuple removed)
- **Tuple** — in `object#relation@user` format

## API Reference

This script uses the OpenFGA [ReadChanges](https://openfga.dev/api/service#/Relationship%20Tuples/ReadChanges) API endpoint.
