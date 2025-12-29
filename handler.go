// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
	nats "github.com/nats-io/nats.go"
)

// HandlerService is the service that handles the messages from NATS about FGA syncing.
type HandlerService struct {
	fgaService FgaService
}

// standardAccessStub represents the default structure for access control objects
type standardAccessStub struct {
	UID        string              `json:"uid"`
	ObjectType string              `json:"object_type"`
	Public     bool                `json:"public"`
	Relations  map[string][]string `json:"relations"`
	References map[string][]string `json:"references"`
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

// processStandardAccessUpdate handles the default access control update logic
func (h *HandlerService) processStandardAccessUpdate(message INatsMsg, obj *standardAccessStub) error {
	ctx := context.Background()

	logger.With("message", string(message.Data())).InfoContext(ctx, "handling "+obj.ObjectType+" access control update")

	if obj.UID == "" {
		logger.ErrorContext(ctx, obj.ObjectType+" ID not found")
		return errors.New(obj.ObjectType + " ID not found")
	}

	object := fmt.Sprintf("%s:%s", obj.ObjectType, obj.UID)

	// Build a list of tuples to sync.
	tuples := h.fgaService.NewTupleKeySlice(4)

	// Convert the "public" attribute to a "user:*" relation.
	if obj.Public {
		tuples = append(tuples, h.fgaService.TupleKey(constants.UserWildcard, constants.RelationViewer, object))
	}

	// for parent relation, project relation, etc
	for reference, valueList := range obj.References {
		refType := reference
		// When the reference is parent, use the object type itself as the reference type.
		// i.e. if the object type is committee, the parent relation should be committee:<parent_id>.
		if reference == constants.RelationParent {
			refType = obj.ObjectType
		}
		for _, value := range valueList {
			key := fmt.Sprintf("%s:%s", refType, value)
			tuples = append(tuples, h.fgaService.TupleKey(key, reference, object))
		}
	}

	// Add each principal from the object as the corresponding relationship tuple
	// (as defined in the OpenFGA schema).
	// for writer, auditor etc
	for relation, principals := range obj.Relations {
		for _, principal := range principals {
			tuples = append(tuples, h.fgaService.TupleKey(constants.ObjectTypeUser+principal, relation, object))
		}
	}

	tuplesWrites, tuplesDeletes, err := h.fgaService.SyncObjectTuples(ctx, object, tuples)
	if err != nil {
		logger.With(errKey, err, "tuples", tuples, "object", object).ErrorContext(ctx, "failed to sync tuples")
		return err
	}

	logger.With(
		"tuples", tuples,
		"object", object,
		"writes", tuplesWrites,
		"deletes", tuplesDeletes,
	).InfoContext(ctx, "synced tuples")

	if message.Reply() != "" {
		// Send a reply if an inbox was provided.
		if err = message.Respond([]byte("OK")); err != nil {
			logger.With(errKey, err).WarnContext(ctx, "failed to send reply")
			return err
		}

		logger.With("object", object).InfoContext(ctx, "sent "+obj.ObjectType+" access control update response")
	}

	return nil
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
