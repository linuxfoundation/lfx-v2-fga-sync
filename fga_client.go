// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"

	openfga "github.com/openfga/go-sdk"

	. "github.com/openfga/go-sdk/client"
)

// IFgaClient is an interface for OpenFGA client operations used in this service.
type IFgaClient interface {
	Read(ctx context.Context, req ClientReadRequest, options ClientReadOptions) (*ClientReadResponse, error)
	Write(ctx context.Context, req ClientWriteRequest) (*ClientWriteResponse, error)
	BatchCheck(ctx context.Context, request ClientBatchCheckRequest) (*openfga.BatchCheckResponse, error)
	ListObjects(
		ctx context.Context,
		body ClientListObjectsRequest,
		options ClientListObjectsOptions,
	) (*ClientListObjectsResponse, error)
}

// FgaClient is a wrapper around the OpenFGA client.
type FgaAdapter struct {
	OpenFgaClient
}

// BatchCheck executes a batch check request.
func (c FgaAdapter) BatchCheck(
	ctx context.Context,
	request ClientBatchCheckRequest,
) (*openfga.BatchCheckResponse, error) {
	return c.OpenFgaClient.BatchCheck(ctx).Body(request).Execute()
}

// Read executes a read request.
func (c FgaAdapter) Read(
	ctx context.Context,
	req ClientReadRequest,
	options ClientReadOptions,
) (*ClientReadResponse, error) {
	return c.OpenFgaClient.Read(ctx).Body(req).Options(options).Execute()
}

// Write executes a write request.
func (c FgaAdapter) Write(
	ctx context.Context,
	req ClientWriteRequest,
) (*ClientWriteResponse, error) {
	return c.OpenFgaClient.Write(ctx).Body(req).Execute()
}

// ListObjects executes a list objects request.
func (c FgaAdapter) ListObjects(
	ctx context.Context,
	body ClientListObjectsRequest,
	options ClientListObjectsOptions,
) (*ClientListObjectsResponse, error) {
	return c.OpenFgaClient.ListObjects(ctx).Body(body).Options(options).Execute()
}
