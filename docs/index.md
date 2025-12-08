# LFX v2 FGA Sync Service

![Build Status](https://github.com/linuxfoundation/lfx-v2-fga-sync/workflows/FGA%20Sync%20Build/badge.svg)
![License](https://img.shields.io/badge/License-MIT-blue.svg)
![Go Version](https://img.shields.io/badge/Go-1.23+-00ADD8?logo=go)

A high-performance microservice that synchronizes authorization data between NATS messaging and OpenFGA
(Fine-Grained Authorization), providing cached relationship checks and real-time access control updates for the
LFX Platform v2.

## 🚀 Features

- **Real-time Authorization Sync**: Synchronizes project access permissions between NATS and OpenFGA
- **High-Performance Caching**: JetStream-based caching with automatic invalidation for sub-millisecond response times
- **Batch Operations**: Efficient bulk relationship checking and updates
- **Health Monitoring**: Kubernetes-ready health checks and observability
- **Security First**: Comprehensive security scanning and best practices

## 📋 Architecture

```mermaid
graph TB
    A[NATS Messages] --> B[FGA Sync Service]
    B --> C[OpenFGA]
    B --> D[JetStream Cache]
    B --> E[Health Endpoints]
    
    subgraph "Message Types"
        F[Access Checks]
        G@{ shape: processes, label: "Resource Updates" }
        H@{ shape: processes, label: "Resource Deletions" }
    end
    
    F --> A
    G --> A
    H --> A
```

### Components

- **Access Check Handler**: Processes authorization queries with intelligent caching
- **Resource Handlers**: Manages resource permission synchronization: updates, deletes
(e.g. `handler_project.go` for project permission data)
- **Cache Layer**: JetStream KeyValue store for high-performance relationship caching

## 🛠️ Quick Start

### Prerequisites

- Go 1.23+
- Kubernetes 1.19+
- Helm 3.2.0+
- Docker (optional)

Dependencies you need but should get from [lfx-v2-helm](https://github.com/linuxfoundation/lfx-v2-helm/blob/main/charts/lfx-platform/README.md):

- NATS Server with JetStream enabled
- OpenFGA Server

### Local Development

1. **Clone the repository**:

   ```bash
   git clone https://github.com/linuxfoundation/lfx-v2-fga-sync.git
   cd lfx-v2-fga-sync
   ```

2. **Install dependencies**:

   ```bash
   # Installs Go dependencies
   make deps
   ```

   IMPORTANT: Install the lfx-platform Helm chart to get all of the dependencies running in a Kubernetes cluster.
   Follow the instructions from [lfx-v2-helm](https://github.com/linuxfoundation/lfx-v2-helm/blob/main/charts/lfx-platform/README.md).
   It is expected that you have the chart installed for the rest of the steps.

3. **Set up OpenFGA store and authorization model**:

    You should already have the OpenFGA store and authorization model configured if you are running the lfx-platform
    helm chart. Read more about the use of OpenFGA and ensuring that you have it configured:
    <https://github.com/linuxfoundation/lfx-v2-helm/blob/main/docs/openfga.md>

    If you are running your own instance of OpenFGA locally, you need to create a store and then an authorization model
    with the same content from
    <https://github.com/linuxfoundation/lfx-v2-helm/blob/main/charts/lfx-platform/templates/openfga/model.yaml>.
    The authorization model expected by this service is maintained there.

4. **Set environment variables**:

   ```bash
   # This assumes you have the lfx-platform chart running
   # from https://github.com/linuxfoundation/lfx-v2-helm/tree/main
   export NATS_URL="nats://lfx-platform-nats.lfx.svc.cluster.local:4222"
   export OPENFGA_API_URL="http://lfx-platform-openfga.lfx.svc.cluster.local:8080"
   export OPENFGA_STORE_ID="01K1GTJZW163H839J3YZHD8ZRY"  # Use your actual store ID if you aren't using the lfx-platform chart
   export OPENFGA_AUTH_MODEL_ID="01K1H4TFHDSBCZVZ5EP6HHDWE6"   # Use your actual model ID if you aren't using the lfx-platform chart
   export CACHE_BUCKET="fga-sync-cache"
   export USE_CACHE=true
   export DEBUG=false
   ```

5. **Create the NATS KeyValue cache bucket**:

   ```bash
   # Using NATS CLI (if available)
   nats kv add fga-sync-cache --history=20 --storage=file --max-value-size=10485760 --max-bucket-size=1073741824

   # Or using kubectl if running in Kubernetes
   kubectl exec -n lfx deploy/nats-box -- nats kv add fga-sync-cache --history=20 --storage=file --max-value-size=10485760 --max-bucket-size=1073741824 --ttl=3h
   ```

6. **Run the service**:

   ```bash
   make run
   ```

### Docker Deployment

```bash
# Build the image (replace the version as needed)
docker build -t linuxfoundation/lfx-v2-fga-sync:0.1.0 .

# Or use Make
make docker-build

# Run the container
docker run -d \
  -e NATS_URL=nats://lfx-platform-nats.lfx.svc.cluster.local:4222 \
  -e OPENFGA_API_URL=http://lfx-platform-openfga.lfx.svc.cluster.local:8080 \
  -e OPENFGA_STORE_ID=01K1GTJZW163H839J3YZHD8ZRY \
  -e OPENFGA_AUTH_MODEL_ID=01K1H4TFHDSBCZVZ5EP6HHDWE6 \
  -e CACHE_BUCKET=fga-sync-cache \
  -p 8080:8080 \
  linuxfoundation/lfx-v2-fga-sync:latest
```

### Kubernetes Deployment

```bash
# Deploy using Helm
helm install lfx-v2-fga-sync ./charts/lfx-v2-fga-sync -n lfx

# Or use Make
make helm-install

# Optionally deploy with custom local values (values.local.yaml) instead
# Create a values.local.yaml file in charts/lfx-v2-fga-sync/ with your custom values
make helm-install-local
```

## 🔧 Configuration

### Environment Variables

| Variable | Description | Default | Required |
|----------|-------------|---------|----------|
| `NATS_URL` | NATS server connection URL | `nats://localhost:4222` | Yes |
| `OPENFGA_API_URL` | OpenFGA API endpoint | - | Yes |
| `OPENFGA_STORE_ID` | OpenFGA store ID | - | Yes |
| `OPENFGA_AUTH_MODEL_ID` | OpenFGA authorization model ID | - | Yes |
| `CACHE_BUCKET` | JetStream KeyValue bucket name | `fga-sync-cache` | No |
| `USE_CACHE` | Whether to try to use cache for access checks | `false` | No |
| `PORT` | HTTP server port | `8080` | No |
| `DEBUG` | Enable debug logging | `false` | No |

Note: if you are developing locally and are writing to the OpenFGA store outside of this service
(e.g. granting certain access to a test user manually) then you should set `USE_CACHE=false`,
because otherwise access checks will use the cached access tuples even though they are out of date.

### NATS Subjects

The service subscribes to these NATS subjects:

- `lfx.access_check.request` - Access permission checks
- `lfx.update_access.project` - Project permission updates  
- `lfx.delete_all_access.project` - Project permission deletion (project deleted)

Follow this convention for other resources that have permissions in OpenFGA:

`lfx.update_access.<resource_type>` - Resource permission updates
`lfx.delete_all_access.<resource_type>` - Resource permission deletion (resource deleted)

## 📊 API Reference

### Health Endpoints

#### Liveness Probe

```http
GET /livez
```

Returns `200 OK` if the service is running.

#### Readiness Probe  

```http
GET /readyz
```

Returns `200 OK` if the service is ready to handle requests (NATS connected).

### Message Formats

#### Access Check Request

`lfx.access_check.request`

Format: `object#relation@user`

```text
project:7cad5a8d-19d0-41a4-81a6-043453daf9ee#viewer@user:456
```

#### Resource Update Message

`lfx.update_access.<resource_type>`

Format: this is dependent on the resource since each resource can have its own schema and relations.

Below is an example of the project message schema.

```json
{
  "uid": "7cad5a8d-19d0-41a4-81a6-043453daf9ee",
  "public": true,
  "parent_uid": "7cad5a8d-19d0-41a4-81a6-043453daf9ef7cad5a8d-19d0-41a4-81a6-043453daf9ee", 
  "writers": ["user1", "user2"],
  "auditors": ["auditor1"],
  "meeting_coordinators": ["coordinator1", "coordinator2"]
}
```

#### Resource Delete Message

`lfx.delete_all_access.<resource_type>`

Format: `<resource_uid>` i.e. the resource UID that was deleted so that all access on the resource can be cleaned up.

```text
7cad5a8d-19d0-41a4-81a6-043453daf9ee
```

## 🧪 Development

### Running Tests

```bash
# Run all tests
make test

# Run tests with coverage
make test-coverage

# Run specific test
go test -v ./... -run TestAccessCheckHandler
```

### Code Quality

```bash
# Format code
make fmt

# Run linter
make lint

# Run security checks
make vet

# Run all quality checks
make check
```

### Building

```bash
# Build for current platform
make build

# Build for multiple platforms
make build-all

# Build development version (with debug symbols)
make dev
```

## 📈 Performance

- **Throughput**: 10,000+ relationship checks per second
- **Latency**: Sub-millisecond response times with cache hits
- **Cache Hit Rate**: >95% for typical workloads
- **Memory Usage**: ~64MB baseline, scales with cache size

### Caching Strategy

1. **Cache Key Format**: `rel.{base32-encoded-relation}`
2. **Cache Invalidation**: Timestamp-based with automatic cleanup
3. **Cache TTL**: Configurable via JetStream bucket settings
4. **Fallback**: Direct OpenFGA queries on cache miss

## 🛡️ Security

- **Principle of Least Privilege**: Runs as non-root user (nobody)
- **Read-only Filesystem**: Container uses read-only root filesystem
- **Security Scanning**: Automated vulnerability scanning with Trivy and Gosec
- **Secret Management**: No secrets stored in code or environment variables

## 🔍 Monitoring

### Metrics

The service exposes metrics via expvar at `/debug/vars`:

- `cache_hits` - Number of successful cache lookups
- `cache_stale_hits` - Number of stale cache entries used
- `cache_misses` - Number of cache misses requiring OpenFGA queries

### Logging

Structured JSON logging with configurable levels:

```bash
# Enable debug logging
export DEBUG=true

# View logs in development
make run 2>&1 | jq '.'
```

## 🚢 Deployment

### Helm Chart

The repository includes a production-ready Helm chart:

```yaml
# values.yaml
application:
  replicas: 3
  resources:
    requests:
      memory: "64Mi"
      cpu: "100m"
    limits:
      memory: "128Mi" 
      cpu: "500m"

nats:
  url: "nats://lfx-platform-nats.lfx.svc.cluster.local:4222"

fga:
  apiUrl: "http://lfx-platform-openfga.lfx.svc.cluster.local:8080"
```

### Production Considerations

- **Horizontal Scaling**: Multiple replicas supported with NATS queue groups
- **Resource Limits**: Configure appropriate CPU/memory limits
- **Network Policies**: Restrict traffic to NATS and OpenFGA only
- **Monitoring**: Set up alerts for cache hit rates and error rates

## Releases

### Creating a Release

To create a new release of the project service:

1. **Update the chart version** in `charts/lfx-v2-fga-sync/Chart.yaml` prior to any project releases, or if any
   change is made to the chart manifests or configuration:

   ```yaml
   version: 0.2.0  # Increment this version
   appVersion: "latest"  # Keep this as "latest"
   ```

2. **After the pull request is merged**, create a GitHub release and choose the
   option for GitHub to also tag the repository. The tag must follow the format
   `v{version}` (e.g., `v0.2.0`). This tag does _not_ have to match the chart
   version: it is the version for the project release, which will dynamically
   update the `appVersion` in the released chart.

3. **The GitHub Actions workflow will automatically**:
   - Build and publish the container images
   - Package and publish the Helm chart to GitHub Pages
   - Publish the chart to GitHub Container Registry (GHCR)
   - Sign the chart with Cosign
   - Generate SLSA provenance

### Important Notes

- The `appVersion` in `Chart.yaml` should always remain `"latest"` in the committed code.
- During the release process, the `ko-build-tag.yaml` workflow automatically overrides the `appVersion` with the actual
tag version (e.g., `v0.2.0` becomes `0.2.0`).
- Only update the chart `version` field when making releases - this represents the Helm chart version.
- The container image tags are automatically managed by the consolidated CI/CD pipeline using the git tag.
- Both container images and the Helm chart are published together in a single workflow.

## 🤝 Contributing

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/amazing-feature`)
3. Make your changes
4. Run tests (`make test`)
5. Run quality checks (`make check`)
6. Commit your changes to a feature branch in your fork. Ensure your commits
   are signed with the [Developer Certificate of Origin (DCO)](https://developercertificate.org/).
   You can use the `git commit -s` command to sign your commits.
7. Ensure the chart version in `charts/lfx-v2-fga-sync/Chart.yaml` has been
   updated following semantic version conventions if you are making changes to the chart.
8. Push to the branch (`git push origin feature/amazing-feature`)
9. Open a Pull Request

### Code Standards

- Follow Go best practices and idioms
- Use structured logging with appropriate levels
- Include comprehensive error handling
- Update documentation for new features
- Add unit tests for features

## 📄 License

<!-- markdown-link-check-disable -->
This project is licensed under the MIT License - see the [LICENSE](/LICENSE) file for details.
