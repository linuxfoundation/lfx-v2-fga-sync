// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// The fga-sync service.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/types"
)

// readTuplesTimeout is the maximum time allowed for the OpenFGA read call.
const readTuplesTimeout = 10 * time.Second

// readTuplesHandler handles requests to read a user's direct OpenFGA tuples for
// a given object type. It responds with a JSON-encoded ReadTuplesResponse
// containing tuple-strings in object#relation@user format.
func (h *HandlerService) readTuplesHandler(message INatsMsg) error {
	ctx, cancel := context.WithTimeout(context.Background(), readTuplesTimeout)
	defer cancel()

	// Unmarshal the JSON request payload.
	var req types.ReadTuplesRequest
	if err := json.Unmarshal(message.Data(), &req); err != nil {
		logger.With(errKey, err).WarnContext(ctx, "failed to unmarshal read tuples request")
		return h.respondReadTuplesError(ctx, message, "invalid request payload")
	}

	if req.User == "" || req.ObjectType == "" {
		logger.WarnContext(ctx, "read tuples request missing required fields", "user", req.User, "object_type", req.ObjectType)
		return h.respondReadTuplesError(ctx, message, "user and object_type are required")
	}

	// Validate that object_type is a clean type name (no colons).
	if strings.Contains(req.ObjectType, ":") {
		logger.WarnContext(ctx, "read tuples request contains invalid object_type", "object_type", req.ObjectType)
		return h.respondReadTuplesError(ctx, message, "object_type must not contain ':'")
	}

	logger.With("user", req.User, "object_type", req.ObjectType).InfoContext(ctx, "handling read tuples request")

	// Query OpenFGA for all direct tuples matching the user + object type.
	tuples, err := h.fgaService.ReadUserTuples(ctx, req.User, req.ObjectType)
	if err != nil {
		logger.With(errKey, err, "user", req.User, "object_type", req.ObjectType).ErrorContext(ctx, "failed to read user tuples")
		return h.respondReadTuplesError(ctx, message, "failed to read tuples")
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
		return h.respondReadTuplesError(ctx, message, "failed to marshal response")
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

// respondReadTuplesError sends a JSON error response over NATS and returns a
// formatted error so the subscription loop can log it consistently with other
// handlers.
func (h *HandlerService) respondReadTuplesError(ctx context.Context, message INatsMsg, errMsg string) error {
	if message.Reply() != "" {
		resp := types.ReadTuplesResponse{Error: errMsg}
		data, err := json.Marshal(resp)
		if err != nil {
			logger.With(errKey, err).ErrorContext(ctx, "failed to marshal error response")
			return err
		}
		if errRespond := message.Respond(data); errRespond != nil {
			logger.With(errKey, errRespond).WarnContext(ctx, "failed to send error reply")
			return errRespond
		}
	}
	return fmt.Errorf("read tuples: %s", errMsg)
}
