// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// member-tiers-callers grants the two M2M clients that call the
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
//	-worker-client-id      Auth0 client ID of the Insights Tiers Service (off-cluster Cloudflare worker)
//	-lfx-one-client-id     Auth0 client ID of the LFX One gateway M2M client (M2M_AUTH_CLIENT_ID from lfx-self-serve)
//
// Required env vars (not needed for --dry-run):
//
//	OPENFGA_API_URL        OpenFGA API endpoint (e.g. http://lfx-platform-openfga.lfx.svc.cluster.local:8080)
//	OPENFGA_STORE_ID       OpenFGA store ID
//	OPENFGA_AUTH_MODEL_ID  OpenFGA authorization model ID
//	NATS_URL               NATS server URL (e.g. nats://lfx-platform-nats.lfx.svc.cluster.local:4222)
//	                       Used to bump the fga-sync cache invalidation key after writing tuples.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/nats-io/nats.go"
	. "github.com/openfga/go-sdk/client"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
)

const teamMemberTiersCaller = constants.ObjectTypeTeam + "member_tiers_caller"

// duplicateWriteIgnore instructs OpenFGA to treat writes of already-existing
// tuples as no-ops instead of errors, making reruns safe.
var duplicateWriteIgnore = ClientWriteOptions{
	Conflict: ClientWriteConflictOptions{
		OnDuplicateWrites: CLIENT_WRITE_REQUEST_ON_DUPLICATE_WRITES_IGNORE,
		OnMissingDeletes:  CLIENT_WRITE_REQUEST_ON_MISSING_DELETES_IGNORE,
	},
}

func main() {
	workerClientID := flag.String("worker-client-id", "", "Auth0 client ID of the Insights Tiers Service (required)")
	lfxOneClientID := flag.String(
		"lfx-one-client-id",
		"",
		"Auth0 client ID of the LFX One gateway M2M client "+
			"(M2M_AUTH_CLIENT_ID from lfx-self-serve) (required)",
	)
	dryRun := flag.Bool("dry-run", false, "Print tuples that would be written without writing them")
	flag.Parse()

	if *workerClientID == "" || *lfxOneClientID == "" {
		fmt.Fprintln(os.Stderr, "both -worker-client-id and -lfx-one-client-id flags are required")
		flag.Usage()
		os.Exit(1)
	}

	tuples := []ClientTupleKey{
		{
			User:     fmt.Sprintf("user:%s@clients", *workerClientID),
			Relation: constants.RelationMember,
			Object:   teamMemberTiersCaller,
		},
		{
			User:     fmt.Sprintf("user:%s@clients", *lfxOneClientID),
			Relation: constants.RelationMember,
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

	fgaURL := os.Getenv("OPENFGA_API_URL")
	fgaStoreID := os.Getenv("OPENFGA_STORE_ID")
	fgaAuthModelID := os.Getenv("OPENFGA_AUTH_MODEL_ID")
	natsURL := os.Getenv("NATS_URL")

	if fgaURL == "" {
		log.Fatal("OPENFGA_API_URL environment variable is required")
	}
	if fgaStoreID == "" {
		log.Fatal("OPENFGA_STORE_ID environment variable is required")
	}
	if fgaAuthModelID == "" {
		log.Fatal("OPENFGA_AUTH_MODEL_ID environment variable is required")
	}
	if natsURL == "" {
		log.Fatal("NATS_URL environment variable is required")
	}

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

	// A transport error from Execute() may fire after OpenFGA has already
	// committed the write. Always attempt cache invalidation regardless of the
	// write outcome so a committed-but-timed-out write does not leave a stale
	// denial in the KV cache.
	_, writeErr := fgaClient.Write(ctx).Body(ClientWriteRequest{Writes: tuples}).Options(duplicateWriteIgnore).Execute()
	if writeErr != nil {
		fmt.Fprintf(os.Stderr, "warning: write returned error (OpenFGA may have committed anyway): %v\n", writeErr)
	} else {
		fmt.Printf("Wrote %d tuple(s) to %s (store: %s)\n", len(tuples), fgaURL, fgaStoreID)
	}

	// Bump the fga-sync JetStream cache invalidation key so any denial cached
	// before this bootstrap run is immediately superseded. The JetStream KV
	// bucket is persistent — restarting fga-sync only rebinds it and does not
	// clear existing entries.
	nc, err := nats.Connect(natsURL)
	if err != nil {
		log.Fatalf("failed to connect to NATS for cache invalidation: %v", err)
	}
	defer nc.Close()

	js, err := nc.JetStream()
	if err != nil {
		log.Fatalf("failed to get JetStream context: %v", err)
	}

	cacheBucket := os.Getenv("CACHE_BUCKET")
	if cacheBucket == "" {
		cacheBucket = constants.KVBucketNameSyncCache
	}

	kv, err := js.KeyValue(cacheBucket)
	if err != nil {
		log.Fatalf("failed to bind to cache bucket %q: %v", cacheBucket, err)
	}

	if _, err = kv.Put("inv", []byte("1")); err != nil {
		log.Fatalf("failed to bump cache invalidation key: %v", err)
	}

	fmt.Println("Cache invalidation key bumped — fga-sync will revalidate cached denials.")

	if writeErr != nil {
		log.Fatalf("write error (cache was invalidated; rerun to confirm tuples exist): %v", writeErr)
	}
}
