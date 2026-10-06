// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package main provides the fga-sync service entry point and supporting types.
package main

import (
	"context"
	"time"
)

// accessCheckHandlerTimeout bounds the total time accessCheckHandler may
// spend, covering the full cache-lookup-then-BatchCheck flow, not just one
// outbound call. Without it, a slow or hung OpenFGA call can hold one of
// this service's limited concurrent-handler slots open indefinitely, long
// after the calling service (query-service) has given up: its
// AccessCheckTimeout defaults to 15s, so this must stay below that. See
// fgaHTTPTimeout and batchCheckMaxParallelRequests in fga.go for the rest of
// this timeout budget's rationale.
const accessCheckHandlerTimeout = 10 * time.Second

// accessCheckHandler handles access check requests from the NATS server.
func (h *HandlerService) accessCheckHandler(ctx context.Context, message INatsMsg) error {
	// Bound the total time this handler may run (see accessCheckHandlerTimeout
	// above) so a slow or hung downstream call can't hold this handler's
	// concurrency slot open indefinitely.
	ctx, cancel := context.WithTimeout(ctx, accessCheckHandlerTimeout)
	defer cancel()

	var response []byte
	var err error

	logger.With("message", string(message.Data())).InfoContext(ctx, "handling access check request")

	// Extract the check requests from the message payload.
	checkRequests, err := h.fgaService.ExtractCheckRequests(message.Data())
	if err != nil {
		errText := "failed to extract check requests"
		logger.With(errKey, err).WarnContext(ctx, errText)
		if replyErr := h.reply(ctx, message, []byte(errText)); replyErr != nil {
			return replyErr
		}
		return err
	}

	if len(checkRequests) == 0 {
		errText := "no check requests found"
		logger.WarnContext(ctx, errText)
		if replyErr := h.reply(ctx, message, []byte(errText)); replyErr != nil {
			return replyErr
		}
		// The message containing no check requests is not an error.
		return nil
	}

	logger.With("count", len(checkRequests)).DebugContext(ctx, "checking fga relationships")
	response, err = h.fgaService.CheckRelationships(ctx, checkRequests)
	if err != nil {
		errText := "failed to check relationship"
		logger.With(errKey, err).ErrorContext(ctx, errText)
		if replyErr := h.reply(ctx, message, []byte(errText)); replyErr != nil {
			return replyErr
		}
		return err
	}

	if message.Reply() != "" {
		err = h.reply(ctx, message, response)
		if err != nil {
			return err
		}
		logger.With(
			"message", string(message.Data()),
			"response", string(response),
		).InfoContext(ctx, "sent access check response")
	}

	return nil
}
