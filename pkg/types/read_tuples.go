// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package types contains shared message types for the fga-sync service.
package types

// ReadTuplesRequest is the JSON payload received over NATS for the
// lfx.access_check.read_tuples subject.
type ReadTuplesRequest struct {
	User       string `json:"user"`
	ObjectType string `json:"object_type"`
}

// ReadTuplesResponse is the JSON response sent back over NATS for the
// lfx.access_check.read_tuples subject. Results are tuple-strings in
// the canonical object#relation@user format. Error is set on failure.
type ReadTuplesResponse struct {
	Results []string `json:"results,omitempty"`
	Error   string   `json:"error,omitempty"`
}
