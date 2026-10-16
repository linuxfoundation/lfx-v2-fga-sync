// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"os"
	"testing"

	openfga "github.com/openfga/go-sdk"
	"github.com/openfga/language/pkg/go/transformer"
	"github.com/stretchr/testify/require"
)

// parseModelDSL converts OpenFGA DSL into the SDK model the service reads from
// the API at runtime.
func parseModelDSL(t *testing.T, dsl string) *openfga.AuthorizationModel {
	t.Helper()
	modelJSON, err := transformer.TransformDSLToJSON(dsl)
	require.NoError(t, err)
	var model openfga.AuthorizationModel
	require.NoError(t, json.Unmarshal([]byte(modelJSON), &model))
	return &model
}

// graphFromDSL builds a modelGraph from DSL.
func graphFromDSL(t *testing.T, dsl string) *modelGraph {
	t.Helper()
	g, err := newModelGraph(parseModelDSL(t, dsl))
	require.NoError(t, err)
	return g
}

// platformModelDSL returns the snapshot of the production authorization model
// in testdata/model.fga.
func platformModelDSL(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("testdata/model.fga")
	require.NoError(t, err)
	return string(data)
}

// platformGraph is the modelGraph for the production model snapshot.
func platformGraph(t *testing.T) *modelGraph {
	t.Helper()
	return graphFromDSL(t, platformModelDSL(t))
}
