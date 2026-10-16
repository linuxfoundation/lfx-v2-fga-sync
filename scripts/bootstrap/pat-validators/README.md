<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# pat-validators bootstrap

One-time script that grants the tenant's `Auth0 Actions` M2M client the `member` relation on
`team:pat_validator` in OpenFGA.

The `pat_validator` team gates the Heimdall-protected `POST /_tokens/validate` endpoint on
`lfx-v2-pat-service`. Its only caller is the Auth0 PAT custom token exchange action
(`lfx_pat_token_exchange` in auth0-terraform), which authenticates as `Auth0 Actions`
(`auth0_client.m2m_clients["Auth0 Actions"]`). Until the tuple exists, every token exchange fails.

## Tuple

```text
user:<actions-client-id>@clients  member  team:pat_validator
```

## Getting the values

- **Auth0 Actions client ID**: Auth0 dashboard for the environment's tenant, Applications, `Auth0 Actions`,
  Client ID. Each tenant has its own.
- **OpenFGA store and model IDs**: the ones Heimdall checks against, read from its deployment:

```bash
kubectl -n lfx get deploy lfx-platform-heimdall \
  -o jsonpath='{range .spec.template.spec.containers[0].env[?(@.name=="OPENFGA_STORE_ID")]}{.value}{"\n"}{end}'
kubectl -n lfx get deploy lfx-platform-heimdall \
  -o jsonpath='{range .spec.template.spec.containers[0].env[?(@.name=="OPENFGA_AUTH_MODEL_ID")]}{.value}{"\n"}{end}'
```

## Usage

Run once per environment, with `kubectl` pointed at that environment's cluster:

```bash
kubectl port-forward -n lfx svc/lfx-platform-openfga 8080:8080 &

export OPENFGA_API_URL="http://localhost:8080"
export OPENFGA_STORE_ID="<store-id>"
export OPENFGA_AUTH_MODEL_ID="<model-id>"

go run ./scripts/bootstrap/pat-validators -actions-client-id <actions-client-id> -dry-run
go run ./scripts/bootstrap/pat-validators -actions-client-id <actions-client-id>
```

After writing, the script runs the same OpenFGA check Heimdall runs and exits non-zero unless it is allowed.
Reruns are safe.

To roll back, pass `-remove`. The script deletes the tuple and confirms the check is denied.

Unlike `member-tiers-callers`, this script does not bump the fga-sync cache: Heimdall's `openfga_check`
authorizer calls the OpenFGA check API directly.
