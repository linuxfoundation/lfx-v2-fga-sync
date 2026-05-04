// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package main provides the fga-sync service entry point and supporting types.
package main

import (
	"context"
)

// accessCheckHandler handles access check requests from the NATS server.
func (h *HandlerService) accessCheckHandler(message INatsMsg) error {
	ctx := context.Background()

	var response []byte
	var err error

	logger.With("message", string(message.Data())).InfoContext(ctx, "handling access check request")

	// Extract the check requests from the message payload.
	checkRequests, err := h.fgaService.ExtractCheckRequests(message.Data())
	if err != nil {
		errText := "failed to extract check requests"
		logger.With(errKey, err).WarnContext(ctx, errText)
		if message.Reply() != "" {
			// Send a reply if an inbox was provided.
			if errRespond := message.Respond([]byte(errText)); errRespond != nil {
				logger.With(errKey, errRespond).WarnContext(ctx, "failed to send reply")
				return errRespond
			}
		}
		return err
	}

	if len(checkRequests) == 0 {
		errText := "no check requests found"
		logger.WarnContext(ctx, errText)
		if message.Reply() != "" {
			// Send a reply if an inbox was provided.
			if errRespond := message.Respond([]byte(errText)); errRespond != nil {
				logger.With(errKey, errRespond).WarnContext(ctx, "failed to send reply")
				return errRespond
			}
		}
		// The message containing no check requests is not an error.
		return nil
	}

	logger.With("count", len(checkRequests)).DebugContext(ctx, "checking fga relationships")
	response, err = h.fgaService.CheckRelationships(ctx, checkRequests)
	if err != nil {
		errText := "failed to check relationship"
		logger.With(errKey, err).ErrorContext(ctx, errText)
		if message.Reply() != "" {
			// Send a reply if an inbox was provided.
			if errRespond := message.Respond([]byte(errText)); errRespond != nil {
				logger.With(errKey, errRespond).WarnContext(ctx, "failed to send reply")
				return errRespond
			}
		}
		return err
	}

	if message.Reply() != "" {
		// Send a reply if an inbox was provided.
		if errRespond := message.Respond(response); errRespond != nil {
			logger.With(errKey, errRespond).WarnContext(ctx, "failed to send reply")
			return errRespond
		}

		logger.With(
			"message", string(message.Data()),
			"response", string(response),
		).InfoContext(ctx, "sent access check response")
	}

	return nil
}
