# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Service Overview

FGA Sync is a high-performance Go microservice that synchronizes authorization data between NATS messaging and OpenFGA (Fine-Grained Authorization). It provides cached relationship checks and real-time access control updates for the LFX Platform v2.

## Architecture

### Core Components

- **NATS Message Handlers**: Process access checks, project updates, and project deletions
- **OpenFGA Client**: Manages authorization relationships and batch operations
- **JetStream Cache**: High-performance KeyValue store for relationship caching
- **Health Endpoints**: Kubernetes-ready liveness and readiness probes

### Message Flow

1. NATS messages arrive on subjects (e.g., `lfx.access_check.request`)
2. Queue groups ensure load balancing across service instances
3. Handlers process messages, interact with cache/OpenFGA, and send replies
4. Cache invalidation occurs on project updates/deletions

### Key Dependencies

- `github.com/nats-io/nats.go` - NATS messaging client
- `github.com/openfga/go-sdk` - OpenFGA authorization client
- Standard library for HTTP server and JSON processing

## Common Development Commands

```bash
# Build and test
make build          # Build the fga-sync binary
make test           # Run all tests
make test-coverage  # Run tests with coverage report
make check          # Format, vet, lint, and security scan

# Development
make dev           # Build with debug symbols and race detection
make run           # Build and run the service locally
make clean         # Clean build artifacts

# Docker operations
make docker-build  # Build Docker image
make docker-run    # Run service in Docker container

# Code quality
make fmt           # Format Go code
make lint          # Run golangci-lint
make vet           # Run go vet
make gosec         # Run security scanner
```

## Configuration

### Required Environment Variables

- `NATS_URL`: NATS server connection URL (e.g., `nats://localhost:4222`)
- `OPENFGA_API_URL`: OpenFGA API endpoint (e.g., `http://localhost:8080`)
- `OPENFGA_STORE_ID`: OpenFGA store ID
- `OPENFGA_AUTH_MODEL_ID`: OpenFGA authorization model ID

### Optional Environment Variables

- `CACHE_BUCKET`: JetStream KeyValue bucket name (default: `fga-sync-cache`)
- `PORT`: HTTP server port (default: `8080`)
- `DEBUG`: Enable debug logging (default: `false`)

## Message Formats

### Access Check Request (`lfx.access_check.request`)

```
project:7cad5a8d-19d0-41a4-81a6-043453daf9ee#writer@user:456
project:7cad5a8d-19d0-41a4-81a6-043453daf9ee#viewer@user:456
```

Multiple relationship checks, one per line. Format: `object#relation@user`

### Project Update Message (`lfx.update_access.project`)

```json
{
  "uid": "7cad5a8d-19d0-41a4-81a6-043453daf9ee",
  "public": true,
  "parent_uid": "7cad5a8d-19d0-41a4-81a6-043453daf9ef", 
  "writers": ["user1", "user2"],
  "auditors": ["auditor1"]
}
```

### Project Delete Message (`lfx.delete_all_access.project`)

```
project-123
```

Simple project UID string.

### Meeting Update Message (`lfx.update_access.meeting`)

```json
{
  "uid": "meeting-123",
  "public": true,
  "project_uid": "project-456",
  "organizers": ["user1", "user2"],
  "committees": ["committee1", "committee2"]
}
```

### Meeting Delete Message (`lfx.delete_all_access.meeting`)

```text
meeting-123
```

Simple meeting UID string.

### Meeting Registrant Put Message (`lfx.put_registrant.meeting`)

```json
{
  "uid": "user-123",
  "meeting_uid": "meeting-456",
  "host": false
}
```

### Meeting Registrant Remove Message (`lfx.remove_registrant.meeting`)

```json
{
  "uid": "user-123",
  "meeting_uid": "meeting-456",
  "host": false
}
```

### Meeting Attachment Update Message (`lfx.update_access.meeting_attachment`)

```json
{
  "uid": "attachment-123",
  "meeting_uid": "meeting-456"
}
```

### Meeting Attachment Delete Message (`lfx.delete_access.meeting_attachment`)

```text
attachment-123
```

Simple attachment UID string.

### Past Meeting Recording Update Message (`lfx.update_access.past_meeting_recording`)

```json
{
  "uid": "recording-123",
  "past_meeting_uid": "past-meeting-456",
  "artifact_visibility": "public",
  "participants": [
    {
      "username": "user1",
      "host": true
    },
    {
      "username": "user2",
      "host": false
    }
  ]
}
```

The `artifact_visibility` field determines who can access the recording:
- `"public"`: All users get viewer access
- `"meeting_hosts"`: Only participants with `host: true` get viewer access
- `"meeting_participants"`: All participants get viewer access

### Past Meeting Transcript Update Message (`lfx.update_access.past_meeting_transcript`)

```json
{
  "uid": "transcript-123",
  "past_meeting_uid": "past-meeting-456",
  "artifact_visibility": "meeting_participants",
  "participants": [
    {
      "username": "user1",
      "host": true
    },
    {
      "username": "user2",
      "host": false
    }
  ]
}
```

The `artifact_visibility` field determines who can access the transcript:
- `"public"`: All users get viewer access
- `"meeting_hosts"`: Only participants with `host: true` get viewer access
- `"meeting_participants"`: All participants get viewer access

### Past Meeting Summary Update Message (`lfx.update_access.past_meeting_summary`)

```json
{
  "uid": "summary-123",
  "past_meeting_uid": "past-meeting-456",
  "artifact_visibility": "meeting_hosts",
  "participants": [
    {
      "username": "user1",
      "host": true
    },
    {
      "username": "user2",
      "host": false
    }
  ]
}
```

The `artifact_visibility` field determines who can access the summary:
- `"public"`: All users get viewer access
- `"meeting_hosts"`: Only participants with `host: true` get viewer access
- `"meeting_participants"`: All participants get viewer access

### Past Meeting Participant Put Message (`lfx.put_participant.past_meeting`)

```json
{
  "uid": "participant-123",
  "past_meeting_uid": "past-meeting-456",
  "artifact_visibility": "meeting_participants",
  "username": "user1",
  "host": true,
  "is_invited": true,
  "is_attended": true
}
```

When a participant is added or updated:
1. Updates their relations on the past meeting (host, invitee, attendee)
2. Syncs their viewer access to all artifacts based on `artifact_visibility`:
   - `"public"`: No action needed (user:* already grants access)
   - `"meeting_hosts"`: Adds viewer access only if `host: true`
   - `"meeting_participants"`: Adds viewer access for all participants

**Important**: If a participant's host status changes (e.g., from participant to host), their artifact access is automatically updated based on the visibility settings.

### Past Meeting Participant Remove Message (`lfx.remove_participant.past_meeting`)

```json
{
  "uid": "participant-123",
  "past_meeting_uid": "past-meeting-456",
  "artifact_visibility": "meeting_participants",
  "username": "user1",
  "host": false,
  "is_invited": false,
  "is_attended": false
}
```

When a participant is removed:
1. Removes all their relations from the past meeting
2. Removes their viewer access from all artifacts (regardless of artifact_visibility)

### Past Meeting Attachment Update Message (`lfx.update_access.past_meeting_attachment`)

```json
{
  "uid": "attachment-123",
  "past_meeting_uid": "past-meeting-456"
}
```

### Past Meeting Attachment Delete Message (`lfx.delete_access.past_meeting_attachment`)

```text
attachment-123
```

Simple attachment UID string.

## Testing

### Running Tests

```bash
# Run all tests
go test ./...

# Run specific test
go test -v ./... -run TestAccessCheckHandler

# Run benchmarks
go test -bench=. ./...

# Integration tests (requires Docker)
./test_integration.sh
```

### Test Structure

- `*_test.go` files contain unit tests for each handler
- `test_integration.sh` runs full service integration tests
- `docker-compose.test.yml` provides test environment with NATS and OpenFGA

## Code Architecture

### Handler Pattern

Each message type has a dedicated handler function:

- `accessCheckHandler()` - Processes authorization queries with caching
- `projectUpdateHandler()` - Manages project permission synchronization  
- `projectDeleteHandler()` - Handles cleanup of project permissions
- `meetingUpdateAccessHandler()` - Manages meeting permission synchronization
- `meetingDeleteAllAccessHandler()` - Handles cleanup of meeting permissions
- `meetingRegistrantAddHandler()` - Adds meeting registrants (participant/host)
- `meetingRegistrantRemoveHandler()` - Removes meeting registrants

### Service Abstraction

**FgaService should remain generic and object-agnostic:**

- FgaService should not contain business logic specific to projects, meetings, or other domain objects
- It should only provide generic tuple management operations (read, write, delete, sync)
- All business-specific logic should remain in the handlers
- This maintains clean separation of concerns and reusability

### Cache Strategy

- **Cache Keys**: Base32-encoded relation tuples (e.g., `rel.{encoded-relation}`)
- **Cache Values**: JSON with `allowed` boolean and `created_at` timestamp
- **Invalidation**: Timestamp-based with configurable staleness tolerance
- **Fallback**: Direct OpenFGA queries on cache miss

### Error Handling

- Structured logging with context
- Graceful degradation (cache miss → OpenFGA query)
- Message reply with error details for debugging
- Service continues running on individual message failures

## Performance Considerations

### Optimization Patterns

- Preallocated slices to reduce garbage collection
- Batch OpenFGA operations (up to 100 tuples per request)
- Cache-first approach with sub-millisecond response times
- Efficient string parsing using `bytes.Cut`

### Monitoring

- Expvar metrics at `/debug/vars` (cache hits/misses/stale hits)
- Structured JSON logging for observability
- Health endpoints for Kubernetes probes

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

### Common Issues

- **Build failures**: Ensure Go 1.24+ and run `go mod tidy`
- **NATS connection**: Verify NATS_URL and network connectivity
- **OpenFGA errors**: Check OPENFGA_API_URL and ensure OpenFGA is healthy
- **Cache issues**: Monitor cache hit rates via `/debug/vars`

### Debugging

- Set `DEBUG=true` for verbose logging
- Check service health at `/livez` and `/readyz`
- Monitor Docker container logs for connection issues
- Use `make check` to validate code quality before deployment
