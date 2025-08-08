// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"

	nats "github.com/nats-io/nats.go"
)

// HandlerService is the service that handles the messages from NATS about FGA syncing.
type HandlerService struct {
	fgaService FgaService
}

// INatsMsg is an interface for [nats.Msg] that allows for mocking.
type INatsMsg interface {
	Reply() string
	Respond(data []byte) error
	Data() []byte
	Subject() string
}

// NatsMsg is a wrapper around [nats.Msg] that implements [INatsMsg].
type NatsMsg struct {
	*nats.Msg
}

// Reply implements [INatsMsg.Reply].
func (m *NatsMsg) Reply() string {
	return m.Msg.Reply
}

// Respond implements [INatsMsg.Respond].
func (m *NatsMsg) Respond(data []byte) error {
	return m.Msg.Respond(data)
}

// Data implements [INatsMsg.Data].
func (m *NatsMsg) Data() []byte {
	return m.Msg.Data
}

// Subject implements [INatsMsg.Subject].
func (m *NatsMsg) Subject() string {
	return m.Msg.Subject
}

// processDeleteAllAccessMessage handles the common logic for deleting all access tuples for an object
func (h *HandlerService) processDeleteAllAccessMessage(
	message INatsMsg,
	objectTypePrefix,
	objectTypeName string,
) error {
	ctx := context.Background()

	logger.InfoContext(
		ctx,
		"handling "+objectTypeName+" access control delete all",
		"message", string(message.Data()),
	)

	objectUID := string(message.Data())
	if objectUID == "" {
		logger.ErrorContext(ctx, "empty deletion payload")
		return errors.New("empty deletion payload")
	}
	if objectUID[0] == '{' || objectUID[0] == '[' || objectUID[0] == '"' {
		// This event payload is not supposed to be serialized.
		logger.ErrorContext(ctx, "unsupported deletion payload")
		return errors.New("unsupported deletion payload")
	}

	object := objectTypePrefix + objectUID

	// Since this is a delete, we can call SyncObjectTuples directly
	// with a zero-value (nil) slice.
	tuplesWrites, tuplesDeletes, err := h.fgaService.SyncObjectTuples(ctx, object, nil)
	if err != nil {
		logger.With(errKey, err, "object", object).ErrorContext(ctx, "failed to sync tuples")
		return err
	}

	logger.InfoContext(
		ctx,
		"synced tuples",
		"object", object,
		"writes", tuplesWrites,
		"deletes", tuplesDeletes,
	)

	if message.Reply() != "" {
		// Send a reply if an inbox was provided.
		if err = message.Respond([]byte("OK")); err != nil {
			logger.With(errKey, err).WarnContext(ctx, "failed to send reply")
			return err
		}

		logger.With("object", object).InfoContext(ctx, "sent "+objectTypeName+" access control delete all response")
	}

	return nil
}
