# scripts

Utility scripts for operating and maintaining the fga-sync service.

## Structure

```
scripts/
└── audit/                          # Inspect and verify authorization state
    └── list-tuple-changes/         # List recent tuple writes and deletes across the store
```

## Folders

### `audit/`

Scripts for inspecting and verifying the state of authorization data in OpenFGA.
Use these when you need to understand what tuples exist, what has changed recently,
or to diagnose unexpected access control behavior.
