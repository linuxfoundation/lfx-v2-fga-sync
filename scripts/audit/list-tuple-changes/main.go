// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// list-tuple-changes is a CLI tool for inspecting recent OpenFGA tuple changes.
//
// Usage:
//
//	go run ./scripts/audit/list-tuple-changes [flags]
//
// Flags:
//
//	-since duration   Show changes from the last duration (default: 1h). Examples: 30m, 2h, 24h
//	-type string      Filter by object type (e.g. project, committee, meeting). Empty = all types.
//	-all-pages        Fetch all pages of results (default: false, stops after first page)
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	. "github.com/openfga/go-sdk/client"
)

func main() {
	since := flag.Duration("since", time.Hour, "Show changes from the last duration (e.g. 30m, 2h, 24h)")
	objectType := flag.String("type", "", "Filter by object type (e.g. project, committee, meeting). Empty = all types.")
	allPages := flag.Bool("all-pages", false, "Fetch all pages of results")
	flag.Parse()

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
	startTime := time.Now().Add(-*since)

	fmt.Printf("Fetching tuple changes since %s", startTime.Format(time.RFC3339))
	if *objectType != "" {
		fmt.Printf(" (type: %s)", *objectType)
	}
	fmt.Println()
	fmt.Println()

	body := ClientReadChangesRequest{
		StartTime: startTime,
	}
	if *objectType != "" {
		body.Type = *objectType
	}

	pageSizeInt32 := int32(100)
	options := ClientReadChangesOptions{
		PageSize: &pageSizeInt32,
	}

	totalChanges := 0
	page := 0
	var prevToken string

	for {
		page++
		resp, err := client.ReadChanges(ctx).Body(body).Options(options).Execute()
		if err != nil {
			log.Fatalf("failed to read changes (page %d): %v", page, err)
		}

		for _, change := range resp.Changes {
			totalChanges++
			fmt.Printf("[%s] %-8s  %s#%s@%s\n",
				change.Timestamp.Local().Format("2006-01-02 15:04:05"),
				change.Operation,
				change.TupleKey.Object,
				change.TupleKey.Relation,
				change.TupleKey.User,
			)
		}

		nextToken := ""
		if resp.ContinuationToken != nil {
			nextToken = *resp.ContinuationToken
		}

		// OpenFGA returns the same token when there are no more changes.
		if !*allPages || nextToken == "" || nextToken == prevToken {
			break
		}
		prevToken = nextToken
		options.ContinuationToken = &nextToken
	}

	fmt.Printf("\nTotal changes: %d\n", totalChanges)
}
