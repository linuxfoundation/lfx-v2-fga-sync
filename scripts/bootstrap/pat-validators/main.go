// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// pat-validators grants the Auth0 Actions M2M client the `member` relation on
// team:pat_validator in OpenFGA.
//
// The Heimdall RuleSet of lfx-v2-pat-service admits POST /_tokens/validate only
// for members of this team. The caller is the Auth0 PAT custom token exchange
// action, which authenticates as the tenant's "Auth0 Actions" client.
//
// Run once per environment. Reruns are safe: an existing tuple is a no-op.
//
// Usage:
//
//	go run ./scripts/bootstrap/pat-validators [flags]
//
// Required flags:
//
//	-actions-client-id     Auth0 client ID of the tenant's "Auth0 Actions" M2M client
//
// Optional flags:
//
//	-dry-run               Print the tuple without writing it
//	-remove                Delete the tuple instead of writing it (rollback)
//
// Required env vars (not needed for -dry-run):
//
//	OPENFGA_API_URL        OpenFGA API endpoint (e.g. http://localhost:8080 through a port-forward)
//	OPENFGA_STORE_ID       OpenFGA store ID Heimdall checks against
//	OPENFGA_AUTH_MODEL_ID  OpenFGA authorization model ID Heimdall checks against
//
// Unlike member-tiers-callers, this script does not touch the fga-sync cache:
// Heimdall's openfga_check authorizer calls the OpenFGA check API directly, so
// no fga-sync cached decision covers this team.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"time"

	. "github.com/openfga/go-sdk/client"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
)

const teamPatValidator = constants.ObjectTypeTeam + "pat_validator"

// validClientID matches the character set of Auth0 client IDs, so a pasted
// principal such as "user:<id>@clients" is rejected instead of written.
var validClientID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// idempotentWrite treats writing an existing tuple, or deleting a missing one,
// as a no-op so reruns and rollbacks are safe.
var idempotentWrite = ClientWriteOptions{
	Conflict: ClientWriteConflictOptions{
		OnDuplicateWrites: CLIENT_WRITE_REQUEST_ON_DUPLICATE_WRITES_IGNORE,
		OnMissingDeletes:  CLIENT_WRITE_REQUEST_ON_MISSING_DELETES_IGNORE,
	},
}

func main() {
	actionsClientID := flag.String(
		"actions-client-id",
		"",
		`Auth0 client ID of the tenant's "Auth0 Actions" M2M client (required)`,
	)
	dryRun := flag.Bool("dry-run", false, "Print the tuple without writing it")
	remove := flag.Bool("remove", false, "Delete the tuple instead of writing it")
	flag.Parse()

	if !validClientID.MatchString(*actionsClientID) {
		fmt.Fprintln(os.Stderr, "-actions-client-id is required and must be a bare Auth0 client ID")
		flag.Usage()
		os.Exit(1)
	}

	tuple := ClientTupleKey{
		User:     fmt.Sprintf("user:%s@clients", *actionsClientID),
		Relation: constants.RelationMember,
		Object:   teamPatValidator,
	}
	verb := "write"
	if *remove {
		verb = "delete"
	}
	fmt.Printf("Tuple to %s:\n  %s  %s  %s\n\n", verb, tuple.User, tuple.Relation, tuple.Object)

	if *dryRun {
		fmt.Println("Dry run — nothing changed.")
		return
	}

	fgaURL := requireEnv("OPENFGA_API_URL")
	fgaStoreID := requireEnv("OPENFGA_STORE_ID")
	fgaAuthModelID := requireEnv("OPENFGA_AUTH_MODEL_ID")

	fgaClient, err := NewSdkClient(&ClientConfiguration{
		ApiUrl:               fgaURL,
		StoreId:              fgaStoreID,
		AuthorizationModelId: fgaAuthModelID,
		HTTPClient:           &http.Client{Timeout: 30 * time.Second},
	})
	if err != nil {
		log.Fatalf("failed to create OpenFGA client: %v", err)
	}

	ctx := context.Background()
	body := ClientWriteRequest{Writes: []ClientTupleKey{tuple}}
	if *remove {
		body = ClientWriteRequest{Deletes: []ClientTupleKeyWithoutCondition{
			{User: tuple.User, Relation: tuple.Relation, Object: tuple.Object},
		}}
	}
	if _, err = fgaClient.Write(ctx).Body(body).Options(idempotentWrite).Execute(); err != nil {
		log.Fatalf("%s failed: %v", verb, err)
	}

	// Run the same check Heimdall runs, so the result is confirmed rather than assumed.
	check, err := fgaClient.Check(ctx).Body(ClientCheckRequest{
		User:     tuple.User,
		Relation: tuple.Relation,
		Object:   tuple.Object,
	}).Execute()
	if err != nil {
		log.Fatalf("%s succeeded but the confirming check failed: %v", verb, err)
	}
	allowed := check.GetAllowed()
	fmt.Printf("Check %s#%s for %s: allowed=%t (store: %s)\n",
		tuple.Object, tuple.Relation, tuple.User, allowed, fgaStoreID)
	if allowed == *remove {
		log.Fatalf("unexpected check result after %s", verb)
	}
}

// requireEnv returns the named environment variable or exits when it is unset.
func requireEnv(name string) string {
	value := os.Getenv(name)
	if value == "" {
		log.Fatalf("%s environment variable is required", name)
	}
	return value
}
