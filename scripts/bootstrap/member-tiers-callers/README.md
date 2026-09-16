# member-tiers-callers bootstrap

One-time script that grants the three M2M clients for the Public Insights API
the `member` relation on `team:member_tiers_caller` in OpenFGA.

The `member_tiers_caller` team gates the Heimdall-protected
`GET /b2b_orgs/member-tiers/{username}` endpoint on `lfx-v2-member-service`.
Callers must be members of this team; they do **not** need `global_org_admin`.

## Callers

| Client | Auth0 name | Direction |
|--------|-----------|-----------|
| Insights Tiers Service | `Insights Tiers Service` | off-cluster (Cloudflare) |
| LFX V2 PAT Service | `LFX V2 PAT Service` | on-cluster |
| LFX One gateway | `auth0_client.lfx_one` in auth0-terraform (`M2M_AUTH_CLIENT_ID` in lfx-self-serve) | on-cluster |

## Getting the client IDs

- **Insights Tiers Service** and **LFX V2 PAT Service**: available after `terraform apply` in auth0-terraform. Read from state with `terraform workspace select <env> && terraform state show auth0_client.insights_tiers_service` (Insights Tiers Service) and `terraform state show 'auth0_client.m2m_clients["LFX V2 PAT Service"]'` (PAT Service).
- **LFX One**: already managed as `auth0_client.lfx_one` in auth0-terraform. Read with `terraform state show auth0_client.lfx_one`. Dev value also in `lfx-self-serve/apps/lfx-one/.env` as `M2M_AUTH_CLIENT_ID`.

## Usage

Run once per environment after Terraform has applied the two new M2M clients.

```bash
export OPENFGA_API_URL="http://lfx-platform-openfga.lfx.svc.cluster.local:8080"
export OPENFGA_STORE_ID="<store-id>"
export OPENFGA_AUTH_MODEL_ID="<model-id>"
export NATS_URL="nats://lfx-platform-nats.lfx.svc.cluster.local:4222"
# Optional — must match the fga-sync deployment's CACHE_BUCKET / nats.cacheFgaKvBucket.name.
# Defaults to "fga-sync-cache" if unset.
# export CACHE_BUCKET="fga-sync-cache"

go run ./scripts/bootstrap/member-tiers-callers \
  -worker-client-id      <insights-tiers-service-client-id> \
  -pat-service-client-id <lfx-v2-pat-service-client-id> \
  -lfx-one-client-id     <lfx-one-m2m-client-id>
```

After writing the tuples the script bumps the fga-sync JetStream cache `inv` key so any cached denial is immediately superseded.

Use `-dry-run` to print the tuples without writing them (no env vars required).
