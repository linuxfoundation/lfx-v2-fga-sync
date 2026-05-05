# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Service Overview

FGA Sync is a high-performance Go microservice that synchronizes authorization data between NATS messaging and OpenFGA (Fine-Grained Authorization). It provides cached relationship checks and real-time access control updates for the LFX Platform v2.

## Architecture

### Core Components

- **NATS Message Handlers**: Process access checks, read-tuples queries, and resource permission sync (updates, deletes, member operations)
- **OpenFGA Client**: Manages authorization relationships and batch operations
- **JetStream Cache**: High-performance KeyValue store for relationship caching
- **Health Endpoints**: Kubernetes-ready liveness and readiness probes

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

Response is plain text, one line per check, tab-delimited `{request}\t{true|false}`. Order is not guaranteed — cached results are returned first.

### Read Tuples Request (`lfx.access_check.read_tuples`)

Returns all direct OpenFGA tuples for a given user and object type. Paginates internally.

**Request:**

```json
{"user": "user:auth0|alice", "object_type": "project"}
```

**Response (success):**

```json
{"results": ["project:uuid1#writer@user:auth0|alice", "project:uuid2#auditor@user:auth0|alice"]}
```

**Response (error):**

```json
{"error": "failed to read tuples"}
```

### Generic Sync Message Format (`lfx.fga-sync.*`)

New integrations should use the generic subjects. All use the `GenericFGAMessage` envelope:

```json
{
  "object_type": "your_resource_type",
  "operation": "operation_name",
  "data": { /* operation-specific fields */ }
}
```

Subjects: `lfx.fga-sync.update_access`, `lfx.fga-sync.delete_access`, `lfx.fga-sync.member_put`,
`lfx.fga-sync.member_remove`. See `docs/client-guide.md` for full format details, examples, and best practices.

If a reply subject is provided, sync handlers respond with `OK` after processing, allowing callers to implement
synchronous acknowledgement.

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
- `readTuplesHandler()` - Returns all direct OpenFGA tuples for a user + object type
- `genericUpdateAccessHandler()` - Generic resource permission sync (any object type)
- `genericDeleteAccessHandler()` - Generic resource permission cleanup
- `genericMemberPutHandler()` - Generic per-user relation add with multi-relation and mutual exclusion support
- `genericMemberRemoveHandler()` - Generic per-user relation removal

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
