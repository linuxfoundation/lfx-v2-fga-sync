// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package types contains shared message types for the fga-sync service.
package types

// ListObjectsRequest is the JSON payload received over NATS for the
// lfx.access_check.list_objects subject.
type ListObjectsRequest struct {
	User    string             `json:"user"`
	Queries []ListObjectsQuery `json:"queries"`
}

// ListObjectsQuery is a single (object_type, relation) pair to resolve via
// OpenFGA ListObjects.
type ListObjectsQuery struct {
	ObjectType string `json:"object_type"`
	Relation   string `json:"relation"`
}

// ListObjectsTarget is a single object the user holds the given relation on,
// annotated with the artifact types creatable on it.
type ListObjectsTarget struct {
	Object         string   `json:"object"`
	ObjectType     string   `json:"object_type"`
	Relation       string   `json:"relation"`
	CreatableTypes []string `json:"creatable_types"`
}

// ListObjectsResponse is the JSON response sent back over NATS for the
// lfx.access_check.list_objects subject. Error is set on failure.
type ListObjectsResponse struct {
	Targets []ListObjectsTarget `json:"targets"`
	Error   string              `json:"error,omitempty"`
}
