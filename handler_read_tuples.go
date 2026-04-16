// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// The fga-sync service.
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/types"
)

// readTuplesHandler handles requests to read a user's direct OpenFGA tuples for
// a given object type. It responds with a JSON-encoded ReadTuplesResponse
// containing tuple-strings in object#relation@user format.
func (h *HandlerService) readTuplesHandler(message INatsMsg) error {
	ctx := context.Background()

	// Unmarshal the JSON request payload.
	var req types.ReadTuplesRequest
	if err := json.Unmarshal(message.Data(), &req); err != nil {
		logger.With(errKey, err).WarnContext(ctx, "failed to unmarshal read tuples request")
		return h.respondReadTuplesError(message, fmt.Sprintf("failed to unmarshal request: %s", err))
	}

	if req.User == "" || req.ObjectType == "" {
		logger.WarnContext(ctx, "read tuples request missing required fields", "user", req.User, "object_type", req.ObjectType)
		return h.respondReadTuplesError(message, "user and object_type are required")
	}

	logger.With("user", req.User, "object_type", req.ObjectType).InfoContext(ctx, "handling read tuples request")

	// Query OpenFGA for all direct tuples matching the user + object type.
	tuples, err := h.fgaService.ReadUserTuples(ctx, req.User, req.ObjectType)
	if err != nil {
		logger.With(errKey, err, "user", req.User, "object_type", req.ObjectType).ErrorContext(ctx, "failed to read user tuples")
		return h.respondReadTuplesError(message, fmt.Sprintf("failed to read tuples: %s", err))
	}

	// Convert []openfga.Tuple to tuple-strings: object#relation@user.
	results := make([]string, 0, len(tuples))
	for _, t := range tuples {
		if t.Key.Object == "" || t.Key.Relation == "" || t.Key.User == "" {
			continue
		}
		results = append(results, fmt.Sprintf("%s#%s@%s", t.Key.Object, t.Key.Relation, t.Key.User))
	}

	resp := types.ReadTuplesResponse{Results: results}
	data, err := json.Marshal(resp)
	if err != nil {
		logger.With(errKey, err).ErrorContext(ctx, "failed to marshal read tuples response")
		return h.respondReadTuplesError(message, fmt.Sprintf("failed to marshal response: %s", err))
	}

	if message.Reply() != "" {
		if errRespond := message.Respond(data); errRespond != nil {
			logger.With(errKey, errRespond).WarnContext(ctx, "failed to send read tuples reply")
			return errRespond
		}
		logger.With("user", req.User, "object_type", req.ObjectType, "count", len(results)).InfoContext(ctx, "sent read tuples response")
	}

	return nil
}

// respondReadTuplesError sends a JSON error response and returns nil so the
// subscription loop does not log a redundant error.
func (h *HandlerService) respondReadTuplesError(message INatsMsg, errMsg string) error {
	if message.Reply() == "" {
		return nil
	}
	resp := types.ReadTuplesResponse{Error: errMsg}
	data, err := json.Marshal(resp)
	if err != nil {
		logger.With(errKey, err).ErrorContext(context.Background(), "failed to marshal error response")
		return err
	}
	if errRespond := message.Respond(data); errRespond != nil {
		logger.With(errKey, errRespond).WarnContext(context.Background(), "failed to send error reply")
		return errRespond
	}
	return nil
}
