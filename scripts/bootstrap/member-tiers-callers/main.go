// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// member-tiers-callers grants the three M2M clients that call the
// GET /b2b_orgs/member-tiers/{username} endpoint the `member` relation on
// team:member_tiers_caller in OpenFGA.
//
// Run once per environment after the Auth0 M2M clients are created and their
// client IDs are known.
//
// Usage:
//
//	go run ./scripts/bootstrap/member-tiers-callers [flags]
//
// Required flags:
//
//	-worker-client-id     Auth0 client ID of the Insights Tiers Worker (off-cluster Cloudflare worker)
//	-pat-service-client-id Auth0 client ID of the LFX V2 PAT Service (on-cluster)
//	-lfx-one-client-id    Auth0 client ID of the LFX One gateway M2M client (M2M_AUTH_CLIENT_ID from lfx-self-serve)
//
// Required env vars:
//
//	OPENFGA_API_URL        OpenFGA API endpoint (e.g. http://localhost:8080)
//	OPENFGA_STORE_ID       OpenFGA store ID
//	OPENFGA_AUTH_MODEL_ID  OpenFGA authorization model ID
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	openfga "github.com/openfga/go-sdk"
	. "github.com/openfga/go-sdk/client"
)

const (
	teamMemberTiersCaller = "team:member_tiers_caller"
	relationMember        = "member"
)

func main() {
	workerClientID := flag.String("worker-client-id", "", "Auth0 client ID of the Insights Tiers Worker (required)")
	patServiceClientID := flag.String("pat-service-client-id", "", "Auth0 client ID of the LFX V2 PAT Service (required)")
	lfxOneClientID := flag.String("lfx-one-client-id", "", "Auth0 client ID of the LFX One gateway M2M client (required)")
	dryRun := flag.Bool("dry-run", false, "Print tuples that would be written without writing them")
	flag.Parse()

	if *workerClientID == "" || *patServiceClientID == "" || *lfxOneClientID == "" {
		fmt.Fprintln(os.Stderr, "all three -worker-client-id, -pat-service-client-id, and -lfx-one-client-id flags are required")
		flag.Usage()
		os.Exit(1)
	}

	fgaURL := os.Getenv("OPENFGA_API_URL")
	fgaStoreID := os.Getenv("OPENFGA_STORE_ID")
	fgaAuthModelID := os.Getenv("OPENFGA_AUTH_MODEL_ID")

	if fgaURL == "" {
		log.Fatal("OPENFGA_API_URL environment variable is required")
	}
	if fgaStoreID == "" {
		log.Fatal("OPENFGA_STORE_ID environment variable is required")
	}
	if fgaAuthModelID == "" {
		log.Fatal("OPENFGA_AUTH_MODEL_ID environment variable is required")
	}

	tuples := []openfga.TupleKey{
		{
			User:     fmt.Sprintf("user:%s@clients", *workerClientID),
			Relation: relationMember,
			Object:   teamMemberTiersCaller,
		},
		{
			User:     fmt.Sprintf("user:%s@clients", *patServiceClientID),
			Relation: relationMember,
			Object:   teamMemberTiersCaller,
		},
		{
			User:     fmt.Sprintf("user:%s@clients", *lfxOneClientID),
			Relation: relationMember,
			Object:   teamMemberTiersCaller,
		},
	}

	fmt.Println("Tuples to write:")
	for _, t := range tuples {
		fmt.Printf("  %s  %s  %s\n", t.User, t.Relation, t.Object)
	}
	fmt.Println()

	if *dryRun {
		fmt.Println("Dry run — no tuples written.")
		return
	}

	client, err := NewSdkClient(&ClientConfiguration{
		ApiUrl:               fgaURL,
		StoreId:              fgaStoreID,
		AuthorizationModelId: fgaAuthModelID,
		HTTPClient:           &http.Client{Timeout: 30 * time.Second},
	})
	if err != nil {
		log.Fatalf("failed to create OpenFGA client: %v", err)
	}

	ctx := context.Background()

	_, err = client.WriteTuples(ctx).Body(tuples).Execute()
	if err != nil {
		log.Fatalf("failed to write tuples: %v", err)
	}

	fmt.Printf("Successfully wrote %d tuple(s) to %s (store: %s)\n", len(tuples), fgaURL, fgaStoreID)
}
