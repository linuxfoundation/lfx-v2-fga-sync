# member-tiers-callers bootstrap

One-time script that grants the two M2M clients for the Public Insights API
the `member` relation on `team:member_tiers_caller` in OpenFGA.

The `member_tiers_caller` team gates the Heimdall-protected
`GET /b2b_orgs/member-tiers/{username}` endpoint on `lfx-v2-member-service`.
Callers must be members of this team; they do **not** need `global_org_admin`.

## Callers

| Client | Auth0 name | Direction |
|--------|-----------|-----------|
| Insights Tiers Service | `Insights Tiers Service` | off-cluster (Cloudflare) |
| LFX One gateway | `auth0_client.lfx_one` in auth0-terraform (`M2M_AUTH_CLIENT_ID` in lfx-self-serve) | on-cluster |

## Getting the values

### Auth0 client IDs

- **Insights Tiers Service**: available after `terraform apply` in auth0-terraform. Read from state with `terraform workspace select <env> && terraform state show auth0_client.insights_tiers_service`.
- **LFX One**: already managed as `auth0_client.lfx_one` in auth0-terraform. Read with `terraform state show auth0_client.lfx_one`. Dev value also in `lfx-self-serve/apps/lfx-one/.env` as `M2M_AUTH_CLIENT_ID`.

### OpenFGA store and model IDs

Read them from the Kubernetes resources managed by the fga-operator:

```bash
# Authorization model ID
kubectl get authorizationmodel lfx-core -n lfx -o jsonpath='{.spec.instances[0].id}'; echo

# Store ID
kubectl get store lfx-core -n lfx -o jsonpath='{.spec.id}'; echo
```

## Usage

Run once per environment after the required Auth0 clients exist and their client IDs are available.

When running from outside the cluster, port-forward the services first:

```bash
kubectl port-forward -n lfx svc/lfx-platform-openfga 8080:8080 &
kubectl port-forward -n lfx svc/lfx-platform-nats 4222:4222 &
```

Then use `localhost` URLs instead of in-cluster DNS:

```bash
export OPENFGA_API_URL="http://localhost:8080"
export OPENFGA_STORE_ID="<store-id>"
export OPENFGA_AUTH_MODEL_ID="<model-id>"
export NATS_URL="nats://localhost:4222"
# Optional — must match the fga-sync deployment's CACHE_BUCKET / nats.cacheFgaKvBucket.name.
# Defaults to "fga-sync-cache" if unset.
# export CACHE_BUCKET="fga-sync-cache"

go run ./scripts/bootstrap/member-tiers-callers \
  -worker-client-id      <insights-tiers-service-client-id> \
  -lfx-one-client-id     <lfx-one-m2m-client-id>
```

After writing the tuples the script bumps the fga-sync JetStream cache `inv` key so any cached denial is immediately superseded.

Use `-dry-run` to print the tuples without writing them (no env vars required).
