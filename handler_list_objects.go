// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package main provides the fga-sync service entry point and supporting types.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/types"
)

// listObjectsTimeout is the maximum time allowed for the batch of OpenFGA
// ListObjects calls.
const listObjectsTimeout = 10 * time.Second

// listObjectsHandler handles requests to resolve, for a batch of
// (object_type, relation) queries, which objects a user holds each relation
// on. It responds with a JSON-encoded ListObjectsResponse where each target
// is annotated with the artifact types creatable on it.
func (h *HandlerService) listObjectsHandler(ctx context.Context, message INatsMsg) error {
	ctx, cancel := context.WithTimeout(ctx, listObjectsTimeout)
	defer cancel()

	// Unmarshal the JSON request payload.
	var req types.ListObjectsRequest
	if err := json.Unmarshal(message.Data(), &req); err != nil {
		logger.With(errKey, err).WarnContext(ctx, "failed to unmarshal list objects request")
		return h.respondListObjectsError(ctx, message, "invalid request payload")
	}

	if req.User == "" || len(req.Queries) == 0 {
		logger.With(
			"user", req.User,
			"query_count", len(req.Queries),
		).WarnContext(ctx, "list objects request missing required fields")
		return h.respondListObjectsError(ctx, message, "user and queries are required")
	}

	for _, q := range req.Queries {
		if q.ObjectType == "" || q.Relation == "" {
			logger.With(
				"object_type", q.ObjectType,
				"relation", q.Relation,
			).WarnContext(ctx, "list objects request has an incomplete query")
			return h.respondListObjectsError(ctx, message, "object_type and relation are required for each query")
		}
		if strings.Contains(q.ObjectType, ":") {
			logger.With("object_type", q.ObjectType).WarnContext(ctx, "list objects request contains invalid object_type")
			return h.respondListObjectsError(ctx, message, "object_type must not contain ':'")
		}
	}

	logger.With(
		"user", req.User,
		"query_count", len(req.Queries),
	).InfoContext(ctx, "handling list objects request")

	// Resolve each (object_type, relation) query and annotate results with
	// their creatable artifact types.
	targets := make([]types.ListObjectsTarget, 0)
	for _, q := range req.Queries {
		objects, err := h.fgaService.ListObjectsCached(ctx, q.ObjectType, q.Relation, req.User)
		if err != nil {
			logger.With(
				errKey, err,
				"user", req.User,
				"object_type", q.ObjectType,
				"relation", q.Relation,
			).ErrorContext(ctx, "failed to list objects")
			return h.respondListObjectsError(ctx, message, "failed to list objects")
		}

		creatableTypes := types.CreatableTypesFor(q.ObjectType, q.Relation)
		for _, object := range objects {
			targets = append(targets, types.ListObjectsTarget{
				Object:         object,
				ObjectType:     q.ObjectType,
				Relation:       q.Relation,
				CreatableTypes: creatableTypes,
			})
		}
	}

	resp := types.ListObjectsResponse{Targets: targets}
	data, err := json.Marshal(resp)
	if err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "failed to marshal list objects response")
		return h.respondListObjectsError(ctx, message, "failed to marshal response")
	}

	if message.Reply() != "" {
		if errRespond := message.Respond(data); errRespond != nil {
			logger.With(errKey, errRespond).WarnContext(ctx, "failed to send list objects reply")
			return errRespond
		}
		logger.With(
			"user", req.User,
			"query_count", len(req.Queries),
			"target_count", len(targets),
		).InfoContext(ctx, "sent list objects response")
	}

	return nil
}

// respondListObjectsError sends a JSON error response over NATS and returns a
// formatted error so the subscription loop can log it consistently with other
// handlers. This helper does not log — callers are responsible for logging
// before calling it.
func (h *HandlerService) respondListObjectsError(_ context.Context, message INatsMsg, errMsg string) error {
	if message.Reply() != "" {
		resp := types.ListObjectsResponse{Error: errMsg}
		data, err := json.Marshal(resp)
		if err != nil {
			return fmt.Errorf("list objects: %s (marshal error response: %w)", errMsg, err)
		}
		if errRespond := message.Respond(data); errRespond != nil {
			return fmt.Errorf("list objects: %s (send error reply: %w)", errMsg, errRespond)
		}
	}
	return fmt.Errorf("list objects: %s", errMsg)
}
