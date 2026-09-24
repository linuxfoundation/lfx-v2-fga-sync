// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package main provides the fga-sync service entry point and supporting types.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	openfga "github.com/openfga/go-sdk"
	. "github.com/openfga/go-sdk/client"
	"github.com/stretchr/testify/require"
)

// TestFgaAdapterBatchCheckBoundsParallelism verifies BatchCheck pins the
// SDK's internal fan-out to batchCheckMaxParallelRequests instead of the
// SDK default (10), so one call cannot open more simultaneous outbound
// requests than the connection pool and handler concurrency were sized for.
func TestFgaAdapterBatchCheckBoundsParallelism(t *testing.T) {
	const storeID = "01GXSB9YR785C4FYS3C0RTG7B2"
	const itemCount = 101 // forces multiple ClientMaxBatchSize (50) chunks

	var (
		mu           sync.Mutex
		inFlight     int
		maxInFlight  int
		requestCount int
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inFlight++
		requestCount++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()

		defer func() {
			mu.Lock()
			inFlight--
			mu.Unlock()
		}()

		result := map[string]openfga.BatchCheckSingleResult{
			"1": {Allowed: openfga.PtrBool(true)},
		}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(openfga.BatchCheckResponse{Result: &result}))
	}))
	defer server.Close()

	sdkClient, err := NewSdkClient(&ClientConfiguration{
		ApiUrl:  server.URL,
		StoreId: storeID,
	})
	require.NoError(t, err)

	adapter := FgaAdapter{OpenFgaClient: *sdkClient}

	var checks []ClientBatchCheckItem
	for i := range itemCount {
		checks = append(checks, ClientBatchCheckItem{
			User:     fmt.Sprintf("user:%d", i),
			Relation: "viewer",
			Object:   fmt.Sprintf("project:%d", i),
		})
	}

	_, err = adapter.BatchCheck(context.Background(), ClientBatchCheckRequest{Checks: checks})
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Greater(t, requestCount, 1, "expected the SDK to chunk 101 items into multiple requests")
	require.LessOrEqualf(t, maxInFlight, int(batchCheckMaxParallelRequests),
		"BatchCheck fanned out %d concurrent requests, want at most batchCheckMaxParallelRequests (%d)",
		maxInFlight, batchCheckMaxParallelRequests)
}
