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
	"time"

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
	// itemCount forces multiple ClientMaxBatchSize (50) chunks: comfortably
	// more than 200 so at least 5 chunks fire, letting an uncapped/default
	// execution (SDK default MaxParallelRequests of 10) exceed
	// batchCheckMaxParallelRequests (4) and fail the assertion below. A
	// smaller item count (e.g. 101, three chunks) could pass vacuously even
	// with the parallelism cap silently dropped.
	const itemCount = 260

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

		// Hold the request open briefly so concurrent chunks overlap in
		// time; an immediate response could let requests complete
		// sequentially even without a parallelism cap, making the
		// maxInFlight assertion below pass vacuously.
		time.Sleep(20 * time.Millisecond)

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
	require.Greaterf(t, requestCount, int(batchCheckMaxParallelRequests),
		"expected the SDK to chunk %d items into more than batchCheckMaxParallelRequests (%d) requests, got %d",
		itemCount, batchCheckMaxParallelRequests, requestCount)
	require.Greaterf(t, maxInFlight, 1,
		"requests never overlapped (maxInFlight=%d); the parallelism assertion below would pass vacuously", maxInFlight)
	require.LessOrEqualf(t, maxInFlight, int(batchCheckMaxParallelRequests),
		"BatchCheck fanned out %d concurrent requests, want at most batchCheckMaxParallelRequests (%d)",
		maxInFlight, batchCheckMaxParallelRequests)
}

// TestFgaHTTPClientHasConfiguredTimeout asserts the *http.Client connectFga
// hands to the OpenFGA SDK carries fgaHTTPTimeout, so a future edit that
// drops the Timeout (e.g. while touching the transport/instrumentation
// wiring) fails a test instead of silently leaving outbound OpenFGA calls
// unbounded.
func TestFgaHTTPClientHasConfiguredTimeout(t *testing.T) {
	client := fgaHTTPClient()
	require.Equal(t, fgaHTTPTimeout, client.Timeout)
}

// TestConnectFgaWiresConfiguredTimeout asserts connectFga itself - not just
// fgaHTTPClient in isolation - hands the OpenFGA SDK a client carrying
// fgaHTTPTimeout. connectFga is the function that actually wires a client
// into production use (fga.go); a future refactor that builds the
// *http.Client inline again, bypassing fgaHTTPClient, would otherwise
// silently drop the timeout without failing the suite.
func TestConnectFgaWiresConfiguredTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	t.Setenv("OPENFGA_API_URL", server.URL)
	t.Setenv("OPENFGA_STORE_ID", "01GXSB9YR785C4FYS3C0RTG7B2")
	t.Setenv("OPENFGA_AUTH_MODEL_ID", "01GXSA8YR785C4FYS3C0RTG7B1")

	fgaClient, err := connectFga()
	require.NoError(t, err)

	adapter, ok := fgaClient.(FgaAdapter)
	require.True(t, ok, "connectFga did not return a FgaAdapter")
	require.Equal(t, fgaHTTPTimeout, adapter.GetConfig().HTTPClient.Timeout)
}

// TestFgaHTTPClientEnforcesTimeout observes the configured timeout actually
// aborting a slow call, rather than only asserting the client's Timeout
// field is set correctly.
func TestFgaHTTPClientEnforcesTimeout(t *testing.T) {
	blockUntilCanceled := make(chan struct{})
	defer close(blockUntilCanceled)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-blockUntilCanceled:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()

	client := fgaHTTPClient()
	client.Timeout = 50 * time.Millisecond

	start := time.Now()
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	require.NoError(t, err)

	_, err = client.Do(req)
	require.Error(t, err, "expected the client's Timeout to abort a call to a server that never responds")
	require.Lessf(t, time.Since(start), 5*time.Second,
		"call took %s to time out, want it bounded by the client's short test Timeout", time.Since(start))
}
